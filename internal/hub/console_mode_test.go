package hub

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/mateusz-klatt/iris/internal/store"
)

func TestClosedConsoleDiscoveryRejectsBearerAuthentication(t *testing.T) {
	h, server := setup(t)
	r := httptest.NewRequest(http.MethodGet, server.URL+"/v1/ui/session", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized || w.Header().Get("X-Iris-Console-Mode") != "closed" {
		t.Fatal("unsigned browser cannot discover the closed console", w.Code, w.Header())
	}
	r.Header.Set("Authorization", "Bearer test-secret")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized || w.Header().Get("X-Iris-Console-Mode") != "closed" {
		t.Fatal("machine bearer opened the closed console", w.Code, w.Header())
	}
	named, namedServer, _ := consoleSetup(t)
	w = consoleDo(t, named, namedServer.URL, http.MethodGet, "/v1/ui/session", nil, consoleBrowser{}, http.StatusUnauthorized)
	if w.Header().Get("X-Iris-Console-Mode") != "" {
		t.Fatal("named account browser was reported as closed")
	}
	r = httptest.NewRequest(http.MethodGet, namedServer.URL+"/v1/ui/session", nil)
	r.RemoteAddr = "127.0.0.1:1234"
	r.Header.Set("Authorization", "Bearer test-secret")
	w = httptest.NewRecorder()
	named.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden || w.Header().Get("X-Iris-Console-Mode") != "" {
		t.Fatal("machine bearer selected active browser authority", w.Code, w.Header())
	}
}

func TestRemovedConsoleImportRoutesRefuseWithoutStoreSideEffects(t *testing.T) {
	for _, open := range []bool{false, true} {
		h := activationHub(t)
		consoleAccount(t, h, "Synthetic", "operator")
		if open {
			if err := h.ConfigureConsole(bytes.Repeat([]byte{7}, 32), nil, nil, nil); err != nil {
				t.Fatal(err)
			}
		}
		before, changed := h.store.TransactionCount(), h.store.Changes()
		for _, path := range []string{"/v1/admin/console/import", "/v1/admin/console/import-grants"} {
			for _, method := range []string{http.MethodGet, http.MethodPost} {
				r := httptest.NewRequest(method, activationBase+path, strings.NewReader(`{"dry_run":false,"batch":{},"transfer":{}}`))
				r.Header.Set("Authorization", "Bearer test-secret")
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), `"code":"not_found"`) || len(w.Result().Cookies()) != 0 || w.Header().Get("Cache-Control") != "no-store" {
					t.Fatal("removed route accepted a request or returned browser authority", method, path, w.Code)
				}
			}
		}
		if after := h.store.TransactionCount(); after != before {
			t.Fatal("removed route reached the store", before, after)
		}
		select {
		case <-changed:
			t.Fatal("removed route published a persisted change")
		default:
		}
	}
}

func TestClosedConsoleAcceptsAndPersistsMachineDoctorUpload(t *testing.T) {
	h := activationHub(t)
	clients := []map[string]any{{"client": "codex", "status": "needs_human"}}
	r := httptest.NewRequest(http.MethodPut, activationBase+"/v1/ui/doctor", strings.NewReader(`{"host":"synthetic-host","checked_at":"2026-11-11T00:00:00Z","clients":[{"client":"codex","status":"needs_human"}]}`))
	r.Header.Set("Authorization", "Bearer test-secret")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK || len(w.Result().Cookies()) != 0 || len(h.console.key) != 0 {
		t.Fatal("machine upload failed or opened browser authentication", w.Code)
	}
	report, err := h.store.Call(t.Context(), "ui_doctor_get", store.Request{})
	want := []map[string]any{{"host": "synthetic-host", "checked_at": "2026-11-11T00:00:00Z", "clients": []any{clients[0]}}}
	if err != nil || !reflect.DeepEqual(report["hosts"], want) {
		t.Fatal("closed-console upload did not persist the machine report", report, err)
	}
	w = consoleDo(t, h, activationBase, http.MethodGet, "/v1/ui/session", nil, consoleBrowser{}, http.StatusUnauthorized)
	if w.Header().Get("X-Iris-Console-Mode") != "closed" {
		t.Fatal("machine upload changed the closed console gate")
	}
}

func TestAnOpenConsoleTransportRefusalDoesNotReportClosedActivation(t *testing.T) {
	h, server, _ := consoleSetup(t)
	r := httptest.NewRequest(http.MethodGet, server.URL+"/v1/ui/session", nil)
	r.RemoteAddr = "192.0.2.10:1234"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized || w.Header().Get("X-Iris-Console-Mode") != "" {
		t.Fatal("a refused request changed the reported activation state", w.Code, w.Header())
	}
}
