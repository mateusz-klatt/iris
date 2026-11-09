package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/mateusz-klatt/iris/internal/store"
)

// A trusted issuance fixture isolates routing and authorization from the password
// tests. Only the digest reaches SQLite; browser requests still use the raw cookie.
func routeConsoleAccount(t *testing.T, h *Hub, username, role string) (store.ConsoleAccount, *http.Cookie, string) {
	t.Helper()
	a, err := h.store.CreateConsoleAccount(context.Background(), store.ConsoleAccountInput{Username: username, PasswordHash: dummyConsoleHash, Role: role})
	if err != nil {
		t.Fatal(err)
	}
	token := secret()
	if _, err = h.store.IssueConsoleSession(context.Background(), a, h.console.mac("session", token), ""); err != nil {
		t.Fatal(err)
	}
	return a, &http.Cookie{Name: consoleCookieName, Value: token}, h.console.csrf("session-csrf", token)
}

func consoleRoute(t *testing.T, h *Hub, server *httptest.Server, cookie *http.Cookie, csrf, method, path, body string, status int) map[string]any {
	t.Helper()
	r := httptest.NewRequest(method, server.URL+path, strings.NewReader(body))
	r.RemoteAddr = "127.0.0.1:1234"
	r.Header.Set("Origin", server.URL)
	r.Header.Set("X-Iris-CSRF", csrf)
	r.AddCookie(cookie)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != status {
		t.Fatalf("console route %s %s: status %d, wanted %d", method, path, w.Code, status)
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal("console route did not return JSON", err)
	}
	return out
}

func TestConsoleProfileAndAccessRoutesRespectCurrentAccountAndRevisions(t *testing.T) {
	h, server := setup(t)
	h.console = newConsoleAuth(make([]byte, 32))
	operator, ownerCookie, ownerCSRF := routeConsoleAccount(t, h, "Operator", "operator")
	member, memberCookie, memberCSRF := routeConsoleAccount(t, h, "Member", "member")
	profile := `{"edit_revision":1,"display_name":"New display","ui_locale":"pl","correspondence_locale":"de"}`
	out := consoleRoute(t, h, server, memberCookie, memberCSRF, "PATCH", "/v1/ui/profile", profile, 200)
	account := out["account"].(map[string]any)
	if account["id"] != member.ID || account["display_name"] != "New display" || account["ui_locale"] != "pl" || account["edit_revision"] != float64(2) {
		t.Fatal("profile update lost self attribution or revision")
	}
	consoleRoute(t, h, server, memberCookie, memberCSRF, "PATCH", "/v1/ui/profile", profile, 409)
	consoleRoute(t, h, server, memberCookie, memberCSRF, "PATCH", "/v1/ui/profile", `{"edit_revision":2,"ui_locale":"unsupported"}`, 400)
	consoleRoute(t, h, server, memberCookie, memberCSRF, "GET", "/v1/ui/session", "", 200)
	consoleRoute(t, h, server, memberCookie, memberCSRF, "GET", "/v1/ui/accounts", "", 403)
	accounts := consoleRoute(t, h, server, ownerCookie, ownerCSRF, "GET", "/v1/ui/accounts", "", 200)["accounts"].([]any)
	if len(accounts) != 2 {
		t.Fatal("operator account list missing records")
	}
	for _, item := range accounts {
		entry := item.(map[string]any)
		if entry["password_hash"] != nil || entry["login_key"] != nil {
			t.Fatal("account list exposed credential material")
		}
	}
	path := "/v1/ui/accounts/" + member.ID + "/access"
	access := `{"edit_revision":2,"role":"member","disabled":false,"grants":[]}`
	consoleRoute(t, h, server, memberCookie, memberCSRF, "PUT", path, access, 403)
	out = consoleRoute(t, h, server, ownerCookie, ownerCSRF, "PUT", path, access, 200)
	if out["account"].(map[string]any)["auth_version"] != float64(member.AuthVersion+1) {
		t.Fatal("access route did not revoke sessions")
	}
	consoleRoute(t, h, server, memberCookie, memberCSRF, "GET", "/v1/ui/session", "", 401)
	consoleRoute(t, h, server, ownerCookie, ownerCSRF, "PUT", path, access, 409)
	consoleRoute(t, h, server, ownerCookie, ownerCSRF, "PUT", "/v1/ui/accounts/"+operator.ID+"/access", `{"edit_revision":1,"role":"none"}`, 409)
}

func TestConsoleObjectRoutingUsesPathIDsAndReturnsOperationResults(t *testing.T) {
	h, server := setup(t)
	h.console = newConsoleAuth(make([]byte, 32))
	_, cookie, csrf := routeConsoleAccount(t, h, "Operator", "operator")
	session := register(t, server, 1)
	agent := session["agent"].(map[string]any)
	body := `{"key":"route-retry","to":["` + agent["id"].(string) + `"],"subject":"route subject","body":"route body"}`
	sent := consoleRoute(t, h, server, cookie, csrf, "POST", "/v1/ui/messages", body, 200)
	message := sent["message_id"].(string)
	thread := sent["thread_id"].(string)
	for _, path := range []string{"/v1/ui/overview", "/v1/ui/deliveries", "/v1/ui/threads?q=route&agent=" + agent["id"].(string), "/v1/ui/threads/" + thread, "/v1/ui/messages/" + message + "/status", "/v1/ui/doctor"} {
		consoleRoute(t, h, server, cookie, csrf, "GET", path, "", 200)
	}
	if replay := consoleRoute(t, h, server, cookie, csrf, "POST", "/v1/ui/messages", body, 200); replay["message_id"] != message {
		t.Fatal("HTTP retry lost its accepted message")
	}
	read := consoleRoute(t, h, server, cookie, csrf, "POST", "/v1/ui/threads/"+thread+"/read", `{"key":"forged-body-id"}`, 200)
	if read["read"] != float64(0) {
		t.Fatal("opening sent mail acknowledged unrelated deliveries")
	}
	claimBody := authority(session)
	claimBody["key"], claimBody["path"], claimBody["kind"] = "route-claim", "file.go", "exact"
	claim := request(t, server, "POST", "/v1/claims", claimBody, 200)
	fence := strconv.FormatInt(int64(claim["fence"].(float64)), 10)
	consoleRoute(t, h, server, cookie, csrf, "POST", "/v1/ui/claims/"+fence+"/revoke", `{}`, 200)
	consoleRoute(t, h, server, cookie, csrf, "GET", "/v1/ui/messages/unknown/status", "", 404)
}

func TestConsoleRouteInputFailuresCannotFallThroughToLegacyAuthority(t *testing.T) {
	h, server := setup(t)
	h.console = newConsoleAuth(make([]byte, 32))
	_, cookie, csrf := routeConsoleAccount(t, h, "Operator", "operator")
	for _, test := range []struct {
		method, path, body string
		status             int
	}{
		{"PATCH", "/v1/ui/profile", `{`, 400},
		{"PATCH", "/v1/ui/profile", `{} {}`, 400},
		{"PATCH", "/v1/ui/profile", `{"role":"operator"}`, 400},
		{"PUT", "/v1/ui/accounts/unknown/access", `{`, 400},
		{"PUT", "/v1/ui/accounts/unknown/access", `{"role":"member","grants":[{"repository_id":"missing","mode":"ro"}]}`, 404},
		{"POST", "/v1/ui/claims/not-a-fence/revoke", `{}`, 400},
		{"POST", "/v1/ui/messages", `{`, 400},
		{"GET", "/v1/ui/threads?limit=bad", "", 400},
		{"GET", "/v1/ui/unknown", "", 404},
		{"GET", "/v1/ui/accounts/account/unknown", "", 404},
		{"POST", "/v1/ui/links", `{}`, 404},
	} {
		consoleRoute(t, h, server, cookie, csrf, test.method, test.path, test.body, test.status)
	}
}

func TestConsoleWatchHandshakeFailureReleasesAccountSlot(t *testing.T) {
	h, server := setup(t)
	h.console = newConsoleAuth(make([]byte, 32))
	a, cookie, csrf := routeConsoleAccount(t, h, "Operator", "operator")
	r := httptest.NewRequest("GET", server.URL+"/v1/ui/watch", nil)
	r.RemoteAddr = "127.0.0.1:1234"
	r.Header.Set("Origin", server.URL)
	r.Header.Set("X-Iris-CSRF", csrf)
	r.AddCookie(cookie)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusUpgradeRequired || len(h.socketCounts) != 0 {
		t.Fatal("failed console handshake retained a slot", w.Code)
	}
	h.Limits.WebSockets = 1
	h.socketCounts["console:"+a.ID] = 1
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") == "" || h.socketCounts["console:"+a.ID] != 1 {
		t.Fatal("account stream quota failed to refuse before upgrade", w.Code)
	}
}
