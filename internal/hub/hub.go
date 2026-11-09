// Package hub exposes the version-one HTTP and content-free WebSocket API.
package hub

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log"
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
	principal  principal
	execution  string
	generation int64
	expires    time.Time
	used       bool
}
type Hub struct {
	console *consoleAuth
	// These seams are the database and WebSocket I/O boundaries; production uses
	// the real store and transport, and tests can report their actual failure modes.
	call         func(context.Context, string, store.Request) (map[string]any, error)
	acceptSocket func(http.ResponseWriter, *http.Request, *websocket.AcceptOptions) (*websocket.Conn, error)
	writeSocket  func(context.Context, *websocket.Conn, []byte) error
	pingSocket   func(context.Context, *websocket.Conn) error
	Limits       Limits
	rates        map[string]window
	socketCounts map[string]int
	sockets      map[string]map[*websocket.Conn]bool
	// PingInterval is configurable before serving; zero uses 25 seconds.
	PingInterval time.Duration
	// MaxRequestBytes bounds every HTTP body; zero uses 64 KiB.
	MaxRequestBytes int64
	// AccessLog logs every authenticated request. Off, only a token's first use since
	// start is logged; authentication failures are logged either way.
	AccessLog bool
	// Log receives the hub's log lines; nil uses the standard logger.
	Log       *log.Logger
	usedMu    sync.Mutex
	used      map[string]bool
	publicURL *url.URL
	monitors  map[string]bool
	ui        http.Handler
	links     map[string]loginGrant
	cookies   map[string]loginGrant
	store     *store.Store
	token     string
	hosts     map[string]bool
	mux       *http.ServeMux
	mu        sync.Mutex
	tickets   map[string]ticket
	now       func() time.Time
}

func New(s *store.Store, token string, allowedHosts []string) *Hub {
	h := &Hub{call: s.Call, acceptSocket: websocket.Accept,
		writeSocket: func(ctx context.Context, c *websocket.Conn, raw []byte) error {
			return c.Write(ctx, websocket.MessageText, raw)
		},
		pingSocket: func(ctx context.Context, c *websocket.Conn) error { return c.Ping(ctx) }, rates: map[string]window{}, socketCounts: map[string]int{}, sockets: map[string]map[*websocket.Conn]bool{}, monitors: map[string]bool{}, links: map[string]loginGrant{}, cookies: map[string]loginGrant{}, store: s, token: token, hosts: map[string]bool{}, mux: http.NewServeMux(), tickets: map[string]ticket{}, used: map[string]bool{}, now: time.Now}
	for _, host := range allowedHosts {
		h.hosts[strings.ToLower(host)] = true
	}
	for route, operation := range map[string]string{
		"POST /v1/shell/reconcile": "reconcile",
		"POST /v1/guard/check":     "check",
		"POST /v1/sessions":        "sessions", "POST /v1/bind": "bind",
		"POST /v1/executions/{id}/heartbeat": "heartbeat", "POST /v1/executions/{id}/end": "end",
		"POST /v1/claims/touch": "touch", "POST /v1/claims": "claim", "POST /v1/claims/release": "release", "GET /v1/claims": "claims",
		"POST /v1/messages": "messages", "GET /v1/inbox": "inbox", "POST /v1/inbox/receipt": "inbox_receipt", "POST /v1/deliveries/{id}/ack": "ack",
		"GET /v1/messages/{id}/status": "receipts",
		"GET /v1/messages/history":     "history", "GET /v1/threads/{id}": "thread",
		"GET /v1/messages/{id}/body": "read_message_body",
		"GET /v1/agents":             "agents", "GET /v1/status": "status", "GET /v1/events": "events",
		"GET /v1/hooks/seen":           "hooks_seen",
		"GET /v1/deliveries/{id}/body": "read_body",
	} {
		h.mux.HandleFunc(route, func(w http.ResponseWriter, r *http.Request) { h.api(w, r, operation) })
	}
	h.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if h.ui != nil && !strings.HasPrefix(r.URL.Path, "/v1/") {
			h.ui.ServeHTTP(w, r)
			return
		}
		writeError(w, &store.Error{Code: "not_found", Message: "unknown API route"})
	})
	h.operatorRoutes()
	h.tokenRoutes()
	h.ownerAccountsRoutes()
	h.mux.HandleFunc("GET /v1/watch", h.watch)
	h.mux.HandleFunc("POST /v1/wake-tickets", h.wakeTicket)
	return h
}
func (h *Hub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if !h.hosts[strings.ToLower(r.Host)] {
		h.reject(w, r, "Host is not allowed")
		return
	}
	p := h.authenticate(r)
	h.logPrincipal(r, p)
	if p.name == "" {
		if !h.anonymousRate(w) {
			return
		}
	} else if !h.tokenRate(w, p) {
		return
	}
	r = r.WithContext(context.WithValue(r.Context(), principalKey{}, p))
	wsOrigin := "ws://" + r.Host
	if r.TLS != nil {
		wsOrigin = "wss://" + r.Host
	}
	if h.publicURL != nil {
		wsOrigin = "wss://" + h.publicURL.Host
	}
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; font-src 'self'; connect-src 'self' "+wsOrigin+"; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
	limit := h.MaxRequestBytes
	if limit <= 0 {
		limit = 64 << 10
	}
	if r.ContentLength > limit {
		writeError(w, &store.Error{Code: "too_large", Message: "request body exceeds limit"})
		return
	}
	if r.Body != nil {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
		if err != nil {
			var large *http.MaxBytesError
			code := "bad_request"
			if errors.As(err, &large) {
				code = "too_large"
			}
			writeError(w, &store.Error{Code: code, Message: "invalid or oversized request body"})
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		u, err := url.Parse(origin)
		scheme := "http"
		if r.TLS != nil || (h.publicURL != nil && r.Host == h.publicURL.Host) {
			scheme = "https"
		}
		if err != nil || u.Scheme != scheme || !strings.EqualFold(u.Host, r.Host) || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
			h.reject(w, r, "Origin is not allowed")
			return
		}
	}
	if h.serveConsole(w, r) {
		return
	}
	if strings.HasPrefix(r.URL.Path, "/v1/") && r.URL.Path != "/v1/status" && r.URL.Path != "/v1/watch" && !h.authorized(r) && (!strings.HasPrefix(r.URL.Path, "/v1/ui/") || r.URL.Path == "/v1/ui/links" || !h.cookieAuthorized(r)) {
		h.reject(w, r, h.refusal(r))
		return
	}
	h.mux.ServeHTTP(w, r)
}

// refusal says why a request has no principal. A revoked or unknown token is told apart
// from a missing one, so the log shows a host still sending a token it should not.
func (h *Hub) refusal(r *http.Request) string {
	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(header, "Bearer ") {
		return "bearer token required"
	}
	if name, revoked := h.store.RevokedTokenName(r.Context(), strings.TrimPrefix(header, "Bearer ")); revoked {
		return "revoked token " + strconv.Quote(name)
	}
	return "unknown bearer token"
}
func (h *Hub) authorized(r *http.Request) bool {
	return requestPrincipal(r).name != ""
}
func decode(w http.ResponseWriter, r *http.Request) (store.Request, error) {
	var req store.Request
	if r.Method == http.MethodGet {
		q := r.URL.Query()
		req.ExecutionID = q.Get("execution_id")
		req.RepositoryID = q.Get("repository_id")
		req.Stream = q.Get("stream")
		req.ConsumerID = q.Get("consumer_id")
		req.All = q.Get("all") == "1"
		req.Receipt = q.Get("receipt") == "1" // ADR 010: keep attention until the receipt
		req.Host = q.Get("host")
		req.Q = q.Get("q")
		req.Before = q.Get("before")
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
	dec := json.NewDecoder(r.Body)
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
	if operation == "status" && !h.authorized(r) {
		if r.Header.Get("Authorization") != "" {
			h.logger().Printf("authentication failed: peer=%q reason=invalid status token", r.RemoteAddr)
		}
		writeJSON(w, map[string]string{"status": "ok"})
		return
	}
	req, err := decode(w, r)
	if err != nil {
		writeError(w, err)
		return
	}
	if operation == "heartbeat" || operation == "end" {
		req.ExecutionID = r.PathValue("id")
	}
	if operation == "ack" || operation == "receipts" || operation == "read_body" || operation == "read_message_body" || operation == "thread" {
		req.Key = r.PathValue("id")
	}
	ctx := r.Context()
	if operation == "messages" {
		// Sends to other repositories are counted per credential (ADR 004).
		ctx = store.WithPrincipal(ctx, requestPrincipal(r).digest)
	}
	out, err := h.call(ctx, operation, req)
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
	case "forbidden":
		status = http.StatusForbidden
	case "not_found":
		status = http.StatusNotFound
	case "conflict", "key_reused", "lost", "ambiguous", "unbound":
		status = http.StatusConflict
	case "rate_limited":
		status = http.StatusTooManyRequests
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
	if _, err = h.call(r.Context(), "validate", req); err != nil {
		writeError(w, err)
		return
	}
	var raw [32]byte
	_, _ = rand.Read(raw[:]) // crypto/rand.Read never returns an error
	key := base64.RawURLEncoding.EncodeToString(raw[:])
	expires := h.now().Add(2 * time.Minute)
	h.mu.Lock()
	for k, t := range h.tickets {
		// A used ticket tracks its socket until disconnect or rotation. Its expiry
		// limits the handshake only, so another Execution's arming must retain it.
		if t.execution == req.ExecutionID || (!t.used && !t.expires.After(h.now())) {
			delete(h.tickets, k)
		}
	}
	h.tickets[key] = ticket{principal: requestPrincipal(r), execution: req.ExecutionID, generation: req.Generation, expires: expires}
	h.mu.Unlock()
	scheme := "ws"
	if r.TLS != nil {
		scheme = "wss"
	}
	host := r.Host
	if h.publicURL != nil {
		scheme, host = "wss", h.publicURL.Host
	}
	u := url.URL{Scheme: scheme, Host: host, Path: "/v1/watch"}
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
	req.Limit = boundedPageSize // paging is internal to the socket; never skip a partial page
	if err != nil {
		writeError(w, err)
		return
	}
	protocols := strings.Split(r.Header.Get("Sec-WebSocket-Protocol"), ",")
	for i := range protocols {
		protocols[i] = strings.TrimSpace(protocols[i])
	}
	protocol := "iris.v1"
	ticketKey := ""
	p := requestPrincipal(r)
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
			for _, offeredTicket := range protocols {
				if t, ok := h.tickets[offeredTicket]; ok && !t.used && t.execution == req.ExecutionID && t.expires.After(h.now()) {
					p = t.principal
					h.logPrincipal(r, p)
					req.Generation = t.generation
					t.used = true
					h.tickets[offeredTicket] = t
					ticketKey = offeredTicket
					valid = true
					break
				}
			}
		}
		h.mu.Unlock()
		if !valid {
			h.reject(w, r, "valid single-use wake ticket required")
			return
		}
		defer func() { h.mu.Lock(); delete(h.tickets, ticketKey); h.mu.Unlock() }()
	}
	if _, err = h.call(r.Context(), "validate", req); err != nil {
		writeError(w, err)
		return
	}
	waiting := false
	if req.ConsumerID == "iris-watch" {
		h.mu.Lock()
		busy := h.monitors[req.ExecutionID]
		if !busy {
			h.monitors[req.ExecutionID] = true
		}
		h.mu.Unlock()
		if busy {
			writeError(w, &store.Error{Code: "conflict", Message: "Execution already has a monitor"})
			return
		}
		defer func() { h.mu.Lock(); delete(h.monitors, req.ExecutionID); h.mu.Unlock() }()
		saved, err := h.call(r.Context(), "monitor_cursor", req)
		if err != nil {
			writeError(w, err)
			return
		}
		if cursor := saved["cursor"].(int64); req.After == 0 {
			req.After = cursor
			waiting = saved["pending"].(int) > 0
		}
	}
	if ticketKey != "" && !h.tokenRate(w, p) {
		return
	}
	if !h.reserveSocket(w, p) {
		return
	}
	defer h.releaseSocket(p)
	conn, err := h.acceptSocket(w, r, &websocket.AcceptOptions{Subprotocols: []string{protocol}})
	if err != nil {
		return
	}
	defer func() { _ = conn.CloseNow() }()
	if !h.trackSocket(r, conn, p) {
		return
	}
	defer h.untrackSocket(conn, p)
	ctx := conn.CloseRead(r.Context())
	pings := time.NewTicker(h.pingInterval())
	defer pings.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		changes := h.store.Changes()
		if ticketKey != "" {
			h.mu.Lock()
			_, exists := h.tickets[ticketKey]
			h.mu.Unlock()
			if !exists {
				_ = conn.Close(websocket.StatusPolicyViolation, "wake ticket rotated or revoked")
				return
			}
		}
		if _, err = h.call(ctx, "validate", req); err != nil {
			_ = conn.Close(websocket.StatusPolicyViolation, "Execution lease lost")
			return
		}
		pageReq := req
		if pageReq.ConsumerID == "iris-watch" {
			pageReq.ConsumerID = ""
		}
		result, err := h.call(ctx, "events", pageReq)
		if err != nil {
			_ = conn.Close(websocket.StatusPolicyViolation, "Execution stream unavailable")
			return
		}
		high := result["high_water"].(int64)
		send := func(cursor int64, kind string) error {
			frame, _ := json.Marshal(map[string]any{"stream": result["stream"], "cursor": cursor, "kind": kind})
			return h.writeSocket(ctx, conn, frame)
		}
		if waiting {
			// A new monitor skips history, so it fetches what is waiting. Frames never
			// say how much, only that something changed.
			if send(req.After, "reset") != nil {
				return
			}
			waiting = false
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
				kind := event["kind"].(string)
				if event["settled"] == true {
					// The cursor must still advance, but this mail has already been seen.
					kind = "inbox_read"
				}
				if send(cursor, kind) != nil {
					return
				}
				req.After = cursor
			}
			if len(events) < boundedPageSize {
				req.After = high
			}

		}
		select {
		case <-ctx.Done():
			return
		case <-changes:
		case <-ticker.C:
		case <-pings.C:
			if !h.ping(ctx, conn) {
				return
			}
		}
	}
}

const boundedPageSize = 100

func (h *Hub) pingInterval() time.Duration {
	if h.PingInterval > 0 {
		return h.PingInterval
	}
	return 25 * time.Second
}

func (h *Hub) ping(ctx context.Context, conn *websocket.Conn) bool {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if h.pingSocket(ctx, conn) != nil {
		_ = conn.CloseNow()
		return false
	}
	return true
}

func (h *Hub) logger() *log.Logger {
	if h.Log != nil {
		return h.Log
	}
	return log.Default()
}

func (h *Hub) reject(w http.ResponseWriter, r *http.Request, reason string) {
	h.logger().Printf("authentication failed: peer=%q reason=%q", r.RemoteAddr, reason)
	writeError(w, &store.Error{Code: "unauthorized", Message: reason})
}
