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

// ADR 014 stages storage before authorization and authentication. A schema upgrade
// must leave the existing console usable without exposing unfinished account APIs.
func TestAccountStorageDoesNotActivateAccountRoutes(t *testing.T) {
	h, server := setup(t)
	link := request(t, server, http.MethodPost, "/v1/ui/links", map[string]any{}, http.StatusOK)["url"].(string)
	login := httptest.NewRequest(http.MethodGet, link, nil)
	login.RemoteAddr = "127.0.0.1:1234"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, login)
	if w.Code != http.StatusSeeOther || len(w.Result().Cookies()) != 1 {
		t.Fatal("legacy login unavailable", w.Code)
	}
	cookie := w.Result().Cookies()[0]
	if !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode {
		t.Fatal("legacy cookie protections changed")
	}

	call := h.call
	calls := 0
	h.call = func(ctx context.Context, op string, r store.Request) (map[string]any, error) {
		calls++
		return call(ctx, op, r)
	}
	for _, credential := range []string{"owner", "cookie"} {
		for _, route := range []struct{ method, path string }{
			{http.MethodGet, "/v1/ui/login"},
			{http.MethodPost, "/v1/ui/login"},
			{http.MethodGet, "/v1/ui/session"},
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
			if credential == "owner" {
				r.Header.Set("Authorization", "Bearer test-secret")
			} else {
				r.AddCookie(cookie)
			}
			w = httptest.NewRecorder()
			h.ServeHTTP(w, r)
			var out struct {
				Error struct{ Code string }
			}
			if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
				t.Fatal(err)
			}
			if w.Code != http.StatusNotFound || out.Error.Code != "not_found" || w.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("%s %s %s activated: %d %s", credential, route.method, route.path, w.Code, w.Body.String())
			}
		}
	}
	if calls != 0 || len(h.links) != 0 || len(h.cookies) != 1 {
		t.Fatal("inactive account route changed console state", calls)
	}

	r := httptest.NewRequest(http.MethodGet, server.URL+"/v1/ui/overview", nil)
	r.RemoteAddr = "127.0.0.1:1234"
	r.AddCookie(cookie)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK || calls != 1 || !strings.Contains(w.Body.String(), `"human_unread":0`) {
		t.Fatal("legacy console no longer works", w.Code, w.Body.String())
	}
}
