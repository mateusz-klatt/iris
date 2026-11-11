package hub

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mateusz-klatt/iris/internal/consoleauth"
)

func createFreshOperator(t *testing.T, h *Hub, name string, status int) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, activationBase+"/v1/admin/console/accounts", strings.NewReader(activationPayload(name, consoleTestPassword)))
	r.Header.Set("Authorization", "Bearer test-secret")
	r.RemoteAddr = "127.0.0.1:1234"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != status {
		t.Fatalf("create status %d, want %d: %s", w.Code, status, w.Body.String())
	}
	if len(w.Result().Cookies()) != 0 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("owner create issued browser authority or allowed caching")
	}
	return w
}

func TestFreshOwnerCreatePreparesKeyAfterCommitAndNamedLoginOpensAfterRestart(t *testing.T) {
	h := activationHub(t)
	prepared := 0
	key := bytes.Repeat([]byte{7}, 32)
	if err := h.ConfigureConsole(nil, func() bool { return prepared == 0 }, func() bool { return prepared > 0 }, func() error {
		// Looking up the account proves the creation transaction has committed
		// and released its mutex before key preparation starts.
		a, err := h.store.LookupConsoleAccount(t.Context(), "fresh operator")
		if err != nil || !a.Enabled() || a.Role != "operator" {
			t.Fatal("key preparation preceded the committed operator", err)
		}
		prepared++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	w := createFreshOperator(t, h, "fresh operator", 200)
	result := consoleJSON(t, w)
	if result["account_created"] != true || result["key_prepared"] != true || result["restart_required"] != true || prepared != 1 || len(h.console.key) != 0 {
		t.Fatal("fresh create did not report exactly its actions or hot-opened the console")
	}
	consoleDo(t, h, activationBase, "GET", "/v1/ui/login", nil, consoleBrowser{}, 401)
	w = createFreshOperator(t, h, "pending operator", 200)
	result = consoleJSON(t, w)
	if result["key_prepared"] != false || result["restart_required"] != true || prepared != 1 {
		t.Fatal("a later create lost the pending restart or prepared the key again")
	}
	if err := h.ConfigureConsole(key, func() bool { return false }, nil, nil); err != nil {
		t.Fatal(err)
	}
	// Production verification proves this is the password create set.
	browser := consoleSignIn(t, h, activationBase, "fresh operator")
	consoleDo(t, h, activationBase, "GET", "/v1/ui/session", nil, browser, 200)
}

func TestOwnerCreateDoesNotPrepareKeyForEitherNonfreshEdge(t *testing.T) {
	for _, edge := range []string{"account-without-key", "key-without-account"} {
		t.Run(edge, func(t *testing.T) {
			h := activationHub(t)
			if edge == "account-without-key" {
				consoleAccount(t, h, "existing member", "member")
			}
			prepared := false
			var key []byte
			if edge == "key-without-account" {
				key = bytes.Repeat([]byte{7}, 32)
			}
			keyAvailable := func() bool { return len(key) == 32 }
			err := h.ConfigureConsole(key, func() bool { return !keyAvailable() }, keyAvailable, func() error { prepared = true; return nil })
			if (err != nil) != keyAvailable() {
				t.Fatal("startup did not close the existing key without an operator", err)
			}
			w := createFreshOperator(t, h, "another operator", 200)
			result := consoleJSON(t, w)
			if result["account_created"] != true || result["key_prepared"] != false || result["restart_required"] != keyAvailable() || prepared || len(h.console.key) != 0 {
				t.Fatal("nonfresh create prepared a key or misreported its actions")
			}
			consoleDo(t, h, activationBase, "GET", "/v1/ui/login", nil, consoleBrowser{}, 401)
			if keyAvailable() {
				if err := h.ConfigureConsole(key, nil, keyAvailable, nil); err != nil {
					t.Fatal(err)
				}
				consoleSignIn(t, h, activationBase, "another operator")
				w = createFreshOperator(t, h, "additional operator", 200)
				result = consoleJSON(t, w)
				if result["key_prepared"] != false || result["restart_required"] != false || prepared {
					t.Fatal("open-console create claimed key preparation or another restart")
				}
			}
		})
	}
}

func TestFreshOwnerCreateKeyFailureLeavesRecoverableOperator(t *testing.T) {
	for _, failure := range []string{"key-error", "unavailable"} {
		t.Run(failure, func(t *testing.T) {
			h := activationHub(t)
			h.consoleKeyMissing = func() bool { return true }
			h.console.verify = consoleauth.VerifyPassword
			h.consoleActivate = func() error { return errors.New("private key diagnostic") }
			if failure == "unavailable" {
				h.consoleActivate = nil
			}
			w := createFreshOperator(t, h, "recoverable operator", 400)
			if !strings.Contains(w.Body.String(), "operator created;") || !strings.Contains(w.Body.String(), "use activate to retry") || strings.Contains(w.Body.String(), "private key diagnostic") || len(h.console.key) != 0 {
				t.Fatal("post-commit failure hid the created operator or exposed diagnostics")
			}
			a, err := h.store.LookupConsoleAccount(t.Context(), "recoverable operator")
			if err != nil || !a.Enabled() || a.Role != "operator" {
				t.Fatal("key failure lost the committed operator", err)
			}
			prepared := false
			h.consoleActivate = func() error { prepared = true; return nil }
			w = activationRequest(t, h, activationPayload(a.Username, consoleTestPassword), 200, nil)
			if !prepared || consoleJSON(t, w)["restart_required"] != true {
				t.Fatal("activate did not recover key preparation")
			}
			if err = h.ConfigureConsole(bytes.Repeat([]byte{8}, 32), nil, nil, nil); err != nil {
				t.Fatal(err)
			}
			consoleSignIn(t, h, activationBase, a.Username)
		})
	}
}
