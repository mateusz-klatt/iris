package hub

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/mateusz-klatt/iris/internal/consoleauth"
	"github.com/mateusz-klatt/iris/internal/store"
)

const consoleTestPassword = "correct synthetic password"

type consoleBrowser struct {
	cookie *http.Cookie
	csrf   string
}

func consoleSetup(t *testing.T) (*Hub, *httptest.Server, string) {
	t.Helper()
	filename := filepath.Join(t.TempDir(), "console.db")
	s, err := store.Open(filename, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	server := httptest.NewUnstartedServer(nil)
	h := New(s, "test-secret", []string{server.Listener.Addr().String()})
	h.console = newConsoleAuth(bytes.Repeat([]byte{9}, 32))
	h.console.verify = func(password, record string) (bool, error) {
		return password == consoleTestPassword && record == dummyConsoleHash, nil
	}
	h.console.create = func(string) (string, error) { return dummyConsoleHash, nil }
	server.Config.Handler = h
	server.Start()
	t.Cleanup(server.Close)
	return h, server, filename
}
func consoleAccount(t *testing.T, h *Hub, name, role string) store.ConsoleAccount {
	t.Helper()
	a, err := h.store.CreateConsoleAccount(context.Background(), store.ConsoleAccountInput{Username: name, PasswordHash: dummyConsoleHash, Role: role})
	if err != nil {
		t.Fatal("synthetic account creation failed")
	}
	return a
}
func consoleDo(t *testing.T, h *Hub, base, method, path string, body any, b consoleBrowser, status int) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(method, base+path, bytes.NewReader(raw))
	r.RemoteAddr = "127.0.0.1:1234"
	r.Header.Set("Origin", base)
	if b.cookie != nil {
		r.AddCookie(b.cookie)
	}
	if b.csrf != "" {
		r.Header.Set("X-Iris-CSRF", b.csrf)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != status {
		t.Fatalf("console %s %s: status %d, expected %d", method, path, w.Code, status)
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("console response became cacheable")
	}
	return w
}
func consoleJSON(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if json.Unmarshal(w.Body.Bytes(), &out) != nil {
		t.Fatal("console response is not JSON")
	}
	return out
}
func consoleChallengeBrowser(t *testing.T, h *Hub, base string) consoleBrowser {
	t.Helper()
	w := consoleDo(t, h, base, "GET", "/v1/ui/login", nil, consoleBrowser{}, 200)
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != consoleLoginCookieName {
		t.Fatal("challenge cookie missing")
	}
	return consoleBrowser{cookies[0], consoleJSON(t, w)["csrf"].(string)}
}
func consoleSignIn(t *testing.T, h *Hub, base, name string) consoleBrowser {
	t.Helper()
	b := consoleChallengeBrowser(t, h, base)
	w := consoleDo(t, h, base, "POST", "/v1/ui/login", map[string]any{"username": name, "password": consoleTestPassword}, b, 200)
	var cookie *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == consoleCookieName {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("login omitted opaque cookie")
	}
	b = consoleBrowser{cookie: cookie}
	w = consoleDo(t, h, base, "GET", "/v1/ui/session", nil, b, 200)
	b.csrf = consoleJSON(t, w)["csrf"].(string)
	return b
}
func consoleDB(t *testing.T, filename, query string, args ...any) {
	t.Helper()
	db, err := sql.Open("sqlite", filename)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	// Live sockets revalidate on the hub's connection while this independent
	// connection arranges a committed expiry/reset. Wait for SQLite's writer
	// lock instead of depending on a gap between those real transactions.
	db.SetMaxOpenConns(1)
	if _, err = db.Exec("PRAGMA busy_timeout=5000"); err != nil {
		t.Fatal("could not configure synthetic database lock wait")
	}
	if _, err = db.Exec(query, args...); err != nil {
		t.Fatal("synthetic database mutation failed")
	}
}
func TestConsoleCookieDomainsAndCanonicalParsing(t *testing.T) {
	c := newConsoleAuth(bytes.Repeat([]byte{1}, 32))
	raw := secret()
	if bytes.Equal(c.mac("session", raw), c.mac("session-csrf", raw)) || c.csrf("session-csrf", raw) == raw {
		t.Fatal("console HMAC domains overlap")
	}
	for _, value := range []string{"", raw + "=", "short", strings.Repeat("!", 43)} {
		if validConsoleToken(value) {
			t.Fatal("noncanonical token accepted")
		}
	}
	r := httptest.NewRequest("GET", "/", nil)
	if consoleCookie(r, consoleCookieName) != "" {
		t.Fatal("missing cookie accepted")
	}
	r.AddCookie(&http.Cookie{Name: consoleCookieName, Value: raw})
	if consoleCookie(r, consoleCookieName) != raw {
		t.Fatal("canonical opaque cookie refused")
	}
	r.AddCookie(&http.Cookie{Name: consoleCookieName, Value: raw})
	if consoleCookie(r, consoleCookieName) != "" {
		t.Fatal("duplicate cookies selected authority")
	}
}
func TestConsoleChallengeSingleUseAndSessionProtection(t *testing.T) {
	h, server, _ := consoleSetup(t)
	a := consoleAccount(t, h, "Alice", "operator")
	b := consoleChallengeBrowser(t, h, server.URL)
	if !b.cookie.HttpOnly || b.cookie.Domain != "" || b.cookie.Path != "/" || b.cookie.SameSite != http.SameSiteStrictMode || b.cookie.Secure {
		t.Fatal("loopback challenge cookie protections changed")
	}
	// A pre-login challenge cannot inspect an account or log in through GET.
	consoleDo(t, h, server.URL, "GET", "/v1/ui/session", nil, b, 401)
	consoleDo(t, h, server.URL, "POST", "/v1/ui/login", map[string]any{"username": "Alice", "password": consoleTestPassword}, consoleBrowser{cookie: b.cookie}, 403)
	w := consoleDo(t, h, server.URL, "POST", "/v1/ui/login", map[string]any{"username": "Alice", "password": consoleTestPassword}, b, 200)
	consoleDo(t, h, server.URL, "POST", "/v1/ui/login", map[string]any{"username": "Alice", "password": consoleTestPassword}, b, 403)
	var cookie *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == consoleCookieName {
			cookie = c
		}
	}
	if cookie == nil || !cookie.HttpOnly || cookie.MaxAge != 43200 || cookie.Domain != "" || cookie.Path != "/" || cookie.SameSite != http.SameSiteStrictMode {
		t.Fatal("session cookie protections changed")
	}
	active := consoleBrowser{cookie: cookie}
	session := consoleJSON(t, consoleDo(t, h, server.URL, "GET", "/v1/ui/session", nil, active, 200))
	account := session["account"].(map[string]any)
	if account["id"] != a.ID || account["password_hash"] != nil || account["login_key"] != nil || session["token"] != nil {
		t.Fatal("session projection lost identity or exposed private fields")
	}
	if session["expires_ms"].(float64)-session["created_ms"].(float64) != float64((12*time.Hour)/time.Millisecond) {
		t.Fatal("session expiry is not absolute twelve hours")
	}
	active.csrf = session["csrf"].(string)
	consoleDo(t, h, server.URL, "POST", "/v1/ui/logout", nil, consoleBrowser{cookie: cookie}, 403)
	logout := consoleDo(t, h, server.URL, "POST", "/v1/ui/logout", nil, active, 200)
	if logout.Result().Cookies()[0].MaxAge != -1 {
		t.Fatal("logout did not clear cookie")
	}
	consoleDo(t, h, server.URL, "GET", "/v1/ui/session", nil, active, 401)
	// A retry with an already absent session needs exact Origin but no stale CSRF.
	consoleDo(t, h, server.URL, "POST", "/v1/ui/logout", nil, consoleBrowser{cookie: cookie}, 200)
	consoleDo(t, h, server.URL, "POST", "/v1/ui/logout", nil, consoleBrowser{}, 200)
}
func TestConsoleGenericLoginFailuresAndNoSensitiveOutput(t *testing.T) {
	h, server, filename := consoleSetup(t)
	enabled := consoleAccount(t, h, "Enabled", "member")
	disabled := consoleAccount(t, h, "Disabled", "member")
	consoleDB(t, filename, "UPDATE ConsoleAccount SET disabled=1 WHERE id=?", disabled.ID)
	consoleAccount(t, h, "None", "none")
	malformed := consoleAccount(t, h, "Malformed", "member")
	consoleDB(t, filename, "UPDATE ConsoleAccount SET password_hash='unusable-sensitive-record' WHERE id=?", malformed.ID)
	var logs bytes.Buffer
	h.Log = log.New(&logs, "", 0)
	count := 0
	h.console.verify = func(password, record string) (bool, error) {
		count++
		return password == consoleTestPassword && record == dummyConsoleHash, nil
	}
	expected := ""
	for _, test := range []struct{ name, password string }{{"Unknown", consoleTestPassword}, {enabled.Username, "wrong-sensitive-password"}, {disabled.Username, consoleTestPassword}, {"None", consoleTestPassword}, {malformed.Username, consoleTestPassword}, {" invalid name ", consoleTestPassword}} {
		b := consoleChallengeBrowser(t, h, server.URL)
		w := consoleDo(t, h, server.URL, "POST", "/v1/ui/login", map[string]any{"username": test.name, "password": test.password}, b, 401)
		if expected == "" {
			expected = w.Body.String()
		}
		if w.Body.String() != expected || !strings.Contains(w.Body.String(), "Invalid username or password") {
			t.Fatal("login failure disclosed account state")
		}
		for _, sentinel := range []string{test.name, test.password, b.cookie.Value, b.csrf, "unusable-sensitive-record"} {
			if sentinel != "" && (strings.Contains(logs.String(), sentinel) || strings.Contains(w.Body.String(), sentinel)) {
				t.Fatal("authentication emitted sensitive input")
			}
		}
	}
	if count != 6 {
		t.Fatal("rejected identities skipped bounded dummy work")
	}
}
func TestConsoleLoginRateQuotasExpireAndBoundMemory(t *testing.T) {
	h, server, _ := consoleSetup(t)
	consoleAccount(t, h, "Straße", "member")
	count := 0
	h.console.verify = func(string, string) (bool, error) { count++; return false, nil }
	for i := 0; i < 6; i++ {
		b := consoleChallengeBrowser(t, h, server.URL)
		status := 401
		if i == 5 {
			status = 429
		}
		w := consoleDo(t, h, server.URL, "POST", "/v1/ui/login", map[string]any{"username": []string{"STRASSE", "straße"}[i%2], "password": "wrong"}, b, status)
		if i == 5 && w.Header().Get("Retry-After") == "" {
			t.Fatal("attempt quota omitted retry delay")
		}
	}
	if count != 5 {
		t.Fatal("per-name quota ran extra KDF")
	}
	c := newConsoleAuth(bytes.Repeat([]byte{8}, 32))
	now := time.Now()
	// Fresh unknown names still share the aggregate KDF attempt quota.
	for i := 0; i < 31; i++ {
		w := httptest.NewRecorder()
		good := c.passwordRate(w, now, fmt.Sprintf("unknown-%d", i))
		if good != (i < 30) {
			t.Fatal("aggregate quota failed")
		}
	}
	// Rolling attempts reach bounded cardinality before the fifteen-minute expiry.
	c = newConsoleAuth(bytes.Repeat([]byte{8}, 32))
	for i := 0; i < consoleAttemptLimit; i++ {
		w := httptest.NewRecorder()
		if !c.passwordRate(w, now.Add(time.Duration(i/30)*time.Minute), fmt.Sprintf("unknown-%d", i)) {
			t.Fatal("bounded quota rejected admitted attempt")
		}
	}
	if c.passwordRate(httptest.NewRecorder(), now.Add(9*time.Minute), "new-name") || len(c.attempts) != consoleAttemptLimit {
		t.Fatal("password buckets grew without bound")
	}
	if !c.passwordRate(httptest.NewRecorder(), now.Add(25*time.Minute), "new-name") || len(c.attempts) != 1 {
		t.Fatal("expired password buckets were retained")
	}
}
func TestConsoleChallengeCapacityAndExpiry(t *testing.T) {
	h, server, _ := consoleSetup(t)
	now := time.Now()
	h.now = func() time.Time { return now }
	first := consoleChallengeBrowser(t, h, server.URL)
	for i := 1; i < consoleChallengeLimit; i++ {
		consoleChallengeBrowser(t, h, server.URL)
	}
	consoleDo(t, h, server.URL, "GET", "/v1/ui/login", nil, consoleBrowser{}, 429)
	now = now.Add(2 * time.Minute)
	consoleDo(t, h, server.URL, "POST", "/v1/ui/login", map[string]any{}, first, 403)
	consoleChallengeBrowser(t, h, server.URL)
	if len(h.console.challenges) != 1 {
		t.Fatal("expired challenges not pruned")
	}
}
func TestConsolePasswordChangeVerifiesCurrentAndEndsAllSessions(t *testing.T) {
	h, server, _ := consoleSetup(t)
	a := consoleAccount(t, h, "Password", "member")
	first := consoleSignIn(t, h, server.URL, a.Username)
	second := consoleSignIn(t, h, server.URL, a.Username)
	consoleDo(t, h, server.URL, "POST", "/v1/ui/password", map[string]any{"current_password": "wrong", "new_password": "a different valid password"}, first, 401)
	consoleDo(t, h, server.URL, "POST", "/v1/ui/password", map[string]any{"current_password": consoleTestPassword, "new_password": "short"}, first, 400)
	consoleDo(t, h, server.URL, "POST", "/v1/ui/password", map[string]any{"current_password": consoleTestPassword, "new_password": "a different valid password"}, first, 200)
	consoleDo(t, h, server.URL, "GET", "/v1/ui/session", nil, first, 401)
	consoleDo(t, h, server.URL, "GET", "/v1/ui/session", nil, second, 401)
	after, err := h.store.GetConsoleAccount(context.Background(), a.ID)
	if err != nil || after.AuthVersion != a.AuthVersion+1 {
		t.Fatal("password change failed to revoke account version")
	}
}
func TestConsoleKDFAndUpgradeErrorsFailClosed(t *testing.T) {
	h, server, _ := consoleSetup(t)
	a := consoleAccount(t, h, "Capacity", "member")
	for _, failure := range []error{consoleauth.ErrBusy, errors.New("private derivation failure")} {
		h.console.verify = func(string, string) (bool, error) { return false, failure }
		b := consoleChallengeBrowser(t, h, server.URL)
		want := 401
		if errors.Is(failure, consoleauth.ErrBusy) {
			want = 429
		}
		w := consoleDo(t, h, server.URL, "POST", "/v1/ui/login", map[string]any{"username": a.Username, "password": consoleTestPassword}, b, want)
		if want == 429 && w.Header().Get("Retry-After") == "" {
			t.Fatal("KDF capacity omitted retry delay")
		}
	}
}

func TestConsoleExactOriginTransportAndMixedCredentialBoundaries(t *testing.T) {
	h, server, _ := consoleSetup(t)
	consoleAccount(t, h, "Origin", "operator")
	b := consoleSignIn(t, h, server.URL, "Origin")
	for _, test := range []struct {
		method, path, origin, auth string
		want                       int
	}{
		{"POST", "/v1/ui/logout", "", "", 403},
		{"POST", "/v1/ui/logout", strings.ToUpper(server.URL), "", 403},
		{"POST", "/v1/ui/logout", server.URL + "/", "", 401},
		{"GET", "/v1/ui/watch", "", "", 403},
		{"GET", "/v1/ui/session", server.URL, "Bearer test-secret", 403},
		{"GET", "/v1/agents", server.URL, "Bearer test-secret", 403},
		{"GET", "/v1/tokens", server.URL, "Bearer test-secret", 403},
		{"PUT", "/v1/ui/doctor", server.URL, "Bearer test-secret", 403},
		{"GET", "/v1/agents", server.URL, "", 401},
		{"GET", "/ui/login?code=old-link", server.URL, "", 404},
	} {
		r := httptest.NewRequest(test.method, server.URL+test.path, strings.NewReader("{}"))
		r.RemoteAddr = "127.0.0.1:1234"
		r.AddCookie(b.cookie)
		r.Header.Set("X-Iris-CSRF", b.csrf)
		if test.origin != "" {
			r.Header.Set("Origin", test.origin)
		}
		if test.auth != "" {
			r.Header.Set("Authorization", test.auth)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != test.want {
			t.Fatalf("origin/credential boundary %s: status %d expected %d", test.path, w.Code, test.want)
		}
	}
	// A machine bearer still uploads doctor data independently.
	r := httptest.NewRequest("PUT", server.URL+"/v1/ui/doctor", strings.NewReader(`{"host":"synthetic","report":{}}`))
	r.RemoteAddr = "127.0.0.1:1234"
	r.Header.Set("Authorization", "Bearer test-secret")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code == 401 || w.Code == 403 {
		t.Fatal("machine doctor upload entered account authentication")
	}
	for _, test := range []struct {
		base, remote string
		forward      string
		secure       bool
		want         int
	}{
		{"http://127.0.0.1:7878", "127.0.0.1:1234", "", false, 200},
		{"http://[::1]:7878", "[::1]:1234", "", false, 200},
		{"http://localhost:7878", "127.0.0.1:1234", "", false, 401},
		{"http://127.0.0.2:7878", "127.0.0.1:1234", "", false, 401},
		{"http://127.0.0.1:7878", "192.0.2.1:1234", "https", false, 401},
		{"https://127.0.0.1:7878", "127.0.0.1:1234", "", true, 200},
	} {
		r = httptest.NewRequest("GET", test.base+"/v1/ui/login", nil)
		r.RemoteAddr = test.remote
		r.Header.Set("X-Forwarded-Proto", test.forward)
		h.hosts[r.Host] = true
		w = httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != test.want {
			t.Fatalf("literal-loopback transport: status %d expected %d", w.Code, test.want)
		}
		if test.want == 200 && w.Result().Cookies()[0].Secure != test.secure {
			t.Fatal("TLS cookie secure flag incorrect")
		}
	}
	if h.SetPublicURL("https://console.example.invalid") != nil {
		t.Fatal("public origin rejected")
	}
	for _, test := range []struct {
		remote, forward, host string
		want                  int
	}{
		{"127.0.0.1:1234", "https", "console.example.invalid", 200},
		{"[::1]:1234", "https", "console.example.invalid", 200},
		{"192.0.2.1:1234", "https", "console.example.invalid", 401},
		{"127.0.0.1:1234", "http", "console.example.invalid", 401},
		{"127.0.0.1:1234", "https", "127.0.0.1:7878", 401},
	} {
		r = httptest.NewRequest("GET", "http://"+test.host+"/v1/ui/login", nil)
		r.RemoteAddr = test.remote
		r.Header.Set("X-Forwarded-Proto", test.forward)
		w = httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != test.want {
			t.Fatalf("proxy origin boundary: status %d expected %d", w.Code, test.want)
		}
		if test.want == 200 && !w.Result().Cookies()[0].Secure {
			t.Fatal("public proxy cookie lacks Secure")
		}
	}
}
func TestConsoleMissingKeyAndRotationFailClosedWithoutStoppingCoordination(t *testing.T) {
	h, server, _ := consoleSetup(t)
	consoleAccount(t, h, "Rotate", "member")
	b := consoleSignIn(t, h, server.URL, "Rotate")
	h.console = newConsoleAuth(bytes.Repeat([]byte{7}, 32))
	consoleDo(t, h, server.URL, "GET", "/v1/ui/session", nil, b, 401)
	h.console = newConsoleAuth(nil)
	consoleDo(t, h, server.URL, "GET", "/v1/ui/login", nil, consoleBrowser{}, 401)
	r := httptest.NewRequest("GET", server.URL+"/v1/agents", nil)
	r.RemoteAddr = "127.0.0.1:1234"
	r.Header.Set("Authorization", "Bearer test-secret")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal("failed console key stopped agent coordination")
	}
}
func TestConsoleShortImportedPasswordUpgradesAtomically(t *testing.T) {
	h, server, _ := consoleSetup(t)
	const imported = "scrypt$16384$8$1$AAECAwQFBgcICQoLDA0ODw$5UJCybzgeNrUxZTV5mzrOYr8Aj1OlDXLrgTFXHxr1m4"
	a, err := h.store.CreateConsoleAccount(context.Background(), store.ConsoleAccountInput{Username: "Imported", PasswordHash: imported, Role: "member"})
	if err != nil {
		t.Fatal("synthetic imported account refused")
	}
	h.console.verify = consoleauth.VerifyPassword
	for _, failure := range []error{consoleauth.ErrBusy, errors.New("private upgrade failure")} {
		h.console.upgrade = func(string) (string, error) { return "", failure }
		b := consoleChallengeBrowser(t, h, server.URL)
		want := 401
		if errors.Is(failure, consoleauth.ErrBusy) {
			want = 429
		}
		consoleDo(t, h, server.URL, "POST", "/v1/ui/login", map[string]any{"username": a.Username, "password": "x"}, b, want)
		after, err := h.store.GetConsoleAccount(context.Background(), a.ID)
		if err != nil || after.PasswordHash != imported || after.AuthVersion != a.AuthVersion {
			t.Fatal("failed upgrade changed account")
		}
	}
	h.console.upgrade = consoleauth.UpgradePassword
	b := consoleChallengeBrowser(t, h, server.URL)
	consoleDo(t, h, server.URL, "POST", "/v1/ui/login", map[string]any{"username": a.Username, "password": "x"}, b, 200)
	after, err := h.store.GetConsoleAccount(context.Background(), a.ID)
	if err != nil || !strings.HasPrefix(after.PasswordHash, "$argon2id$") || after.AuthVersion != a.AuthVersion+1 {
		t.Fatal("short imported login did not upgrade atomically")
	}
	if good, err := consoleauth.VerifyPassword("x", after.PasswordHash); err != nil || !good {
		t.Fatal("import upgrade changed password bytes")
	}
}
func TestConsoleMaximumInputAndResetDuringVerificationCannotIssueSession(t *testing.T) {
	h, server, filename := consoleSetup(t)
	a := consoleAccount(t, h, "Concurrent", "member")
	count := 0
	h.console.verify = func(string, string) (bool, error) { count++; return true, nil }
	b := consoleChallengeBrowser(t, h, server.URL)
	consoleDo(t, h, server.URL, "POST", "/v1/ui/login", map[string]any{"username": a.Username, "password": strings.Repeat("x", 1025)}, b, 401)
	if count != 0 {
		t.Fatal("oversized password reached KDF")
	}
	h.console.verify = func(string, string) (bool, error) {
		consoleDB(t, filename, "UPDATE ConsoleAccount SET auth_version=auth_version+1 WHERE id=?", a.ID)
		return true, nil
	}
	b = consoleChallengeBrowser(t, h, server.URL)
	consoleDo(t, h, server.URL, "POST", "/v1/ui/login", map[string]any{"username": a.Username, "password": consoleTestPassword}, b, 401)
}

func consoleRepository(t *testing.T, h *Hub, name string) (string, string) {
	t.Helper()
	out, err := h.store.Call(context.Background(), "sessions", store.Request{Client: "codex", Host: "synthetic-host", SessionID: name, Cwd: "/synthetic/" + name, Worktree: "/synthetic/" + name, RemoteURL: "https://example.invalid/" + name + ".git", RootCommit: name, Source: "hook"})
	if err != nil {
		t.Fatal("synthetic repository registration failed")
	}
	return out["repository_id"].(string), out["agent"].(map[string]any)["id"].(string)
}
func TestConsoleHTTPDowngradeRefusesOldPreparedAndCachedWrites(t *testing.T) {
	for _, mode := range []string{"ro", "none"} {
		t.Run(mode, func(t *testing.T) {
			h, server, _ := consoleSetup(t)
			repo, agent := consoleRepository(t, h, "write-scope")
			a := consoleAccount(t, h, "Writer", "member")
			var err error
			a, err = h.store.ReplaceConsoleAccess(context.Background(), a.ID, a.EditRevision, "member", false, []store.ConsoleGrant{{RepositoryID: repo, Mode: "rw"}})
			if err != nil {
				t.Fatal("write grant failed")
			}
			b := consoleSignIn(t, h, server.URL, a.Username)
			accepted := map[string]any{"key": "saved-send", "repository_id": repo, "to": []string{agent}, "body": "first"}
			first := consoleJSON(t, consoleDo(t, h, server.URL, "POST", "/v1/ui/messages", accepted, b, 200))
			replay := consoleJSON(t, consoleDo(t, h, server.URL, "POST", "/v1/ui/messages", accepted, b, 200))
			if first["message_id"] != replay["message_id"] {
				t.Fatal("HTTP exact retry duplicated message")
			}
			prepared := map[string]any{"key": "prepared-send", "repository_id": repo, "to": []string{agent}, "body": "prepared"}
			reply := map[string]any{"key": "prepared-reply", "repository_id": repo, "to": []string{agent}, "body": "prepared reply", "reply_to": first["message_id"]}
			grants := []store.ConsoleGrant{}
			if mode == "ro" {
				grants = append(grants, store.ConsoleGrant{RepositoryID: repo, Mode: "ro"})
			}
			_, err = h.store.ReplaceConsoleAccess(context.Background(), a.ID, a.EditRevision, "member", false, grants)
			if err != nil {
				t.Fatal("downgrade failed")
			}
			for _, body := range []any{accepted, prepared, reply} {
				consoleDo(t, h, server.URL, "POST", "/v1/ui/messages", body, b, 401)
			}
			// Three attempts stay under the account's login/change quota.
			fresh := consoleSignIn(t, h, server.URL, a.Username)
			consoleDo(t, h, server.URL, "GET", "/v1/ui/overview", nil, fresh, 200)
			for _, body := range []any{accepted, prepared, reply} {
				consoleDo(t, h, server.URL, "POST", "/v1/ui/messages", body, fresh, 403)
			}
		})
	}
}
func consoleDial(t *testing.T, server *httptest.Server, b consoleBrowser) *websocket.Conn {
	t.Helper()
	header := http.Header{"Origin": []string{server.URL}, "Cookie": []string{b.cookie.Name + "=" + b.cookie.Value}}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/v1/ui/watch", &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		t.Fatal("account socket handshake failed")
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	_, raw, err := conn.Read(ctx)
	if err != nil {
		t.Fatal("account socket omitted initial scoped frame")
	}
	var frame map[string]any
	if json.Unmarshal(raw, &frame) != nil || len(frame) != 2 || frame["kind"] != "changed" || frame["cursor"] == nil {
		t.Fatal("account socket frame exposed unexpected data")
	}
	return conn
}
func TestConsoleLiveSocketsCloseForLogoutAccessPasswordAndExpiry(t *testing.T) {
	for _, cause := range []string{"logout", "access", "password", "reset", "disabled", "expiry"} {
		t.Run(cause, func(t *testing.T) {
			h, server, filename := consoleSetup(t)
			a := consoleAccount(t, h, "Socket", "operator")
			first := consoleSignIn(t, h, server.URL, a.Username)
			second := consoleSignIn(t, h, server.URL, a.Username)
			sockets := []*websocket.Conn{consoleDial(t, server, first), consoleDial(t, server, second)}
			count := 2
			switch cause {
			case "logout":
				consoleDo(t, h, server.URL, "POST", "/v1/ui/logout", nil, first, 200)
				count = 1
			case "access", "disabled":
				_, err := h.store.ReplaceConsoleAccess(context.Background(), a.ID, a.EditRevision, "operator", cause == "disabled", nil)
				if err != nil {
					t.Fatal("socket access revocation failed")
				}
			case "reset":
				if _, err := h.store.RecoverConsoleOperator(context.Background(), a, dummyConsoleHash); err != nil {
					t.Fatal("owner recovery failed to revoke sockets")
				}
			case "password":
				consoleDo(t, h, server.URL, "POST", "/v1/ui/password", map[string]any{"current_password": consoleTestPassword, "new_password": "another sufficiently long password"}, first, 200)
			case "expiry":
				consoleDB(t, filename, "UPDATE ConsoleSession SET created_ms=1,expires_ms=2 WHERE account_id=?", a.ID)
			}
			for _, conn := range sockets[:count] {
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				_, _, err := conn.Read(ctx)
				cancel()
				if err == nil || errors.Is(err, context.DeadlineExceeded) {
					t.Fatal("revoked live account socket remained open")
				}
			}
			if cause == "logout" {
				consoleDo(t, h, server.URL, "GET", "/v1/ui/session", nil, second, 200)
			}
		})
	}
}
func TestConsoleLiveFramesIgnoreHiddenRepositoryActivity(t *testing.T) {
	h, server, _ := consoleSetup(t)
	repo, _ := consoleRepository(t, h, "visible")
	a := consoleAccount(t, h, "Scoped", "member")
	a, err := h.store.ReplaceConsoleAccess(context.Background(), a.ID, a.EditRevision, "member", false, []store.ConsoleGrant{{RepositoryID: repo, Mode: "ro"}})
	if err != nil {
		t.Fatal("read grant failed")
	}
	b := consoleSignIn(t, h, server.URL, a.Username)
	conn := consoleDial(t, server, b)
	result := make(chan error, 1)
	go func() { _, _, err := conn.Read(context.Background()); result <- err }()
	consoleRepository(t, h, "hidden")
	select {
	case err := <-result:
		t.Fatal("hidden activity emitted a frame or closed the account stream", err)
	case <-time.After(100 * time.Millisecond):
	}
	if _, err = h.store.ReplaceConsoleAccess(context.Background(), a.ID, a.EditRevision, "member", false, nil); err != nil {
		t.Fatal("grant removal failed")
	}
	select {
	case err = <-result:
		if err == nil {
			t.Fatal("revocation emitted frame after access loss")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("quiet scoped socket was not woken for revocation")
	}
}

func TestConsolePasswordFailureAdmissionAndCommitRecheck(t *testing.T) {
	for _, cause := range []string{"malformed", "quota", "verify_busy", "create_busy", "create_error", "changed_during_kdf"} {
		t.Run(cause, func(t *testing.T) {
			h, server, filename := consoleSetup(t)
			a := consoleAccount(t, h, "PasswordFailure", "member")
			b := consoleSignIn(t, h, server.URL, a.Username)
			body := map[string]any{"current_password": consoleTestPassword, "new_password": "another valid synthetic password"}
			want := 429
			verifies, creates := 0, 0
			h.console.verify = func(string, string) (bool, error) { verifies++; return true, nil }
			h.console.create = func(string) (string, error) { creates++; return dummyConsoleHash, nil }
			switch cause {
			case "malformed":
				body["account_id"] = a.ID
				want = 400
			case "quota":
				for i := 0; i < 4; i++ {
					if !h.console.passwordRate(httptest.NewRecorder(), h.now(), a.Username) {
						t.Fatal("test did not fill existing account quota")
					}
				}
			case "verify_busy":
				h.console.verify = func(string, string) (bool, error) { verifies++; return false, consoleauth.ErrBusy }
			case "create_busy":
				h.console.create = func(string) (string, error) { creates++; return "", consoleauth.ErrBusy }
			case "create_error":
				h.console.create = func(string) (string, error) { creates++; return "", errors.New("private derivation error") }
				want = 400
			case "changed_during_kdf":
				h.console.create = func(string) (string, error) {
					creates++
					consoleDB(t, filename, "UPDATE ConsoleAccount SET auth_version=auth_version+1 WHERE id=?", a.ID)
					return dummyConsoleHash, nil
				}
				want = 401
			}
			w := consoleDo(t, h, server.URL, "POST", "/v1/ui/password", body, b, want)
			if strings.Contains(w.Body.String(), "private derivation error") {
				t.Fatal("password failure exposed derivation error")
			}
			if (cause == "malformed" || cause == "quota") && (verifies != 0 || creates != 0) {
				t.Fatal("rejected password request ran KDF")
			}
			if cause == "verify_busy" && creates != 0 {
				t.Fatal("verification capacity failure continued to derivation")
			}
			after, err := h.store.GetConsoleAccount(context.Background(), a.ID)
			if err != nil || after.PasswordHash != a.PasswordHash {
				t.Fatal("failed password change changed stored credentials")
			}
			if want == 429 && w.Header().Get("Retry-After") == "" {
				t.Fatal("password capacity failure omitted retry delay")
			}
		})
	}
}

func TestConsoleTLSAndProxyMutationsRequireExactOrigin(t *testing.T) {
	h, _, _ := consoleSetup(t)
	for _, proxy := range []bool{false, true} {
		base := "https://127.0.0.1:7878"
		origin := base
		if proxy {
			if h.SetPublicURL("https://console.example.invalid") != nil {
				t.Fatal("public origin rejected")
			}
			base = "http://console.example.invalid"
			origin = "https://console.example.invalid"
		}
		for _, duplicate := range []bool{false, true} {
			r := httptest.NewRequest("POST", base+"/v1/ui/logout", nil)
			r.RemoteAddr = "127.0.0.1:1234"
			h.hosts[r.Host] = true
			r.Header.Set("Origin", origin)
			if proxy {
				r.Header.Set("X-Forwarded-Proto", "https")
			}
			if duplicate {
				r.Header.Add("Origin", origin)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			want := 200
			if duplicate {
				want = 403
			}
			if w.Code != want {
				t.Fatalf("TLS/proxy exact-Origin mutation: status %d expected %d", w.Code, want)
			}
			if want == 200 && !w.Result().Cookies()[0].Secure {
				t.Fatal("TLS/proxy logout clearing cookie omitted Secure")
			}
		}
	}
}
