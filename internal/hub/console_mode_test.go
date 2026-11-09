package hub

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestStagedConsoleDiscoveryPreservesLegacyAuthentication(t *testing.T) {
	h, server := setup(t)
	r := httptest.NewRequest(http.MethodGet, server.URL+"/v1/ui/session", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized || w.Header().Get("X-Iris-Console-Mode") != "links" {
		t.Fatal("unsigned legacy browser cannot discover its existing sign-in mode", w.Code, w.Header())
	}
	r.Header.Set("Authorization", "Bearer test-secret")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusNotFound || w.Header().Get("X-Iris-Console-Mode") != "links" {
		t.Fatal("legacy session endpoint unexpectedly became active", w.Code, w.Header())
	}
	named, namedServer, _ := consoleSetup(t)
	w = consoleDo(t, named, namedServer.URL, http.MethodGet, "/v1/ui/session", nil, consoleBrowser{}, http.StatusUnauthorized)
	if w.Header().Get("X-Iris-Console-Mode") != "" {
		t.Fatal("named account browser was redirected to the legacy login path")
	}
}
