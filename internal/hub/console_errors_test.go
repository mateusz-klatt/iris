package hub

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestConsoleMissingChallengeAndMalformedLoginDoNotReachPasswordWork(t *testing.T) {
	h, server, _ := consoleSetup(t)
	calls := 0
	h.console.verify = func(string, string) (bool, error) { calls++; return true, nil }
	consoleDo(t, h, server.URL, "POST", "/v1/ui/login", nil, consoleBrowser{}, 403)
	b := consoleChallengeBrowser(t, h, server.URL)
	consoleRoute(t, h, server, b.cookie, b.csrf, "POST", "/v1/ui/login", `{`, 400)
	// Even an undecodable attempt consumes the bound challenge.
	consoleRoute(t, h, server, b.cookie, b.csrf, "POST", "/v1/ui/login", `{}`, 403)
	consoleDo(t, h, server.URL, "GET", "/v1/ui/session", nil, consoleBrowser{}, 401)
	if calls != 0 {
		t.Fatal("unbound or malformed login reached password work")
	}
}

func TestConsoleLogoutFailureDoesNotClaimSuccessOrClearAnActiveCookie(t *testing.T) {
	h, server, filename := consoleSetup(t)
	consoleAccount(t, h, "Logout", "member")
	b := consoleSignIn(t, h, server.URL, "Logout")
	consoleDo(t, h, server.URL, "POST", "/v1/ui/logout", nil, consoleBrowser{cookie: b.cookie, csrf: "wrong"}, 403)
	consoleDo(t, h, server.URL, "GET", "/v1/ui/session", nil, b, 200)
	consoleDB(t, filename, `CREATE TRIGGER logout_unavailable BEFORE DELETE ON ConsoleSession BEGIN SELECT RAISE(ABORT,'storage unavailable'); END`)
	w := consoleDo(t, h, server.URL, "POST", "/v1/ui/logout", nil, b, 400)
	if len(w.Result().Cookies()) != 0 || strings.Contains(w.Body.String(), `"logged_out":true`) {
		t.Fatal("failed logout cleared cookie or claimed revocation")
	}
	consoleDB(t, filename, "DROP TRIGGER logout_unavailable")
	consoleDo(t, h, server.URL, "GET", "/v1/ui/session", nil, b, 200)
	consoleDo(t, h, server.URL, "POST", "/v1/ui/logout", nil, b, 200)
}

func TestConsoleStorageFailureNeverReturnsSessionOrAccountData(t *testing.T) {
	h, server, _ := consoleSetup(t)
	consoleAccount(t, h, "Storage", "operator")
	b := consoleSignIn(t, h, server.URL, "Storage")
	if err := h.store.Close(); err != nil {
		t.Fatal(err)
	}
	for _, route := range []struct{ method, path string }{{"GET", "/v1/ui/session"}, {"GET", "/v1/ui/accounts"}, {"POST", "/v1/ui/logout"}} {
		w := consoleDo(t, h, server.URL, route.method, route.path, nil, b, 400)
		if len(w.Result().Cookies()) != 0 || strings.Contains(w.Body.String(), `"account"`) || strings.Contains(w.Body.String(), `"accounts"`) {
			t.Fatal("storage failure exposed stale account data or changed cookie")
		}
	}
}

func TestRevokedSessionCannotStartPasswordWorkOrUpgradeSocket(t *testing.T) {
	h, server, _ := consoleSetup(t)
	consoleAccount(t, h, "Revoked", "operator")
	b := consoleSignIn(t, h, server.URL, "Revoked")
	if err := h.store.DeleteConsoleSession(context.Background(), h.console.mac("session", b.cookie.Value)); err != nil {
		t.Fatal(err)
	}
	calls := 0
	h.console.verify = func(string, string) (bool, error) { calls++; return true, nil }
	consoleDo(t, h, server.URL, "POST", "/v1/ui/password", map[string]string{"current_password": consoleTestPassword, "new_password": "another long password"}, b, 401)
	r := httptest.NewRequest(http.MethodGet, server.URL+"/v1/ui/watch", nil)
	r.RemoteAddr = "127.0.0.1:1234"
	r.Header.Set("Origin", server.URL)
	r.AddCookie(b.cookie)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized || calls != 0 || len(h.socketCounts) != 0 {
		t.Fatal("revoked session reached password work or socket upgrade", w.Code)
	}
}

func TestConsoleMutationCSRFAndMachineBoundariesApplyAcrossRoutes(t *testing.T) {
	h, server, _ := consoleSetup(t)
	a := consoleAccount(t, h, "Boundary", "operator")
	b := consoleSignIn(t, h, server.URL, a.Username)
	for _, route := range []struct{ method, path string }{
		{"PATCH", "/v1/ui/profile"}, {"POST", "/v1/ui/password"},
		{"PUT", "/v1/ui/accounts/" + a.ID + "/access"}, {"POST", "/v1/ui/messages"},
		{"POST", "/v1/ui/threads/unknown/read"}, {"POST", "/v1/ui/claims/1/revoke"},
	} {
		for _, csrf := range []string{"", "incorrect"} {
			consoleDo(t, h, server.URL, route.method, route.path, map[string]any{}, consoleBrowser{cookie: b.cookie, csrf: csrf}, 403)
		}
	}
	consoleDo(t, h, server.URL, "GET", "/v1/ui/session", nil, b, 200)
	request(t, server, "GET", "/v1/ui/session", nil, 403)
	for _, cookie := range []*http.Cookie{b.cookie, {Name: "iris_session", Value: "legacy"}} {
		r := httptest.NewRequest(http.MethodPut, server.URL+"/v1/ui/doctor", strings.NewReader(`{}`))
		r.RemoteAddr = "127.0.0.1:1234"
		r.AddCookie(cookie)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Fatal("browser cookie authorized machine doctor upload", w.Code)
		}
	}
}
