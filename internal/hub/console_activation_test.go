package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mateusz-klatt/iris/internal/consoleauth"
	"github.com/mateusz-klatt/iris/internal/store"
)

const activationBase = "http://127.0.0.1:7878"

func activationHub(t *testing.T) *Hub {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "activation.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	h := New(s, "test-secret", []string{"127.0.0.1:7878"})
	h.consoleActivate = func() error { return nil }
	h.console.verify = func(password, hash string) (bool, error) {
		return password == consoleTestPassword && hash == dummyConsoleHash, nil
	}
	return h
}

func activationRequest(t *testing.T, h *Hub, payload string, status int, change func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, activationBase+"/v1/admin/console/activate", strings.NewReader(payload))
	r.Header.Set("Authorization", "Bearer test-secret")
	r.RemoteAddr = "127.0.0.1:1234"
	if change != nil {
		change(r)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != status {
		t.Fatalf("activation status %d, want %d: %s", w.Code, status, w.Body.String())
	}
	if len(w.Result().Cookies()) != 0 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("owner activation issued browser authority or a cacheable response")
	}
	return w
}

func activationPayload(username, password string) string {
	raw, _ := json.Marshal(map[string]string{"username": username, "password": password})
	return string(raw)
}

func TestConsoleStartupRequiresKeyAndEnabledOperator(t *testing.T) {
	h := activationHub(t)
	key := bytes.Repeat([]byte{7}, 32)
	for _, input := range [][]byte{nil, {1}, key} {
		err := h.ConfigureConsole(input, nil, nil, func() error { return nil })
		if (len(input) != 0) != (err != nil) {
			t.Fatalf("startup gate error for %d key bytes: %v", len(input), err)
		}
		consoleDo(t, h, activationBase, "GET", "/v1/ui/session", nil, consoleBrowser{}, 401)
	}
	consoleAccount(t, h, "Bootstrap", "operator")
	if err := h.ConfigureConsole(key, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	key[0]++
	if h.console.key[0] == key[0] {
		t.Fatal("startup retained caller-owned key memory")
	}
	consoleDo(t, h, activationBase, "GET", "/v1/ui/login", nil, consoleBrowser{}, 200)
	if err := h.store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := h.ConfigureConsole(key, nil, nil, nil); err == nil || len(h.console.key) != 0 {
		t.Fatal("failed operator readiness opened the console")
	}
}

func TestOwnerActivationVerifiesThenPersistsKeyWithoutOpeningRunningConsole(t *testing.T) {
	h := activationHub(t)
	a := consoleAccount(t, h, "Bootstrap", "operator")
	created := 0
	h.consoleActivate = func() error { created++; return nil }
	// A store call from inside verification proves the expensive work holds no
	// transaction mutex. Activation still rechecks the snapshot afterward.
	h.console.verify = func(password, hash string) (bool, error) {
		ready, err := h.store.HasEnabledConsoleOperator(context.Background())
		return ready && password == consoleTestPassword && hash == dummyConsoleHash, err
	}
	w := activationRequest(t, h, activationPayload(a.Username, consoleTestPassword), 200, nil)
	result := consoleJSON(t, w)
	if created != 1 || result["restart_required"] != true || result["account"].(map[string]any)["id"] != a.ID || len(h.console.key) != 0 {
		t.Fatal("activation omitted restart or changed the running gate")
	}
	if strings.Contains(w.Body.String(), a.PasswordHash) || strings.Contains(w.Body.String(), "password_hash") {
		t.Fatal("activation exposed credential material")
	}
	consoleDo(t, h, activationBase, "GET", "/v1/ui/session", nil, consoleBrowser{}, 401)
	// A fresh startup with the persisted key and same account opens named login.
	if err := h.ConfigureConsole(bytes.Repeat([]byte{8}, 32), nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	activationRequest(t, h, activationPayload(a.Username, consoleTestPassword), 409, nil)
}

func TestOwnerActivationAcceptsShortArgonPassword(t *testing.T) {
	h := activationHub(t)
	a, err := h.store.CreateConsoleAccount(t.Context(), store.ConsoleAccountInput{Username: "Restored", PasswordHash: shortConsoleHash, Role: "operator"})
	if err != nil {
		t.Fatal(err)
	}
	h.console.verify = consoleauth.VerifyPassword
	activationRequest(t, h, activationPayload(a.Username, "x"), 200, nil)
	after, err := h.store.GetConsoleAccount(t.Context(), a.ID)
	if err != nil || after.PasswordHash != shortConsoleHash || after.AuthVersion != a.AuthVersion {
		t.Fatal("activation changed a retained short Argon2id password")
	}
}

func TestOwnerActivationRefusesUnreadableHashWithRecoveryAdvice(t *testing.T) {
	for _, hash := range []string{legacyConsoleHash, "unreadable-sensitive-record"} {
		h, server, filename := consoleSetup(t)
		a := consoleAccount(t, h, "Restored", "operator")
		consoleDB(t, filename, "UPDATE ConsoleAccount SET password_hash=? WHERE id=?", hash, a.ID)
		h.console = newConsoleAuth(nil)
		created, calls := false, 0
		h.consoleActivate = func() error { created = true; return nil }
		h.console.verify = func(password, record string) (bool, error) {
			calls++
			if record != dummyConsoleHash {
				t.Fatal("unreadable activation hash reached password work")
			}
			return consoleauth.VerifyPassword(password, record)
		}
		w := ownerAccountRequest(t, h, server, "/v1/admin/console/activate", activationPayload(a.Username, "x"), "test-secret", 400)
		if len(w.Result().Cookies()) != 0 || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("refused owner activation issued browser authority or a cacheable response")
		}
		result := consoleJSON(t, w)["error"].(map[string]any)
		if result["code"] != "bad_request" || result["message"] != "this version does not read the stored password format; recover the operator with iris admin console reset <username>, then activate" {
			t.Fatal("unreadable operator did not receive the fixed owner recovery advice")
		}
		after, err := h.store.GetConsoleAccount(t.Context(), a.ID)
		if err != nil || after.PasswordHash != hash || after.AuthVersion != a.AuthVersion || after.EditRevision != a.EditRevision || created || calls != 1 || len(h.console.key) != 0 {
			t.Fatal("refused activation changed credentials, key or password work")
		}
	}
}

func TestOwnerActivationRejectsInvalidAuthorityAndInputBeforeKeyCreation(t *testing.T) {
	for _, test := range []struct {
		name, payload string
		status        int
		change        func(*http.Request)
	}{
		{"anonymous", activationPayload("Bootstrap", consoleTestPassword), 401, func(r *http.Request) { r.Header.Del("Authorization") }},
		{"host", activationPayload("Bootstrap", consoleTestPassword), 403, func(r *http.Request) { r.Header.Set("Authorization", "Bearer host-secret") }},
		{"duplicate", activationPayload("Bootstrap", consoleTestPassword), 403, func(r *http.Request) { r.Header.Add("Authorization", "Bearer test-secret") }},
		{"cookie", activationPayload("Bootstrap", consoleTestPassword), 403, func(r *http.Request) { r.Header.Set("Cookie", "iris_session=old") }},
		{"malformed", "{", 400, nil},
		{"oversized", activationPayload("Bootstrap", strings.Repeat("x", 1025)), 400, nil},
		{"bad-name", activationPayload(" invalid name ", consoleTestPassword), 400, nil},
		{"unknown", activationPayload("Unknown", consoleTestPassword), 401, nil},
		{"password", activationPayload("Bootstrap", "incorrect"), 401, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := activationHub(t)
			consoleAccount(t, h, "Bootstrap", "operator")
			if _, err := h.store.AddToken(context.Background(), "host"); err != nil {
				t.Fatal(err)
			}
			if test.name == "host" {
				token, err := h.store.AddToken(context.Background(), "real-host")
				if err != nil {
					t.Fatal(err)
				}
				test.change = func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+token) }
			}
			created := false
			h.consoleActivate = func() error { created = true; return nil }
			activationRequest(t, h, test.payload, test.status, test.change)
			if created || len(h.console.key) != 0 {
				t.Fatal("refused activation reached key creation")
			}
		})
	}
}

func TestActivationFailureAndVerificationRacesKeepTheConsoleClosed(t *testing.T) {
	for _, reason := range []string{"unconfigured", "lookup", "busy", "verify-error", "reset", "key-error", "rate", "member", "disabled"} {
		t.Run(reason, func(t *testing.T) {
			h := activationHub(t)
			role := "operator"
			if reason == "member" {
				role = "member"
			}
			a := consoleAccount(t, h, "Bootstrap", role)
			want, created := 401, false
			h.consoleActivate = func() error { created = true; return nil }
			switch reason {
			case "unconfigured":
				h.consoleActivate, want = nil, 400
			case "lookup":
				_ = h.store.Close()
				want = 400
			case "busy":
				h.console.verify = func(string, string) (bool, error) { return false, consoleauth.ErrBusy }
				want = 429
			case "verify-error":
				h.console.verify = func(string, string) (bool, error) { return false, errors.New("synthetic secret diagnostic") }
			case "reset":
				h.console.verify = func(string, string) (bool, error) {
					_, err := h.store.RecoverConsoleOperator(context.Background(), a, dummyConsoleHash)
					return true, err
				}
			case "key-error":
				h.consoleActivate = func() error { created = true; return errors.New("synthetic secret diagnostic") }
				want = 400
			case "rate":
				for range 5 {
					activationRequest(t, h, activationPayload(a.Username, "wrong"), 401, nil)
				}
				want = 429
			case "disabled":
				if _, err := h.store.ReplaceConsoleAccess(context.Background(), a.ID, a.EditRevision, "operator", true, nil); err != nil {
					t.Fatal(err)
				}
			}
			w := activationRequest(t, h, activationPayload(a.Username, consoleTestPassword), want, nil)
			if len(h.console.key) != 0 || strings.Contains(w.Body.String(), "synthetic secret diagnostic") || created != (reason == "key-error") {
				t.Fatal("failed activation changed authority or exposed private diagnostics")
			}
			if want == 429 && w.Header().Get("Retry-After") == "" {
				t.Fatal("bounded password refusal omitted retry guidance")
			}
		})
	}
}

func TestClosedConsoleStillValidatesAndReportsMachineUploadFailures(t *testing.T) {
	h := activationHub(t)
	calls := 0
	h.call = func(context.Context, string, store.Request) (map[string]any, error) {
		calls++
		return nil, errors.New("private storage diagnostic")
	}
	for _, body := range []string{"{", "{}"} {
		r := httptest.NewRequest(http.MethodPut, activationBase+"/v1/ui/doctor", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer test-secret")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 400 || strings.Contains(w.Body.String(), "private storage diagnostic") {
			t.Fatal("machine upload bypassed input validation or exposed storage diagnostics")
		}
	}
	if calls != 1 {
		t.Fatal("malformed machine upload reached the store")
	}
}
