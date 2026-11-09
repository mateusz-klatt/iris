package hub

import (
	"net/http"
	"net/http/httptest"
	"testing"
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
