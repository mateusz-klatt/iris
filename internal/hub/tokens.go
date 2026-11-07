package hub

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"log"
	"net/http"
	"strings"

	"github.com/coder/websocket"
	"github.com/mateusz-klatt/iris/internal/store"
)

type principal struct{ name, digest string }

// ownerName is the principal for the hub's own token file, the owner credential. Logs
// have always called it this, so the name stays.
const ownerName = "(legacy)"

// ownerOnly refuses every principal but the owner. A host token that could manage
// tokens could mint itself more or lock other hosts out, which is what per-host
// tokens exist to prevent.
func (h *Hub) ownerOnly(w http.ResponseWriter, r *http.Request) bool {
	if requestPrincipal(r).name == ownerName {
		return true
	}
	writeError(w, &store.Error{Code: "forbidden", Message: "only the hub's own token may do this"})
	return false
}

type principalKey struct{}

func tokenDigest(token string) string {
	sum := sha256.Sum256([]byte(token))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
func (h *Hub) authenticate(r *http.Request) principal {
	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(header, "Bearer ") {
		return principal{}
	}
	token := strings.TrimPrefix(header, "Bearer ")
	got, want := sha256.Sum256([]byte(token)), sha256.Sum256([]byte(h.token))
	if h.token != "" && subtle.ConstantTimeCompare(got[:], want[:]) == 1 {
		return principal{ownerName, tokenDigest(token)}
	}
	name, err := h.store.AuthenticateToken(r.Context(), token)
	if err != nil {
		return principal{}
	}
	return principal{name, tokenDigest(token)}
}
func (h *Hub) tokenRoutes() {
	h.mux.HandleFunc("POST /v1/admin/forget", func(w http.ResponseWriter, r *http.Request) {
		if !h.ownerOnly(w, r) {
			return
		}
		req, err := decode(w, r)
		if err != nil {
			writeError(w, err)
			return
		}
		out, err := h.call(r.Context(), "forget", req)
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, out)
	})
	h.mux.HandleFunc("GET /v1/tokens", func(w http.ResponseWriter, r *http.Request) {
		if !h.ownerOnly(w, r) {
			return
		}
		tokens, err := h.store.ListTokens(r.Context())
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, map[string]any{"tokens": tokens})
	})
	h.mux.HandleFunc("POST /v1/tokens", func(w http.ResponseWriter, r *http.Request) {
		if !h.ownerOnly(w, r) {
			return
		}
		req, err := decode(w, r)
		if err != nil {
			writeError(w, err)
			return
		}
		token, err := h.store.AddToken(r.Context(), req.Host)
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, map[string]any{"name": req.Host, "token": token})
	})
	h.mux.HandleFunc("DELETE /v1/tokens/{name}", func(w http.ResponseWriter, r *http.Request) {
		if !h.ownerOnly(w, r) {
			return
		}
		h.mu.Lock()
		digest, err := h.store.RevokeToken(r.Context(), r.PathValue("name"))
		if err == nil {
			for conn := range h.sockets[digest] {
				go conn.Close(websocket.StatusPolicyViolation, "bearer token revoked")
			}
			for key, t := range h.tickets {
				if t.principal.digest == digest {
					delete(h.tickets, key)
				}
			}
		}
		h.mu.Unlock()
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, map[string]any{"revoked": r.PathValue("name")})
	})
}

// Registration and revocation share a lock; checking the original digest also
// rejects a replaced token if its former name has already been reused.
func (h *Hub) trackSocket(r *http.Request, conn *websocket.Conn, p principal) bool {
	h.mu.Lock()
	if p.name != "" && p.name != ownerName && !h.store.TokenActive(r.Context(), p.digest) {
		h.mu.Unlock()
		_ = conn.Close(websocket.StatusPolicyViolation, "bearer token revoked")
		return false
	}
	if h.sockets[p.digest] == nil {
		h.sockets[p.digest] = map[*websocket.Conn]bool{}
	}
	h.sockets[p.digest][conn] = true
	h.mu.Unlock()
	return true
}
func (h *Hub) untrackSocket(conn *websocket.Conn, p principal) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.sockets[p.digest], conn)
	if len(h.sockets[p.digest]) == 0 {
		delete(h.sockets, p.digest)
	}
}
func requestPrincipal(r *http.Request) principal {
	p, _ := r.Context().Value(principalKey{}).(principal)
	return p
}

// logPrincipal records who used the hub. A line per request filled a launchd or
// container log within days, so by default each token is named once per start.
// It has its own lock because one caller already holds h.mu.
func (h *Hub) logPrincipal(r *http.Request, p principal) {
	if p.name == "" {
		return
	}
	if h.AccessLog {
		log.Printf("authenticated request: token=%q method=%q path=%q peer=%q", p.name, r.Method, r.URL.Path, r.RemoteAddr)
		return
	}
	h.usedMu.Lock()
	first := !h.used[p.name]
	h.used[p.name] = true
	h.usedMu.Unlock()
	if first {
		log.Printf("token first used since start: token=%q peer=%q", p.name, r.RemoteAddr)
	}
}
