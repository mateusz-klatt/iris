package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/mateusz-klatt/iris/internal/config"
	"github.com/mateusz-klatt/iris/internal/consoleauth"
	"github.com/mateusz-klatt/iris/internal/store"
)

// This handler-level access/recovery rehearsal uses synthetic state. Real-binary
// activation, browser and restore evidence is recorded separately.
func TestNamedAccessCutoverAndRecoveryRehearsal(t *testing.T) {
	dir := t.TempDir()
	database := filepath.Join(dir, "rehearsal.db")
	keyPath := filepath.Join(dir, "console.key")
	key, err := config.ConsoleKey(keyPath)
	if err != nil {
		t.Fatal("could not create rehearsal console key", err)
	}
	start := func(key []byte) (*Hub, *httptest.Server, func()) {
		t.Helper()
		s, err := store.Open(database, nil)
		if err != nil {
			t.Fatal("could not open rehearsal database", err)
		}
		server := httptest.NewUnstartedServer(nil)
		h := New(s, "test-secret", []string{server.Listener.Addr().String()})
		h.console = newConsoleAuth(key)
		server.Config.Handler = h
		server.Start()
		stop := sync.OnceFunc(func() {
			server.Close()
			if err := s.Close(); err != nil {
				t.Error("could not close rehearsal database", err)
			}
		})
		t.Cleanup(stop)
		return h, server, stop
	}
	h, server, stop := start(key)
	const owner = "Rehearsal operator"
	const memberName = "Rehearsal member"
	const ownerPath = "/v1/admin/console/accounts"
	const recoveryPassword = "new synthetic recovery password"
	ownerBody, _ := json.Marshal(map[string]string{"username": owner, "password": consoleTestPassword})
	provision := ownerAccountRequest(t, h, server, ownerPath, string(ownerBody), "test-secret", 200)
	if len(provision.Result().Cookies()) != 0 {
		t.Fatal("owner provisioning minted a browser session")
	}
	operator, err := h.store.LookupConsoleAccount(t.Context(), owner)
	if err != nil || !operator.Enabled() || operator.Role != "operator" {
		t.Fatal("owner provisioning did not create an enabled named operator", err)
	}
	if valid, err := consoleauth.VerifyPassword(consoleTestPassword, operator.PasswordHash); err != nil || !valid {
		t.Fatal("provisioned operator password did not verify", err)
	}
	operatorBrowser := consoleSignIn(t, h, server.URL, owner)
	initialSession := consoleJSON(t, consoleDo(t, h, server.URL, "GET", "/v1/ui/session", nil, operatorBrowser, 200))
	registered := register(t, server, 1)
	repositoryID := registered["repository_id"].(string)
	agentID := registered["agent"].(map[string]any)["id"].(string)
	hash, err := consoleauth.CreatePassword(consoleTestPassword)
	if err != nil {
		t.Fatal("could not derive synthetic member password", err)
	}
	member, err := h.store.CreateConsoleAccount(t.Context(), store.ConsoleAccountInput{Username: memberName, PasswordHash: hash, Role: "member"})
	if err != nil {
		t.Fatal("could not create rehearsal member", err)
	}
	memberBrowser := consoleSignIn(t, h, server.URL, memberName)
	assertRepositories := func(b consoleBrowser, want int) {
		t.Helper()
		out := consoleJSON(t, consoleDo(t, h, server.URL, "GET", "/v1/ui/overview", nil, b, 200))
		if len(out["repositories"].([]any)) != want {
			t.Fatal("member overview did not enforce current repository grants")
		}
	}
	assertRepositories(memberBrowser, 0)
	consoleDo(t, h, server.URL, "GET", "/v1/ui/accounts", nil, memberBrowser, 403)
	accessPath := "/v1/ui/accounts/" + member.ID + "/access"
	accessBody := func(revision int64, role string, disabled bool, mode string) map[string]any {
		grants := []map[string]string{}
		if mode != "" {
			grants = append(grants, map[string]string{"repository_id": repositoryID, "mode": mode})
		}
		return map[string]any{"edit_revision": revision, "role": role, "disabled": disabled, "grants": grants}
	}
	consoleDo(t, h, server.URL, "PUT", accessPath, accessBody(1, "member", false, "rw"), memberBrowser, 403)
	consoleDo(t, h, server.URL, "PUT", accessPath, accessBody(1, "member", false, "ro"), operatorBrowser, 200)
	consoleDo(t, h, server.URL, "GET", "/v1/ui/session", nil, memberBrowser, 401)
	consoleDo(t, h, server.URL, "PUT", accessPath, accessBody(1, "member", false, "rw"), operatorBrowser, 409)
	accounts := consoleJSON(t, consoleDo(t, h, server.URL, "GET", "/v1/ui/accounts", nil, operatorBrowser, 200))["accounts"].([]any)
	var projected map[string]any
	for _, entry := range accounts {
		a := entry.(map[string]any)
		if a["id"] == member.ID {
			projected = a
		}
	}
	if projected == nil || projected["edit_revision"] != float64(2) {
		t.Fatal("conflict changed the accepted access revision")
	}
	grants, ok := projected["grants"].([]any)
	if !ok || len(grants) != 1 || grants[0].(map[string]any)["repository_id"] != repositoryID || grants[0].(map[string]any)["mode"] != "ro" {
		t.Fatal("operator list did not expose the accepted repository grant after a stale update")
	}
	memberBrowser = consoleSignIn(t, h, server.URL, memberName)
	assertRepositories(memberBrowser, 1)
	message := map[string]any{"key": "rehearsal-send", "repository_id": repositoryID, "to": []string{agentID}, "body": "Synthetic access rehearsal"}
	consoleDo(t, h, server.URL, "POST", "/v1/ui/messages", message, memberBrowser, 403)
	consoleDo(t, h, server.URL, "PUT", accessPath, accessBody(2, "member", false, "rw"), operatorBrowser, 200)
	consoleDo(t, h, server.URL, "GET", "/v1/ui/session", nil, memberBrowser, 401)
	memberBrowser = consoleSignIn(t, h, server.URL, memberName)
	sent := consoleJSON(t, consoleDo(t, h, server.URL, "POST", "/v1/ui/messages", message, memberBrowser, 200))
	if sent["message_id"] == nil {
		t.Fatal("member rw grant did not permit sending to the repository's agent")
	}
	consoleDo(t, h, server.URL, "PUT", accessPath, accessBody(3, "member", false, ""), operatorBrowser, 200)
	consoleDo(t, h, server.URL, "POST", "/v1/ui/messages", message, memberBrowser, 401)
	memberBrowser = consoleSignIn(t, h, server.URL, memberName)
	assertRepositories(memberBrowser, 0)
	consoleDo(t, h, server.URL, "POST", "/v1/ui/messages", message, memberBrowser, 403)
	consoleDo(t, h, server.URL, "PUT", accessPath, accessBody(4, "member", true, "rw"), operatorBrowser, 200)
	consoleDo(t, h, server.URL, "GET", "/v1/ui/session", nil, memberBrowser, 401)
	challenge := consoleChallengeBrowser(t, h, server.URL)
	consoleDo(t, h, server.URL, "POST", "/v1/ui/login", map[string]string{"username": memberName, "password": consoleTestPassword}, challenge, 401)
	none := consoleJSON(t, consoleDo(t, h, server.URL, "PUT", accessPath, accessBody(5, "none", false, ""), operatorBrowser, 200))["account"].(map[string]any)
	if none["role"] != "none" || none["disabled"] != true {
		t.Fatal("role none did not force disabled account state")
	}
	for _, rejected := range []map[string]any{accessBody(1, "operator", true, ""), accessBody(1, "member", false, ""), accessBody(1, "none", true, "")} {
		consoleDo(t, h, server.URL, "PUT", "/v1/ui/accounts/"+operator.ID+"/access", rejected, operatorBrowser, 409)
	}
	// Retired entry points and cookies cannot mint named-account authority. The
	// backend no longer stores legacy link or cookie grants.
	for _, path := range []string{"/ui/login?code=synthetic-retired-link", "/v1/ui/overview"} {
		want := 401
		if strings.HasPrefix(path, "/ui/login") {
			want = 404
		}
		w := consoleDo(t, h, server.URL, "GET", path, nil, consoleBrowser{cookie: &http.Cookie{Name: "iris_session", Value: "synthetic-retired-cookie"}}, want)
		if len(w.Result().Cookies()) != 0 {
			t.Fatal("retired login route or cookie minted account authority")
		}
	}
	request(t, server, "POST", "/v1/ui/links", map[string]any{}, 404)
	request(t, server, "GET", "/v1/ui/session", nil, 403)
	stop()
	reloaded, err := config.ConsoleKey(keyPath)
	if err != nil || !bytes.Equal(reloaded, key) {
		t.Fatal("ordinary rehearsal restart changed the console key", err)
	}
	h, server, stop = start(reloaded)
	restartedSession := consoleJSON(t, consoleDo(t, h, server.URL, "GET", "/v1/ui/session", nil, operatorBrowser, 200))
	if restartedSession["created_ms"] != initialSession["created_ms"] || restartedSession["expires_ms"] != initialSession["expires_ms"] {
		t.Fatal("ordinary restart lost or extended the verified operator session")
	}
	// Recovery stays owner-authenticated and restores the named identity. It must
	// revoke the old operator's session without granting the browser a bypass.
	recoveryBody, _ := json.Marshal(map[string]string{"username": owner, "password": recoveryPassword})
	w := ownerAccountRequest(t, h, server, ownerPath+"/reset", string(recoveryBody), "test-secret", 200)
	if len(w.Result().Cookies()) != 0 {
		t.Fatal("owner recovery created an anonymous browser session")
	}
	consoleDo(t, h, server.URL, "GET", "/v1/ui/session", nil, operatorBrowser, 401)
	recovered, err := h.store.LookupConsoleAccount(context.Background(), owner)
	if err != nil || recovered.ID != operator.ID || !recovered.Enabled() || recovered.Role != "operator" {
		t.Fatal("owner recovery changed the person or failed to restore enabled operator authority", err)
	}
	if valid, err := consoleauth.VerifyPassword(recoveryPassword, recovered.PasswordHash); err != nil || !valid {
		t.Fatal("replacement operator password did not verify", err)
	}
	challenge = consoleChallengeBrowser(t, h, server.URL)
	login := consoleDo(t, h, server.URL, "POST", "/v1/ui/login", map[string]string{"username": owner, "password": recoveryPassword}, challenge, 200)
	var recoveredCookie *http.Cookie
	for _, cookie := range login.Result().Cookies() {
		if cookie.Name == consoleCookieName {
			recoveredCookie = cookie
		}
	}
	if recoveredCookie == nil {
		t.Fatal("recovered named operator could not sign in")
	}
	consoleDo(t, h, server.URL, "GET", "/v1/ui/accounts", nil, consoleBrowser{cookie: recoveredCookie}, 200)
	stop()
}
