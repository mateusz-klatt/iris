package hub

import (
	"bytes"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/mateusz-klatt/iris/internal/config"
	"github.com/mateusz-klatt/iris/internal/consoleauth"
	"github.com/mateusz-klatt/iris/internal/store"
)

func TestConsoleTrustedProxyCompletesLoginAndRejectsUntrustedMutations(t *testing.T) {
	h, _, filename := consoleSetup(t)
	consoleAccount(t, h, "Proxied", "member")
	const host = "console.example.invalid"
	const origin = "https://" + host
	if err := h.SetPublicURL(origin); err != nil {
		t.Fatal(err)
	}
	call := func(method, path, body, remote, forwarded, from string, b consoleBrowser, want int) *httptest.ResponseRecorder {
		t.Helper()
		// The trusted proxy terminates TLS; the hub sees loopback HTTP and the
		// original public Host, exact browser Origin and overwritten scheme.
		r := httptest.NewRequest(method, "http://"+host+path, strings.NewReader(body))
		r.RemoteAddr = remote
		if forwarded != "" {
			r.Header.Set("X-Forwarded-Proto", forwarded)
		}
		if from != "" {
			r.Header.Set("Origin", from)
		}
		if b.cookie != nil {
			r.AddCookie(b.cookie)
		}
		if b.csrf != "" {
			r.Header.Set("X-Iris-CSRF", b.csrf)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != want || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("proxied account request status=%d, wanted=%d", w.Code, want)
		}
		return w
	}
	challenge := call(http.MethodGet, "/v1/ui/login", "", "127.0.0.1:1234", "https", origin, consoleBrowser{}, 200)
	if cookies := challenge.Result().Cookies(); len(cookies) != 1 || !cookies[0].Secure || !cookies[0].HttpOnly || cookies[0].Domain != "" || cookies[0].Path != "/" || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatal("proxied pre-login cookie lost its protections")
	}
	b := consoleBrowser{cookie: challenge.Result().Cookies()[0], csrf: consoleJSON(t, challenge)["csrf"].(string)}
	const loginBody = `{"username":"Proxied","password":"` + consoleTestPassword + `"}`
	for _, test := range []struct {
		remote, forwarded, from string
		want                    int
	}{
		{"192.0.2.1:1234", "https", origin, 401},
		{"127.0.0.1:1234", "http", origin, 401},
		{"127.0.0.1:1234", "", origin, 401},
		{"127.0.0.1:1234", "https", "https://another.example.invalid", 401},
		{"127.0.0.1:1234", "https", "http://" + host, 401},
		{"127.0.0.1:1234", "https", "", 403},
		{"127.0.0.1:1234", "https", "https://CONSOLE.example.invalid", 403},
	} {
		w := call(http.MethodPost, "/v1/ui/login", loginBody, test.remote, test.forwarded, test.from, b, test.want)
		for _, cookie := range w.Result().Cookies() {
			if cookie.Name == consoleCookieName && cookie.Value != "" {
				t.Fatal("refused proxy login issued an account cookie")
			}
		}
	}
	// A rejected challenge-bound login must not have persisted a hidden session.
	db, err := sql.Open("sqlite", filename)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var sessions int
	if err = db.QueryRow("SELECT COUNT(*) FROM ConsoleSession").Scan(&sessions); err != nil || sessions != 0 {
		t.Fatal("refused proxy login changed session state", err)
	}
	wrongCSRF := b
	wrongCSRF.csrf = "wrong"
	call(http.MethodPost, "/v1/ui/login", loginBody, "127.0.0.1:1234", "https", origin, wrongCSRF, 403)
	login := call(http.MethodPost, "/v1/ui/login", loginBody, "127.0.0.1:1234", "https", origin, b, 200)
	var cookie *http.Cookie
	for _, c := range login.Result().Cookies() {
		if c.Name == consoleCookieName {
			cookie = c
		}
	}
	if cookie == nil || !cookie.Secure || !cookie.HttpOnly || cookie.Domain != "" || cookie.Path != "/" || cookie.SameSite != http.SameSiteStrictMode || cookie.MaxAge != 43200 {
		t.Fatal("proxied account login omitted its protected Secure cookie")
	}
	active := consoleBrowser{cookie: cookie}
	session := consoleJSON(t, call(http.MethodGet, "/v1/ui/session", "", "127.0.0.1:1234", "https", origin, active, 200))
	active.csrf = session["csrf"].(string)
	if session["account"].(map[string]any)["username"] != "Proxied" || active.csrf == b.csrf || active.csrf == cookie.Value {
		t.Fatal("proxied session did not return account-bound CSRF")
	}
	call(http.MethodPost, "/v1/ui/logout", `{}`, "192.0.2.1:1234", "https", origin, active, 401)
	call(http.MethodPost, "/v1/ui/logout", `{}`, "127.0.0.1:1234", "https", "", active, 403)
	call(http.MethodGet, "/v1/ui/session", "", "127.0.0.1:1234", "https", origin, active, 200)
}

func TestConsoleHTTPRestartKeepsSessionAndAbsoluteExpiryUntilKeyRotation(t *testing.T) {
	dir := t.TempDir()
	database := filepath.Join(dir, "console.db")
	keyPath := filepath.Join(dir, "console.key")
	key, err := config.ConsoleKey(keyPath)
	if err != nil {
		t.Fatal("could not create protected console key", err)
	}
	start := func(key []byte) (*Hub, *httptest.Server, func()) {
		t.Helper()
		s, err := store.Open(database, nil)
		if err != nil {
			t.Fatal(err)
		}
		server := httptest.NewUnstartedServer(nil)
		h := New(s, "test-secret", []string{server.Listener.Addr().String()})
		h.console = newConsoleAuth(key)
		server.Config.Handler = h
		server.Start()
		stop := sync.OnceFunc(func() {
			server.Close()
			if err := s.Close(); err != nil {
				t.Error("could not close staged hub", err)
			}
		})
		t.Cleanup(stop)
		return h, server, stop
	}
	h, server, stop := start(key)
	hash, err := consoleauth.CreatePassword(consoleTestPassword)
	if err != nil {
		t.Fatal("could not derive synthetic restart password", err)
	}
	a, err := h.store.CreateConsoleAccount(t.Context(), store.ConsoleAccountInput{Username: "Persistent", PasswordHash: hash, Role: "member"})
	if err != nil {
		t.Fatal("could not create synthetic restart account", err)
	}
	b := consoleSignIn(t, h, server.URL, a.Username)
	initial := consoleJSON(t, consoleDo(t, h, server.URL, http.MethodGet, "/v1/ui/session", nil, b, 200))
	stop()
	reloaded, err := config.ConsoleKey(keyPath)
	if err != nil || !bytes.Equal(key, reloaded) {
		t.Fatal("normal restart changed console key", err)
	}
	h, server, stop = start(reloaded)
	restored := consoleJSON(t, consoleDo(t, h, server.URL, http.MethodGet, "/v1/ui/session", nil, b, 200))
	if restored["account"].(map[string]any)["id"] != a.ID || restored["created_ms"] != initial["created_ms"] || restored["expires_ms"] != initial["expires_ms"] || restored["csrf"] != initial["csrf"] {
		t.Fatal("normal HTTP restart lost or extended account session")
	}
	stop()
	// Rotation is an offline operation: the old hub and sockets have ended before
	// replacing the independent key and constructing a fresh hub instance.
	if err = os.Remove(keyPath); err != nil {
		t.Fatal(err)
	}
	rotated, err := config.ConsoleKey(keyPath)
	if err != nil || bytes.Equal(key, rotated) {
		t.Fatal("console key rotation did not create an independent key", err)
	}
	h, server, _ = start(rotated)
	consoleDo(t, h, server.URL, http.MethodGet, "/v1/ui/session", nil, b, 401)
}
