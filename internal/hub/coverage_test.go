package hub

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestMalformedAuthenticatedRequests(t *testing.T) {
	h, s := setup(t)
	h.console = newConsoleAuth(make([]byte, 32))
	for _, tc := range []struct {
		method, path, body string
		status             int
	}{
		{"GET", "/v1/events?limit=bad", "", 400}, {"GET", "/v1/events?limit=-1", "", 400}, {"GET", "/v1/events?limit=1", "", 409},
		{"GET", "/v1/events?generation=bad", "", 400}, {"GET", "/v1/events?generation=-1", "", 400},
		{"POST", "/v1/tokens", "{", 400}, {"POST", "/v1/tokens", `{"host":"../bad"}`, 400}, {"DELETE", "/v1/tokens/missing", "", 404},
		{"POST", "/v1/wake-tickets", "{", 400}, {"POST", "/v1/wake-tickets", `{}`, 400}, {"POST", "/v1/wake-tickets", `{"generation":1,"execution_id":"missing"}`, 409},
		{"GET", "/v1/watch?after=bad", "", 400}, {"GET", "/v1/watch", "", 400},
		{"PUT", "/v1/ui/doctor", "{", 400}, {"POST", "/v1/ui/claims/no-number/revoke", `{}`, 403}, {"GET", "/v1/ui/threads/missing", "", 403},
	} {
		r := httptest.NewRequest(tc.method, s.URL+tc.path, strings.NewReader(tc.body))
		r.RemoteAddr = "127.0.0.1:1234"
		r.Header.Set("Authorization", "Bearer test-secret")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Fatalf("%s: %d %s", tc.path, w.Code, w.Body.String())
		}
	}
	_, cookie, csrf := routeConsoleAccount(t, h, "Malformed operator", "operator")
	consoleRoute(t, h, s, cookie, csrf, "POST", "/v1/ui/claims/no-number/revoke", `{}`, 400)
	consoleRoute(t, h, s, cookie, csrf, "GET", "/v1/ui/threads/missing", "", 404)
	r := httptest.NewRequest("GET", s.URL+"/v1/status", nil)
	r.TLS = &tls.ConnectionState{}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Header().Get("Content-Security-Policy"), "wss://") {
		t.Fatal(w.Code, w.Header())
	}
	w = httptest.NewRecorder()
	writeError(w, errors.New("private database failure"))
	if w.Code != 400 || strings.Contains(w.Body.String(), "private") {
		t.Fatal(w.Code, w.Body.String())
	}
	if loopback(&http.Request{RemoteAddr: "invalid"}) {
		t.Fatal("malformed address trusted")
	}
	r = httptest.NewRequest("GET", s.URL+"/v1/status", nil)
	r.RemoteAddr = "invalid"
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
}

func TestClosedStoreErrorsStayPrivate(t *testing.T) {
	h, s := setup(t)
	if err := h.store.Close(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ method, path, body string }{{"GET", "/v1/tokens", ""}, {"POST", "/v1/tokens", `{"host":"host"}`}, {"DELETE", "/v1/tokens/host", ""}} {
		r := httptest.NewRequest(tc.method, s.URL+tc.path, strings.NewReader(tc.body))
		r.Header.Set("Authorization", "Bearer test-secret")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 400 || strings.Contains(w.Body.String(), "database") {
			t.Fatal(w.Code, w.Body.String())
		}
	}
}

func TestPingFailureAndUIUpgradeQuota(t *testing.T) {
	h, s := setup(t)
	h.console = newConsoleAuth(make([]byte, 32))
	_, cookie, _ := routeConsoleAccount(t, h, "Quota operator", "operator")
	h.Limits.WebSockets = 1
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	endpoint := "ws" + strings.TrimPrefix(s.URL, "http") + "/v1/ui/watch"
	options := &websocket.DialOptions{HTTPHeader: http.Header{"Origin": {s.URL}, "Cookie": {cookie.String()}}}
	conn, _, err := websocket.Dial(ctx, endpoint, options)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.CloseNow() }()
	other, response, err := websocket.Dial(ctx, endpoint, options)
	if err == nil {
		_ = other.CloseNow()
		t.Fatal("UI socket cap bypassed")
	}
	if response == nil || response.StatusCode != 429 {
		t.Fatal(response, err)
	}
	cancelled, stop := context.WithCancel(ctx)
	stop()
	if h.ping(cancelled, conn) {
		t.Fatal("cancelled ping succeeded")
	}
	// Another account has its own bucket, and a failed HTTP upgrade releases it.
	account, second, csrf := routeConsoleAccount(t, h, "Second quota operator", "operator")
	r := httptest.NewRequest("GET", s.URL+"/v1/ui/watch", nil)
	r.RemoteAddr = "127.0.0.1:1234"
	r.Header.Set("Origin", s.URL)
	r.Header.Set("X-Iris-CSRF", csrf)
	r.AddCookie(second)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 426 {
		t.Fatal(w.Code, w.Body.String())
	}
	h.mu.Lock()
	retained := h.socketCounts["console:"+account.ID]
	h.mu.Unlock()
	if retained != 0 {
		t.Fatal("failed upgrade retained the account's socket slot", retained)
	}
}

func TestDecoderSizeAndBrokenBody(t *testing.T) {
	h, s := setup(t)
	r := httptest.NewRequest("POST", s.URL+"/v1/sessions", nil)
	r.Body = io.NopCloser(brokenReader{})
	r.Header.Set("Authorization", "Bearer test-secret")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 400 {
		t.Fatal(w.Code)
	}
	r = httptest.NewRequest("POST", "/", strings.NewReader(strings.Repeat(" ", 100)))
	r.Body = http.MaxBytesReader(w, r.Body, 10)
	if _, err := decode(w, r); err == nil {
		t.Fatal("oversized JSON accepted")
	}
}

type brokenReader struct{}

func (brokenReader) Read([]byte) (int, error) { return 0, errors.New("broken input") }

func TestTLSWakeTicketAndMissingConsoleSession(t *testing.T) {
	h, s := setup(t)
	session := register(t, s, 1)
	r := httptest.NewRequest("POST", s.URL+"/v1/wake-tickets", strings.NewReader(`{"execution_id":"`+session["execution_id"].(string)+`","generation":1}`))
	r.RemoteAddr = "127.0.0.1:1234"
	r.TLS = &tls.ConnectionState{}
	r.Header.Set("Authorization", "Bearer test-secret")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "wss://") {
		t.Fatal(w.Code, w.Body.String())
	}
	r = httptest.NewRequest("GET", s.URL+"/v1/ui/session", nil)
	r.RemoteAddr = "127.0.0.1:1234"
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatal("missing console cookie authorized", w.Code)
	}
}

func TestNegativeWatchCursorClosesStream(t *testing.T) {
	_, s := setup(t)
	session := register(t, s, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, watchURL(s, session["execution_id"], -1), &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer test-secret"}}, Subprotocols: []string{"iris.v1"}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.CloseNow() }()
	_, _, err = conn.Read(ctx)
	if websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
		t.Fatal("invalid cursor stream remained open", err)
	}
}

func TestRevocationBetweenAuthenticationAndUpgrade(t *testing.T) {
	h, s := setup(t)
	secret, err := h.store.AddToken(context.Background(), "race-host")
	if err != nil {
		t.Fatal(err)
	}
	// This principal models an HTTP request already authenticated when revoke commits.
	p := principal{name: "race-host", digest: tokenDigest(secret)}
	if _, err = h.store.RevokeToken(context.Background(), p.name); err != nil {
		t.Fatal(err)
	}
	h.mux.HandleFunc("GET /upgrade-race", func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		if h.trackSocket(r, conn, p) {
			h.untrackSocket(conn, p)
			t.Error("revoked principal registered stream")
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(s.URL, "http")+"/upgrade-race", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.CloseNow() }()
	_, _, err = conn.Read(ctx)
	if websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
		t.Fatal("revocation race left socket open", err)
	}
}
