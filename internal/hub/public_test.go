package hub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestPublicLogin(t *testing.T) {
	for _, public := range []bool{false, true} {
		t.Run(map[bool]string{false: "local", true: "public"}[public], func(t *testing.T) {
			h, server, _ := consoleSetup(t)
			consoleAccount(t, h, "Public", "operator")
			h.hosts["iris.klatt.ie"] = true // an allowed Host alone never opts into proxy trust
			now := time.Now()
			h.now = func() time.Time { return now }
			if public {
				if err := h.SetPublicURL("https://iris.klatt.ie"); err != nil {
					t.Fatal(err)
				}
			}
			call := func(method, host, path, proto, remote string, browser consoleBrowser) *httptest.ResponseRecorder {
				r := httptest.NewRequest(method, "http://"+host+path, strings.NewReader(`{"username":"Public","password":"`+consoleTestPassword+`"}`))
				r.RemoteAddr = remote
				r.Header.Set("X-Forwarded-Proto", proto)
				if method != http.MethodGet {
					r.Header.Set("Origin", "https://"+host)
				}
				if browser.cookie != nil {
					r.AddCookie(browser.cookie)
				}
				if browser.csrf != "" {
					r.Header.Set("X-Iris-CSRF", browser.csrf)
				}
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				return w
			}
			w := call("GET", "iris.klatt.ie", "/v1/ui/login", "https", "127.0.0.1:1234", consoleBrowser{})
			if !public {
				if w.Code != 401 {
					t.Fatal("forwarded header enabled login", w.Code)
				}
				return
			}
			if w.Code != 200 {
				t.Fatal(w.Code, w.Body.String())
			}
			challenge := consoleBrowser{cookie: w.Result().Cookies()[0], csrf: consoleJSON(t, w)["csrf"].(string)}
			for _, tc := range []struct{ host, proto, remote string }{
				{"iris.klatt.ie", "", "127.0.0.1:1234"},
				{"iris.klatt.ie", "http", "127.0.0.1:1234"},
				{strings.TrimPrefix(server.URL, "http://"), "https", "127.0.0.1:1234"},
				{"evil.example", "https", "127.0.0.1:1234"},
				{"iris.klatt.ie", "https", "198.51.100.1:1234"},
			} {
				if w := call("GET", tc.host, "/v1/ui/login", tc.proto, tc.remote, consoleBrowser{}); w.Code != 401 {
					t.Fatal(tc, w.Code)
				}
			}
			w = call("POST", "iris.klatt.ie", "/v1/ui/login", "https", "127.0.0.1:1234", challenge)
			if w.Code != 200 {
				t.Fatal(w.Code, w.Body.String())
			}
			var c *http.Cookie
			for _, cookie := range w.Result().Cookies() {
				if cookie.Name == consoleCookieName {
					c = cookie
				}
			}
			if c == nil || !c.Secure || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode {
				t.Fatal(c)
			}
			if w := call("POST", "iris.klatt.ie", "/v1/ui/login", "https", "127.0.0.1:1234", challenge); w.Code != 403 {
				t.Fatal("reused challenge", w.Code)
			}
			for _, proto := range []string{"", "http", "https"} {
				w := call("GET", "iris.klatt.ie", "/v1/ui/session", proto, "127.0.0.1:1234", consoleBrowser{cookie: c})
				want := 401
				if proto == "https" {
					want = 200
				}
				if w.Code != want {
					t.Fatal(proto, w.Code)
				}
			}
			w = call("GET", "iris.klatt.ie", "/v1/ui/login", "https", "127.0.0.1:1234", consoleBrowser{})
			if w.Code != 200 {
				t.Fatal(w.Code, w.Body.String())
			}
			challenge = consoleBrowser{cookie: w.Result().Cookies()[0], csrf: consoleJSON(t, w)["csrf"].(string)}
			now = now.Add(2 * time.Minute)
			if w := call("POST", "iris.klatt.ie", "/v1/ui/login", "https", "127.0.0.1:1234", challenge); w.Code != 403 {
				t.Fatal("expired challenge", w.Code)
			}
		})
	}
}

func TestPublicURLValidation(t *testing.T) {
	for _, raw := range []string{"http://iris.klatt.ie", "https://", "https://user@iris.klatt.ie", "https://iris.klatt.ie/path", "https://iris.klatt.ie?q=1", "https://iris.klatt.ie#f"} {
		if err := New(nil, "", nil).SetPublicURL(raw); err == nil {
			t.Fatal(raw)
		}
	}
}

func TestPublicConsoleOriginsAndTickets(t *testing.T) {
	h, s, _ := consoleSetup(t)
	consoleAccount(t, h, "Public", "operator")
	if err := h.SetPublicURL("https://iris.klatt.ie"); err != nil {
		t.Fatal(err)
	}
	a := register(t, s, 1)
	call := func(method, path, origin, body string, bearer bool, browser consoleBrowser) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://iris.klatt.ie"+path, strings.NewReader(body))
		r.RemoteAddr = "127.0.0.1:4321"
		if bearer {
			r.Header.Set("Authorization", "Bearer test-secret")
		}
		if browser.cookie != nil {
			r.AddCookie(browser.cookie)
		}
		if browser.csrf != "" {
			r.Header.Set("X-Iris-CSRF", browser.csrf)
		}
		r.Header.Set("X-Forwarded-Proto", "https")
		r.Header.Set("Origin", origin)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	w := call("GET", "/v1/ui/login", "https://iris.klatt.ie", "", false, consoleBrowser{})
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	challenge := consoleBrowser{cookie: w.Result().Cookies()[0], csrf: consoleJSON(t, w)["csrf"].(string)}
	for _, origin := range []string{"http://iris.klatt.ie", "https://evil.example", "https://iris.klatt.ie"} {
		w := call("POST", "/v1/ui/login", origin, `{"username":"Public","password":"`+consoleTestPassword+`"}`, false, challenge)
		if origin == "https://iris.klatt.ie" {
			if w.Code != 200 || !strings.Contains(w.Body.String(), `"authenticated":true`) {
				t.Fatal(w.Code, w.Body.String())
			}
		} else if w.Code != 401 {
			t.Fatal(origin, w.Code)
		}
	}
	raw, _ := json.Marshal(authority(a))
	w = call("POST", "/v1/wake-tickets", "https://iris.klatt.ie", string(raw), true, consoleBrowser{})
	if w.Code != 200 || !strings.Contains(w.Body.String(), "wss://iris.klatt.ie/v1/watch?") {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestLocalLoginExpiryAndForwarding(t *testing.T) {
	h, s, _ := consoleSetup(t)
	consoleAccount(t, h, "Local", "operator")
	now := time.Now()
	h.now = func() time.Time { return now }
	r := httptest.NewRequest("GET", s.URL+"/v1/ui/login", nil)
	r.RemoteAddr = "127.0.0.1:4321"
	r.Header.Set("X-Forwarded-Proto", "https")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 || w.Result().Cookies()[0].Secure {
		t.Fatal("forwarded header changed local cookie", w.Code, w.Header())
	}
	challenge := consoleBrowser{cookie: w.Result().Cookies()[0], csrf: consoleJSON(t, w)["csrf"].(string)}
	now = now.Add(2 * time.Minute)
	consoleDo(t, h, s.URL, "POST", "/v1/ui/login", map[string]any{"username": "Local", "password": consoleTestPassword}, challenge, 403)
}

// Browsers send a lower-case Host without the default port, whatever the flag said.
func TestPublicURLMatchesWhatBrowsersSend(t *testing.T) {
	for raw, want := range map[string]string{
		"https://iris.klatt.ie:443":  "iris.klatt.ie",
		"https://IRIS.Klatt.IE":      "iris.klatt.ie",
		"https://IRIS.klatt.ie:443/": "iris.klatt.ie",
		"https://iris.klatt.ie:8443": "iris.klatt.ie:8443",
		"https://[2001:DB8::1]:443":  "[2001:db8::1]",
	} {
		h := New(nil, "", nil)
		if err := h.SetPublicURL(raw); err != nil {
			t.Fatal(raw, err)
		}
		if h.publicURL.Host != want || !h.hosts[want] {
			t.Errorf("%s: %q %v", raw, h.publicURL.Host, h.hosts)
		}
		r := httptest.NewRequest(http.MethodGet, "http://"+want+"/", nil)
		r.RemoteAddr = "127.0.0.1:5000"
		r.Host = want
		r.Header.Set("X-Forwarded-Proto", "https")
		if !h.loginAllowed(r) {
			t.Errorf("%s: a browser request for %s was refused", raw, want)
		}
	}
}
