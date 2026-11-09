package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mateusz-klatt/iris/internal/consoleauth"
	"github.com/mateusz-klatt/iris/internal/store"
)

func ownerAccountRequest(t *testing.T, h *Hub, server *httptest.Server, path, body, token string, status int) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, server.URL+path, strings.NewReader(body))
	r.RemoteAddr = "127.0.0.1:1234"
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != status {
		t.Fatalf("owner account request status=%d, wanted=%d", w.Code, status)
	}
	return w
}

func TestOwnerProvisioningAndRecoveryRemainGatedAndOwnerOnly(t *testing.T) {
	h, server := setup(t)
	const path = "/v1/admin/console/accounts"
	const body = `{"username":"Owner","password":"a new operator password"}`
	ownerAccountRequest(t, h, server, path, body, "test-secret", http.StatusNotFound)
	h.console = newConsoleAuth(nil)
	ownerAccountRequest(t, h, server, path, body, "test-secret", http.StatusUnauthorized)
	h.console = newConsoleAuth(make([]byte, 32))
	host, err := h.store.AddToken(context.Background(), "another-host")
	if err != nil {
		t.Fatal(err)
	}
	ownerAccountRequest(t, h, server, path, body, host, http.StatusForbidden)
	ownerAccountRequest(t, h, server, path+"/reset", body, host, http.StatusForbidden)
	ownerAccountRequest(t, h, server, path, body, "", http.StatusUnauthorized)
	if _, err = h.store.LookupConsoleAccount(context.Background(), "Owner"); err == nil {
		t.Fatal("non-owner request created an operator")
	}
}

func TestOwnerCreateAndRecoverNamedOperatorWithoutReturningSecrets(t *testing.T) {
	h, server := setup(t)
	h.console = newConsoleAuth(make([]byte, 32))
	var logs bytes.Buffer
	h.Log = log.New(&logs, "", 0)
	h.AccessLog = true
	const path = "/v1/admin/console/accounts"
	const password = "a new operator password"
	const replacement = "a fresh recovery password"
	body := `{"username":"Owner","password":"` + password + `"}`
	w := ownerAccountRequest(t, h, server, path, body, "test-secret", http.StatusOK)
	var response struct{ Account store.ConsoleAccount }
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	account, err := h.store.LookupConsoleAccount(context.Background(), "owner")
	if err != nil || !account.Enabled() || account.Role != "operator" || account.ID != response.Account.ID {
		t.Fatal("owner setup did not create a named enabled operator", err)
	}
	if ok, err := consoleauth.VerifyPassword(password, account.PasswordHash); err != nil || !ok {
		t.Fatal("owner password was not derived correctly", err)
	}
	if response.Account.PasswordHash != "" || response.Account.LoginKey != "" || len(w.Result().Cookies()) != 0 || strings.Contains(w.Body.String(), password) || strings.Contains(w.Body.String(), account.PasswordHash) {
		t.Fatal("owner setup returned secret or anonymous session")
	}
	ownerAccountRequest(t, h, server, path, body, "test-secret", http.StatusConflict)
	digest := bytes.Repeat([]byte{7}, 32)
	if _, err = h.store.IssueConsoleSession(context.Background(), account, digest, ""); err != nil {
		t.Fatal(err)
	}
	before, err := h.store.ReplaceConsoleAccess(context.Background(), account.ID, account.EditRevision, "none", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	w = ownerAccountRequest(t, h, server, path+"/reset", `{"username":"OWNER","password":"`+replacement+`"}`, "test-secret", http.StatusOK)
	recovered, err := h.store.LookupConsoleAccount(context.Background(), "Owner")
	if err != nil || recovered.ID != account.ID || recovered.Role != "operator" || !recovered.Enabled() || recovered.AuthVersion != before.AuthVersion+1 {
		t.Fatal("owner recovery failed to restore named operator", err)
	}
	if ok, err := consoleauth.VerifyPassword(replacement, recovered.PasswordHash); err != nil || !ok {
		t.Fatal("owner recovery password was not derived correctly", err)
	}
	if _, err = h.store.ConsoleSessionInfo(context.Background(), digest); err == nil {
		t.Fatal("recovery retained old browser session")
	}
	if len(w.Result().Cookies()) != 0 || strings.Contains(w.Body.String(), replacement) || strings.Contains(w.Body.String(), recovered.PasswordHash) || strings.Contains(logs.String(), password) || strings.Contains(logs.String(), replacement) || strings.Contains(logs.String(), account.PasswordHash) || strings.Contains(logs.String(), recovered.PasswordHash) {
		t.Fatal("owner recovery returned or logged credentials")
	}
}

func TestOwnerAccountInputAndKDFAdmissionFailures(t *testing.T) {
	h, server := setup(t)
	h.console = newConsoleAuth(make([]byte, 32))
	const path = "/v1/admin/console/accounts"
	for _, body := range []string{`{`, `{"username":"Owner","password":"a long new password","role":"operator"}`, `{"username":"Owner"} {}`, `{"username":" Owner","password":"a long new password"}`, `{"username":"Owner","password":"short"}`} {
		ownerAccountRequest(t, h, server, path, body, "test-secret", http.StatusBadRequest)
	}
	ownerAccountRequest(t, h, server, path+"/reset", `{"username":"missing","password":"a long new password"}`, "test-secret", http.StatusNotFound)
	original := createOwnerPassword
	t.Cleanup(func() { createOwnerPassword = original })
	createOwnerPassword = func(string) (string, error) { return "", consoleauth.ErrBusy }
	w := ownerAccountRequest(t, h, server, path, `{"username":"Owner","password":"a long new password"}`, "test-secret", http.StatusTooManyRequests)
	if w.Header().Get("Retry-After") != "1" || !strings.Contains(w.Body.String(), `"code":"rate_limited"`) {
		t.Fatal("KDF admission refusal did not provide retry guidance")
	}
	for i := 0; i < 4; i++ {
		w = ownerAccountRequest(t, h, server, path, `{"username":"Owner","password":"a long new password"}`, "test-secret", http.StatusTooManyRequests)
	}
	if w.Header().Get("Retry-After") == "1" || w.Header().Get("Retry-After") == "" {
		t.Fatal("owner password requests bypassed per-name attempt quota")
	}
	createOwnerPassword = original
	if err := h.store.Close(); err != nil {
		t.Fatal(err)
	}
	ownerAccountRequest(t, h, server, path, `{"username":"Another","password":"a long new password"}`, "test-secret", http.StatusBadRequest)
}
