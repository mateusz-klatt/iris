package hub

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestConsoleReadsStayBoundToTheirMountedSession(t *testing.T) {
	h, server := setup(t)
	h.console = newConsoleAuth(make([]byte, 32))
	a, cookieA, csrfA := routeConsoleAccount(t, h, "First person", "operator")
	b, cookieB, csrfB := routeConsoleAccount(t, h, "Second person", "operator")

	for _, path := range []string{"/v1/ui/overview", "/v1/ui/session"} {
		t.Run(path, func(t *testing.T) {
			for _, headers := range [][]string{{csrfA}, {"incorrect"}, {""}, {csrfB, csrfB}} {
				r := httptest.NewRequest(http.MethodGet, server.URL+path, nil)
				r.RemoteAddr = "127.0.0.1:1234"
				r.AddCookie(cookieB)
				for _, value := range headers {
					r.Header.Add("X-Iris-CSRF", value)
				}
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				if w.Code != http.StatusUnauthorized {
					t.Fatalf("read with another or ambiguous session binding: got %d", w.Code)
				}
				out := consoleJSON(t, w)
				failure := out["error"].(map[string]any)
				if failure["code"] != "unauthorized" || failure["message"] != "console session changed" || len(out) != 1 {
					t.Fatal("session binding refusal exposed a data response", out)
				}
				if len(w.Result().Cookies()) != 0 {
					t.Fatal("read binding mismatch changed the shared browser cookie")
				}
			}
			consoleRoute(t, h, server, cookieB, csrfB, http.MethodGet, path, "", http.StatusOK)
			consoleDo(t, h, server.URL, http.MethodGet, path, nil, consoleBrowser{cookie: cookieB}, http.StatusOK)
		})
	}

	for _, session := range []struct {
		id     string
		cookie *http.Cookie
		csrf   string
	}{{a.ID, cookieA, csrfA}, {b.ID, cookieB, csrfB}} {
		out := consoleRoute(t, h, server, session.cookie, session.csrf, http.MethodGet, "/v1/ui/session", "", http.StatusOK)
		if out["account"].(map[string]any)["id"] != session.id {
			t.Fatal("read binding refusal revoked or switched an existing session")
		}
	}
	challenge := consoleDo(t, h, server.URL, http.MethodGet, "/v1/ui/login", nil, consoleBrowser{cookie: cookieB, csrf: csrfA}, http.StatusOK)
	cookies := challenge.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != consoleLoginCookieName || consoleJSON(t, challenge)["csrf"] == csrfA {
		t.Fatal("login challenge was bound to the mounted console session")
	}
}
