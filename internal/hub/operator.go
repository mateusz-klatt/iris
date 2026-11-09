package hub

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/mateusz-klatt/iris/internal/store"
)

type loginGrant struct{ expires time.Time }

// SetUI supplies the embedded static handler. The frontend package owns its files.
func (h *Hub) SetUI(handler http.Handler) { h.ui = handler }
func secret() string {
	var b [32]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never returns an error
	return base64.RawURLEncoding.EncodeToString(b[:])
}
func loopback(r *http.Request) bool {
	remote, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	host, _, err := net.SplitHostPort(r.Host)
	if err != nil {
		host = r.Host
	}
	return net.ParseIP(remote).IsLoopback() && net.ParseIP(host).IsLoopback()
}

// SetPublicURL opts into a trusted loopback TLS reverse proxy. Configure before serving.
func (h *Hub) SetPublicURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return errors.New("public URL must be an https origin without credentials, query or fragment")
	}
	// Browsers send a lower-case Host without the default port, and login compares
	// it exactly.
	u.Host = strings.TrimSuffix(strings.ToLower(u.Host), ":443")
	h.publicURL = u
	h.hosts[u.Host] = true
	return nil
}

func (h *Hub) loginAllowed(r *http.Request) bool {
	if h.publicURL == nil {
		return loopback(r)
	}
	remote, _, err := net.SplitHostPort(r.RemoteAddr)
	return err == nil && net.ParseIP(remote).IsLoopback() && r.Host == h.publicURL.Host && r.Header.Get("X-Forwarded-Proto") == "https"
}
func (h *Hub) cookieAuthorized(r *http.Request) bool {
	if !h.loginAllowed(r) {
		return false
	}
	c, err := r.Cookie("iris_session")
	if err != nil {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	g, ok := h.cookies[c.Value]
	return ok && g.expires.After(h.now())
}
func (h *Hub) uiLink(w http.ResponseWriter, r *http.Request) {
	if !h.loginAllowed(r) {
		h.reject(w, r, "UI login origin is not trusted")
		return
	}
	code := secret()
	h.mu.Lock()
	for k, g := range h.links {
		if !g.expires.After(h.now()) {
			delete(h.links, k)
		}
	}
	h.links[code] = loginGrant{h.now().Add(time.Minute)}
	h.mu.Unlock()
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	base := scheme + "://" + r.Host
	if h.publicURL != nil {
		base = "https://" + h.publicURL.Host
	}
	writeJSON(w, map[string]any{"url": base + "/ui/login?code=" + code})
}
func (h *Hub) uiLogin(w http.ResponseWriter, r *http.Request) {
	if !h.loginAllowed(r) {
		h.reject(w, r, "UI cookie origin is not trusted")
		return
	}
	code := r.URL.Query().Get("code")
	h.mu.Lock()
	grant, ok := h.links[code]
	delete(h.links, code)
	if !ok || !grant.expires.After(h.now()) {
		h.mu.Unlock()
		h.reject(w, r, "login link expired or used")
		return
	}
	key := secret()
	for k, g := range h.cookies {
		if !g.expires.After(h.now()) {
			delete(h.cookies, k)
		}
	}
	h.cookies[key] = loginGrant{h.now().Add(12 * time.Hour)}
	h.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: "iris_session", Value: key, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil || h.publicURL != nil, MaxAge: 43200})
	w.Header().Set("Referrer-Policy", "no-referrer")
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
func (h *Hub) uiWatch(w http.ResponseWriter, r *http.Request) {
	p := requestPrincipal(r)
	if !h.reserveSocket(w, p) {
		return
	}
	defer h.releaseSocket(p)
	conn, err := h.acceptSocket(w, r, nil)
	if err != nil {
		return
	}
	defer func() { _ = conn.CloseNow() }()
	if !h.trackSocket(r, conn, p) {
		return
	}
	defer h.untrackSocket(conn, p)
	ctx := conn.CloseRead(r.Context())
	wakes, release := h.subscribeStreams()
	defer release()
	pings := time.NewTicker(h.pingInterval())
	defer pings.Stop()
	var previous int64 = -1
	for {
		changes := wakes.Changes()
		result, err := h.call(ctx, "ui_events", store.Request{})
		if err != nil {
			return
		}
		cursor := result["cursor"].(int64)
		if cursor != previous {
			raw, _ := json.Marshal(result)
			if h.writeSocket(ctx, conn, raw) != nil {
				return
			}
			previous = cursor
		}
		waiting := true
		for waiting {
			select {
			case <-ctx.Done():
				return
			case <-changes:
				waiting = false
			case <-pings.C:
				if !h.ping(ctx, conn) {
					return
				}
			}
		}
	}
}
func (h *Hub) operatorRoutes() {
	for route, op := range map[string]string{"GET /v1/ui/overview": "ui_overview", "GET /v1/ui/deliveries": "ui_deliveries", "GET /v1/ui/threads": "ui_threads", "GET /v1/ui/threads/{id}": "ui_thread", "POST /v1/ui/threads/{id}/read": "ui_read", "POST /v1/ui/messages": "ui_messages", "POST /v1/ui/claims/{fence}/revoke": "ui_revoke", "GET /v1/ui/doctor": "ui_doctor_get", "PUT /v1/ui/doctor": "ui_doctor_put"} {
		h.mux.HandleFunc(route, func(w http.ResponseWriter, r *http.Request) {
			req, err := decode(w, r)
			if err != nil {
				writeError(w, err)
				return
			}
			q := r.URL.Query()
			req.Q = q.Get("q")
			req.Agent = q.Get("agent")
			req.Before = q.Get("before")
			if op == "ui_thread" || op == "ui_read" {
				req.Key = r.PathValue("id")
			}

			if op == "ui_revoke" {
				req.Fence, err = strconv.ParseInt(r.PathValue("fence"), 10, 64)
				if err != nil {
					writeError(w, &store.Error{Code: "bad_request", Message: "invalid fence"})
					return
				}
			}
			out, err := h.call(r.Context(), op, req)
			if err != nil {
				writeError(w, err)
				return
			}
			writeJSON(w, out)
		})
	}
	h.mux.HandleFunc("POST /v1/ui/links", h.uiLink)
	h.mux.HandleFunc("GET /ui/login", h.uiLogin)
	h.mux.HandleFunc("GET /v1/ui/watch", h.uiWatch)
}
