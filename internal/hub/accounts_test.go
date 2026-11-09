package hub

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mateusz-klatt/iris/internal/store"
)

func TestStagedConsoleCredentialsNeverSelectMachineAuthority(t *testing.T) {
	for _, test := range []struct {
		path, header string
		digest       []byte
		want         string
	}{
		{"/v1/ui/messages", "Bearer owner", nil, "forbidden"},
		{"/v1/ui/messages", "Bearer host", make([]byte, 32), "forbidden"},
		{"/v1/ui/overview", "invalid", make([]byte, 32), "forbidden"},
		{"/v1/ui/overview", "", nil, "unauthorized"},
		{"/v1/messages", "", make([]byte, 32), "forbidden"},
		{"/v1/tokens", "", make([]byte, 32), "forbidden"},
		{"/v1/admin/merge", "", make([]byte, 32), "forbidden"},
		{"/v1/ui/overview", "", make([]byte, 32), ""},
	} {
		r := httptest.NewRequest(http.MethodPost, test.path, nil)
		if test.header != "" {
			r.Header.Set("Authorization", test.header)
		}
		r.AddCookie(&http.Cookie{Name: "iris_session", Value: "synthetic"})
		p, err := consoleRequestPrincipal(r, test.digest)
		if test.want == "" {
			if err != nil || p.Kind != store.ConsolePrincipal || len(p.SessionDigest) != 32 {
				t.Fatal("verified console credential lost its type", err)
			}
		} else {
			var domain *store.Error
			if !errors.As(err, &domain) || domain.Code != test.want {
				t.Fatal("credential boundary selected the wrong authority", err)
			}
		}
	}
	r := httptest.NewRequest(http.MethodGet, "/v1/ui/overview", nil)
	r.Header.Add("Authorization", "")
	r.Header.Add("Authorization", "Bearer synthetic")
	if _, err := consoleRequestPrincipal(r, make([]byte, 32)); err == nil {
		t.Fatal("duplicate authorization headers bypassed mixed-credential refusal")
	}
}

func TestLegacyCookieDoesNotAuthorizeMachineRoutes(t *testing.T) {
	h, server := setup(t)
	for _, route := range []struct{ method, path string }{{"GET", "/v1/agents"}, {"GET", "/v1/tokens"}, {"POST", "/v1/admin/merge"}} {
		r := httptest.NewRequest(route.method, server.URL+route.path, nil)
		r.RemoteAddr = "127.0.0.1:1234"
		r.AddCookie(&http.Cookie{Name: "iris_session", Value: "synthetic"})
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Fatal("cookie reached machine route", route.path, w.Code)
		}
	}
}
