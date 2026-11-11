package hub

import (
	"bytes"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mateusz-klatt/iris/internal/config"
	"github.com/mateusz-klatt/iris/internal/consoleauth"
)

func TestFreshOwnerCreateAuditsOnlyAfterKeyPublication(t *testing.T) {
	for _, outcome := range []string{"success", "key-failure", "audit-failure"} {
		t.Run(outcome, func(t *testing.T) {
			h, server, filename := consoleSetup(t)
			db, err := sql.Open("sqlite", filename)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = db.Close() }()
			assertAudit := func(action string, want int) {
				t.Helper()
				var count int
				if err := db.QueryRow(`SELECT COUNT(*) FROM ConsoleAction WHERE actor_kind='owner' AND actor_account_id IS NULL AND target_account_id=(SELECT id FROM ConsoleAccount WHERE username='fresh operator') AND action=? AND outcome='accepted'`, action).Scan(&count); err != nil || count != want {
					t.Fatalf("%s audit count = %d, want %d: %v", action, count, want, err)
				}
			}
			if outcome == "audit-failure" {
				if _, err = db.Exec(`CREATE TRIGGER fail_prepared_audit BEFORE INSERT ON ConsoleAction WHEN NEW.action='activation_prepared' BEGIN SELECT RAISE(ABORT,'private audit diagnostic'); END`); err != nil {
					t.Fatal(err)
				}
			}
			keyPath := filepath.Join(t.TempDir(), "console.key")
			if err = h.ConfigureConsole(nil, func() bool { return true }, nil, func() error {
				// An independent connection sees the committed provisioning audit,
				// but no preparation record while the key still does not exist.
				assertAudit("owner_provision", 1)
				assertAudit("activation_prepared", 0)
				if _, err := os.Stat(keyPath); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("key existed before publication")
				}
				if outcome == "key-failure" {
					return errors.New("private key diagnostic")
				}
				_, err := config.ConsoleKey(keyPath)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			status, wantPrepared := http.StatusBadRequest, 0
			if outcome == "success" {
				status, wantPrepared = http.StatusOK, 1
			}
			w := ownerAccountRequest(t, h, server, "/v1/admin/console/accounts", activationPayload("fresh operator", consoleTestPassword), "test-secret", status)
			assertAudit("owner_provision", 1)
			assertAudit("activation_prepared", wantPrepared)
			if len(h.console.key) != 0 || strings.Contains(w.Body.String(), "private") {
				t.Fatal("create hot-opened the console or exposed private diagnostics")
			}
			if outcome == "success" {
				if result := consoleJSON(t, w); result["key_prepared"] != true || result["restart_required"] != true {
					t.Fatal("published and audited key was not reported")
				}
			} else if !strings.Contains(w.Body.String(), "operator created;") || strings.Contains(w.Body.String(), `"key_prepared":true`) {
				t.Fatal("failed preparation or audit reported success or hid the committed operator")
			}
			if outcome == "key-failure" {
				if _, err := os.Stat(keyPath); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("failed key creation left a key")
				}
			} else if key, err := config.LoadConsoleKey(keyPath); err != nil || len(key) != 32 {
				t.Fatal("successful publication did not retain a usable key", err)
			}
			if outcome == "audit-failure" && !strings.Contains(w.Body.String(), "console key prepared; activation audit could not be recorded; restart required") {
				t.Fatal("audit failure hid the published key and required restart")
			}
		})
	}
}

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
