package hub

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
)

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

// Host diagnostics remain a machine-authenticated upload. Browser reads use the
// account namespace and its scoped principal, never an owner or host bearer.
func (h *Hub) operatorRoutes() {
	h.mux.HandleFunc("PUT /v1/ui/doctor", func(w http.ResponseWriter, r *http.Request) {
		req, err := decode(w, r)
		if err != nil {
			writeError(w, err)
			return
		}
		out, err := h.call(r.Context(), "ui_doctor_put", req)
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, out)
	})
}
