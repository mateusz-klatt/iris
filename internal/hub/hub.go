// Package hub exposes the version-one HTTP and content-free WebSocket API.
package hub

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/mateusz-klatt/iris/internal/store"
)

type ticket struct {
	execution  string
	generation int64
	expires    time.Time
}
type Hub struct {
	store   *store.Store
	token   string
	hosts   map[string]bool
	mux     *http.ServeMux
	mu      sync.Mutex
	tickets map[string]ticket
	now     func() time.Time
}

func New(s *store.Store, token string, allowedHosts []string) *Hub {
	h := &Hub{store: s, token: token, hosts: map[string]bool{}, mux: http.NewServeMux(), tickets: map[string]ticket{}, now: time.Now}
	for _, host := range allowedHosts {
		h.hosts[strings.ToLower(host)] = true
	}
	for route, operation := range map[string]string{
		"POST /v1/sessions": "sessions", "POST /v1/bind": "bind",
		"POST /v1/executions/{id}/heartbeat": "heartbeat", "POST /v1/executions/{id}/end": "end",
		"POST /v1/claims/touch": "touch", "POST /v1/claims": "claim", "POST /v1/claims/release": "release", "GET /v1/claims": "claims",
		"POST /v1/messages": "messages", "GET /v1/inbox": "inbox", "POST /v1/deliveries/{id}/ack": "ack",
		"GET /v1/agents": "agents", "GET /v1/status": "status", "GET /v1/events": "events",
	} {
		h.mux.HandleFunc(route, func(w http.ResponseWriter, r *http.Request) { h.api(w, r, operation) })
	}
	h.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, &store.Error{Code: "not_found", Message: "unknown API route"})
	})
	h.mux.HandleFunc("GET /v1/watch", h.watch)
	h.mux.HandleFunc("POST /v1/wake-tickets", h.wakeTicket)
	return h
}
func (h *Hub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if !h.hosts[strings.ToLower(r.Host)] {
		writeError(w, &store.Error{Code: "unauthorized", Message: "Host is not allowed", Details: map[string]any{}})
		return
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		u, err := url.Parse(origin)
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		if err != nil || u.Scheme != scheme || !strings.EqualFold(u.Host, r.Host) || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
			writeError(w, &store.Error{Code: "unauthorized", Message: "Origin is not allowed", Details: map[string]any{}})
			return
		}
	}
	if r.URL.Path != "/v1/status" && r.URL.Path != "/v1/watch" && !h.authorized(r) {
		writeError(w, &store.Error{Code: "unauthorized", Message: "bearer token required", Details: map[string]any{}})
		return
	}
	h.mux.ServeHTTP(w, r)
}
func (h *Hub) authorized(r *http.Request) bool {
	value := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	return h.token != "" && r.Header.Get("Authorization") != "" && subtle.ConstantTimeCompare([]byte(value), []byte(h.token)) == 1 && strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ")
}
func decode(w http.ResponseWriter, r *http.Request) (store.Request, error) {
	var req store.Request
	if r.Method == http.MethodGet {
		q := r.URL.Query()
		req.ExecutionID = q.Get("execution_id")
		req.RepositoryID = q.Get("repository_id")
		req.Stream = q.Get("stream")
		req.ConsumerID = q.Get("consumer_id")
		var err error
		if q.Has("after") {
			req.After, err = strconv.ParseInt(q.Get("after"), 10, 64)
			if err != nil {
				return req, &store.Error{Code: "bad_request", Message: "invalid cursor"}
			}
		}
		if q.Has("limit") {
			req.Limit, err = strconv.Atoi(q.Get("limit"))
			if err != nil || req.Limit < 0 {
				return req, &store.Error{Code: "bad_request", Message: "invalid limit"}
			}
		}
		return req, nil
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	if err := dec.Decode(&req); err != nil {
		var large *http.MaxBytesError
		code := "bad_request"
		if errors.As(err, &large) {
			code = "too_large"
		}
		return req, &store.Error{Code: code, Message: "invalid or oversized JSON request"}
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return req, &store.Error{Code: "bad_request", Message: "one JSON object is required"}
	}
	return req, nil
}
func (h *Hub) api(w http.ResponseWriter, r *http.Request, operation string) {
	req, err := decode(w, r)
	if err != nil {
		writeError(w, err)
		return
	}
	if operation == "heartbeat" || operation == "end" {
		req.ExecutionID = r.PathValue("id")
	}
	if operation == "ack" {
		req.Key = r.PathValue("id")
	}
	out, err := h.store.Call(r.Context(), operation, req)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, out)
}
func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}
func writeError(w http.ResponseWriter, err error) {
	var domain *store.Error
	if !errors.As(err, &domain) {
		domain = &store.Error{Code: "bad_request", Message: "hub operation failed", Details: map[string]any{}}
	}
	status := http.StatusBadRequest
	switch domain.Code {
	case "unauthorized":
		status = http.StatusUnauthorized
	case "not_found":
		status = http.StatusNotFound
	case "conflict", "key_reused", "lost", "ambiguous", "unbound":
		status = http.StatusConflict
	case "too_large":
		status = http.StatusRequestEntityTooLarge
	}
	if domain.Details == nil {
		domain.Details = map[string]any{}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": domain})
}
func (h *Hub) wakeTicket(w http.ResponseWriter, r *http.Request) {
	req, err := decode(w, r)
	if err != nil {
		writeError(w, err)
		return
	}
	if req.Generation <= 0 {
		writeError(w, &store.Error{Code: "bad_request", Message: "generation required"})
		return
	}
	if _, err = h.store.Call(r.Context(), "validate", req); err != nil {
		writeError(w, err)
		return
	}
	var raw [32]byte
	if _, err = rand.Read(raw[:]); err != nil {
		writeError(w, err)
		return
	}
	key := base64.RawURLEncoding.EncodeToString(raw[:])
	expires := h.now().Add(2 * time.Minute)
	h.mu.Lock()
	for k, t := range h.tickets {
		if t.execution == req.ExecutionID || !t.expires.After(h.now()) {
			delete(h.tickets, k)
		}
	}
	h.tickets[key] = ticket{req.ExecutionID, req.Generation, expires}
	h.mu.Unlock()
	scheme := "ws"
	if r.TLS != nil {
		scheme = "wss"
	}
	u := url.URL{Scheme: scheme, Host: r.Host, Path: "/v1/watch"}
	q := u.Query()
	q.Set("execution_id", req.ExecutionID)
	u.RawQuery = q.Encode()
	writeJSON(w, map[string]any{"url": u.String(), "protocols": []string{"iris.wake", key}, "expires_at": expires.UTC().Format(time.RFC3339Nano)})
}
func offered(protocols []string, p string) bool {
	for _, x := range protocols {
		if x == p {
			return true
		}
	}
	return false
}
func (h *Hub) watch(w http.ResponseWriter, r *http.Request) {
	req, err := decode(w, r)
	if err != nil {
		writeError(w, err)
		return
	}
	protocols := strings.Split(r.Header.Get("Sec-WebSocket-Protocol"), ",")
	for i := range protocols {
		protocols[i] = strings.TrimSpace(protocols[i])
	}
	protocol := "iris.v1"
	if h.authorized(r) {
		if !offered(protocols, "iris.v1") {
			writeError(w, &store.Error{Code: "bad_request", Message: "iris.v1 subprotocol required"})
			return
		}
	} else {
		protocol = "iris.wake"
		valid := false
		h.mu.Lock()
		if offered(protocols, "iris.wake") {
			for _, p := range protocols {
				if t, ok := h.tickets[p]; ok && t.execution == req.ExecutionID && t.expires.After(h.now()) {
					req.Generation = t.generation
					delete(h.tickets, p)
					valid = true
					break
				}
			}
		}
		h.mu.Unlock()
		if !valid {
			writeError(w, &store.Error{Code: "unauthorized", Message: "valid single-use wake ticket required"})
			return
		}
	}
	if _, err = h.store.Call(r.Context(), "validate", req); err != nil {
		writeError(w, err)
		return
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{protocol}})
	if err != nil {
		return
	}
	defer conn.CloseNow()
	ctx := conn.CloseRead(r.Context())
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	initial := true
	for {
		changes := h.store.Changes()
		if _, err = h.store.Call(ctx, "validate", req); err != nil {
			_ = conn.Close(websocket.StatusPolicyViolation, "Execution lease lost")
			return
		}
		result, err := h.store.Call(ctx, "events", req)
		if err != nil {
			return
		}
		high := result["high_water"].(int64)
		send := func(cursor int64, kind string) error {
			frame, _ := json.Marshal(map[string]any{"stream": result["stream"], "cursor": cursor, "kind": kind})
			return conn.Write(ctx, websocket.MessageText, frame)
		}
		if result["reset"] == true {
			if send(high, "reset") != nil {
				return
			}
			req.After = high
			req.Stream = result["stream"].(string)
		} else {
			events := result["events"].([]map[string]any)
			for _, event := range events {
				cursor := event["cursor"].(int64)
				if send(cursor, event["kind"].(string)) != nil {
					return
				}
				req.After = cursor
			}
			if len(events) < boundedPageSize {
				req.After = high
			}
			if initial && len(events) == 0 {
				snapshot, err := h.store.Call(ctx, "attention", req)
				if err != nil {
					return
				}
				if snapshot["unread"].(int) > 0 {
					if send(high, "inbox_changed") != nil {
						return
					}
				}
			}
		}
		initial = false
		select {
		case <-ctx.Done():
			return
		case <-changes:
		case <-ticker.C:
		}
	}
}

const boundedPageSize = 100
