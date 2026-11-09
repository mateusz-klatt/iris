package hub

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestConsoleJSONPreservesValidUnicodeAndEscapedBytes(t *testing.T) {
	for _, test := range []struct{ raw, want string }{
		{`{"password":"literal replacement � stays"}`, "literal replacement � stays"},
		{`{"password":"raw non-BMP 🚀 stays"}`, "raw non-BMP 🚀 stays"},
		{`{"password":"paired \uD83d\uDE80 stays"}`, "paired 🚀 stays"},
		{`{"password":"\uD800\uDC00\uDBFF\uDFFF"}`, "\U00010000\U0010ffff"},
		{`{"password":"literal \\ud800 stays"}`, `literal \ud800 stays`},
		{`{"password":"escaped \" and \\ stay"}`, `escaped " and \ stay`},
		{`{"password":"e\u0301 stays decomposed"}`, "e\u0301 stays decomposed"},
		{`{"password":"\u0000\u1234\uabcd"}`, "\x00\u1234\uabcd"},
	} {
		var got struct {
			Password string `json:"password"`
		}
		r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(test.raw))
		w := httptest.NewRecorder()
		if !consoleDecode(w, r, &got) || got.Password != test.want {
			t.Fatal("valid credential Unicode was rejected or transformed")
		}
	}
}

func TestConsoleJSONRejectsMalformedUnicodeWithoutReplacement(t *testing.T) {
	for _, raw := range []string{
		"{\"password\":\"invalid \xff UTF8\"}",
		"{\"username\":\"invalid \xed\xa0\x80 UTF8 surrogate\"}",
		`{"password":"\uDC00"}`,
		`{"password":"\uDFFF"}`,
		`{"password":"\uD800"}`,
		`{"password":"\uDBFF"}`,
		`{"password":"\uD800\u0061"}`,
		`{"password":"\uD800\uDBFF"}`,
		`{"password":"\uD800\uE000"}`,
		`{"password":"\uD800\uZZZZ"}`,
		`{"password":"\uD800no pair"}`,
		`{"password":"\uD800\\DC00"}`,
		`{"password":"\uZZZZ"}`,
		`{"password":"\u+001"}`,
		`{"password":"\`,
		`{"password":"\u`,
		`{"password":"\u1`,
		`{"password":"\u12`,
		`{"password":"\u123`,
		`{"password":"\uD800\u`,
		`{"password":"\uD800\u123`,
		`{"password":"\u1234`,
	} {
		var got struct {
			Password string `json:"password"`
		}
		r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(raw))
		w := httptest.NewRecorder()
		if consoleDecode(w, r, &got) || w.Code != http.StatusBadRequest || got.Password != "" {
			t.Fatal("malformed credential Unicode was accepted or replaced")
		}
	}
}

func TestConsoleJSONRetainsSyntaxAndReadFailureBoundaries(t *testing.T) {
	for _, raw := range []string{`{`, `{"unknown":1}`, `{"password":3}`, `{"password":"ok"} {}`} {
		var got struct {
			Password string `json:"password"`
		}
		r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(raw))
		w := httptest.NewRecorder()
		if consoleDecode(w, r, &got) || w.Code != http.StatusBadRequest {
			t.Fatal("invalid JSON credential request accepted")
		}
	}
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	r.Body = io.NopCloser(brokenReader{})
	w := httptest.NewRecorder()
	var got any
	if consoleDecode(w, r, &got) || w.Code != http.StatusBadRequest {
		t.Fatal("credential input read failure was accepted")
	}
}

func TestMalformedUnicodeCredentialsCannotReachKDFOrCreateAccount(t *testing.T) {
	h, server, _ := consoleSetup(t)
	_, cookie, csrf := routeConsoleAccount(t, h, "Existing", "member")
	work := 0
	h.console.verify = func(string, string) (bool, error) { work++; return true, nil }
	h.console.create = func(string) (string, error) { work++; return dummyConsoleHash, nil }
	original := createOwnerPassword
	t.Cleanup(func() { createOwnerPassword = original })
	createOwnerPassword = func(string) (string, error) { work++; return dummyConsoleHash, nil }
	for _, test := range []struct{ path, raw string }{
		{"/v1/ui/login", "{\"username\":\"Candidate\",\"password\":\"a long invalid \xff password\"}"},
		{"/v1/ui/login", `{"username":"Candidate","password":"a long invalid \ud800 password"}`},
		{"/v1/ui/password", `{"current_password":"\ud800","new_password":"a long new password"}`},
		{"/v1/ui/password", `{"current_password":"current password","new_password":"a long invalid \udfff password"}`},
		{"/v1/admin/console/accounts", "{\"username\":\"Candidate\",\"password\":\"a long invalid \xff password\"}"},
		{"/v1/admin/console/accounts", `{"username":"Candidate\ud800","password":"a long new password"}`},
		{"/v1/admin/console/accounts/reset", `{"username":"Existing","password":"a long invalid \udc00 password"}`},
	} {
		r := httptest.NewRequest(http.MethodPost, server.URL+test.path, strings.NewReader(test.raw))
		r.RemoteAddr = "127.0.0.1:1234"
		r.Header.Set("Origin", server.URL)
		switch test.path {
		case "/v1/ui/login":
			b := consoleChallengeBrowser(t, h, server.URL)
			r.AddCookie(b.cookie)
			r.Header.Set("X-Iris-CSRF", b.csrf)
		case "/v1/ui/password":
			r.AddCookie(cookie)
			r.Header.Set("X-Iris-CSRF", csrf)
		default:
			r.Header.Set("Authorization", "Bearer test-secret")
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusBadRequest || work != 0 {
			t.Fatal("malformed Unicode credentials reached password work")
		}
		for _, c := range w.Result().Cookies() {
			if c.Name == consoleCookieName && c.Value != "" {
				t.Fatal("malformed credentials issued a session")
			}
		}
	}
	if _, err := h.store.LookupConsoleAccount(t.Context(), "Candidate"); err == nil {
		t.Fatal("malformed credentials created a named account")
	}
	consoleRoute(t, h, server, cookie, csrf, http.MethodGet, "/v1/ui/session", "", http.StatusOK)
}
