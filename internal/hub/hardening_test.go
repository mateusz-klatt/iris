package hub

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mateusz-klatt/iris/internal/buildinfo"
)

func TestStatusAndBodyLimits(t *testing.T) {
	h, s := setup(t)
	for _, token := range []string{"", "wrong", "test-secret"} {
		r := httptest.NewRequest("GET", s.URL+"/v1/status", nil)
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatal(w.Code)
		}
		if token != "test-secret" && strings.TrimSpace(w.Body.String()) != `{"status":"ok"}` {
			t.Fatal(w.Body.String())
		}
		if token == "test-secret" && !strings.Contains(w.Body.String(), "counts") {
			t.Fatal(w.Body.String())
		}
		if token == "test-secret" {
			var status map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil || status["version"] != buildinfo.String() {
				t.Fatalf("authenticated build identity: %s (%v)", w.Body.String(), err)
			}
		}
		if w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal(w.Header())
		}
	}
	for _, path := range []string{"/v1/status", "/v1/ui/links", "/ui/login", "/v1/watch", "/v1/ui/watch", "/v1/sessions", "/"} {
		for _, chunked := range []bool{false, true} {
			r := httptest.NewRequest("POST", s.URL+path, strings.NewReader(strings.Repeat(" ", 64<<10+1)))
			if chunked {
				r.ContentLength = -1
			}
			r.Header.Set("Authorization", "Bearer test-secret")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != 413 {
				t.Fatal(path, chunked, w.Code, w.Body.String())
			}
		}
	}
}

func TestAuthenticationLogRedactsCredentials(t *testing.T) {
	h, s := setup(t)
	var output bytes.Buffer
	before := log.Writer()
	log.SetOutput(&output)
	defer log.SetOutput(before)
	r := httptest.NewRequest("GET", s.URL+"/v1/agents", nil)
	r.Header.Set("Authorization", "Bearer do-not-log-this-token")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 401 || !strings.Contains(output.String(), "authentication failed") || strings.Contains(output.String(), "do-not-log-this-token") {
		t.Fatal(w.Code, output.String())
	}
	r = httptest.NewRequest("GET", s.URL+"/", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	csp := w.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "script-src 'self'") || !strings.Contains(csp, "frame-ancestors 'none'") || strings.Contains(csp, "unsafe-inline") {
		t.Fatal(csp)
	}
}
