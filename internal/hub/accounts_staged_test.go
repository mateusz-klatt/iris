package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mateusz-klatt/iris/internal/store"
)

// Account storage alone never opens the browser namespace or derives a person
// from a machine bearer or an obsolete shared cookie.
func TestAccountStorageLeavesUnconfiguredConsoleClosed(t *testing.T) {
	h, server := setup(t)
	consoleAccount(t, h, "Stored operator", "operator")
	real := h.call
	calls := 0
	h.call = func(ctx context.Context, op string, r store.Request) (map[string]any, error) {
		calls++
		return real(ctx, op, r)
	}
	for _, credential := range []string{"none", "owner", "legacy-cookie"} {
		for _, route := range []struct{ method, path string }{
			{http.MethodGet, "/v1/ui/login"},
			{http.MethodPost, "/v1/ui/login"},
			{http.MethodGet, "/v1/ui/session"},
			{http.MethodGet, "/v1/ui/overview"},
			{http.MethodGet, "/v1/ui/watch"},
			{http.MethodPost, "/v1/ui/logout"},
			{http.MethodPatch, "/v1/ui/profile"},
			{http.MethodPost, "/v1/ui/password"},
			{http.MethodGet, "/v1/ui/accounts"},
			{http.MethodPut, "/v1/ui/accounts/account/access"},
			{http.MethodGet, "/v1/ui/messages/message/status"},
		} {
			r := httptest.NewRequest(route.method, server.URL+route.path, strings.NewReader("{}"))
			r.RemoteAddr = "127.0.0.1:1234"
			r.Header.Set("Origin", server.URL)
			want, code := http.StatusUnauthorized, "unauthorized"
			if credential == "owner" {
				r.Header.Set("Authorization", "Bearer test-secret")
			} else if credential == "legacy-cookie" {
				r.AddCookie(&http.Cookie{Name: "iris_session", Value: "obsolete"})
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			var out struct{ Error struct{ Code string } }
			if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
				t.Fatal(err)
			}
			if w.Code != want || out.Error.Code != code || w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("X-Iris-Console-Mode") != "closed" {
				t.Fatalf("%s %s %s reached closed console: %d %s", credential, route.method, route.path, w.Code, w.Body.String())
			}
		}
	}
	if calls != 0 {
		t.Fatal("closed browser requests reached a domain operation", calls)
	}
}
