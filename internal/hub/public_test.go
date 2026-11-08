package hub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestPublicLogin(t *testing.T) {
	for _, public := range []bool{false, true} {
		t.Run(map[bool]string{false: "local", true: "public"}[public], func(t *testing.T) {
			h, _ := setup(t)
			h.hosts["iris.klatt.ie"] = true // an allowed Host alone never opts into proxy trust
			now := time.Now()
			h.now = func() time.Time { return now }
			if public {
				if err := h.SetPublicURL("https://iris.klatt.ie"); err != nil {
					t.Fatal(err)
				}
			}
			call := func(method, host, path, proto string, bearer bool, cookie *http.Cookie) *httptest.ResponseRecorder {
				r := httptest.NewRequest(method, "http://"+host+path, nil)
				r.RemoteAddr = "127.0.0.1:1234"
				r.Header.Set("X-Forwarded-Proto", proto)
				if bearer {
					r.Header.Set("Authorization", "Bearer test-secret")
				}
				if cookie != nil {
					r.AddCookie(cookie)
				}
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				return w
			}
			if w := call("POST", "iris.klatt.ie", "/v1/ui/links", "https", false, nil); w.Code != 401 {
				t.Fatal(w.Code)
			}
			w := call("POST", "iris.klatt.ie", "/v1/ui/links", "https", true, nil)
			if !public {
				if w.Code != 401 {
					t.Fatal("forwarded header enabled login", w.Code)
				}
				return
			}
			if w.Code != 200 {
				t.Fatal(w.Code, w.Body.String())
			}
			h.mu.Lock()
			var code string
			for k := range h.links {
				code = k
			}
			h.mu.Unlock()
			path := "/ui/login?code=" + url.QueryEscape(code)
			for _, tc := range []struct{ host, proto string }{{"iris.klatt.ie", ""}, {"iris.klatt.ie", "http"}, {"127.0.0.1:7878", "https"}, {"evil.example", "https"}} {
				if w := call("GET", tc.host, path, tc.proto, false, nil); w.Code != 401 {
					t.Fatal(tc, w.Code)
				}
			}
			w = call("GET", "iris.klatt.ie", path, "https", false, nil)
			if w.Code != 303 {
				t.Fatal(w.Code, w.Body.String())
			}
			c := w.Result().Cookies()[0]
			if !c.Secure || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode {
				t.Fatal(c)
			}
			if w := call("GET", "iris.klatt.ie", path, "https", false, nil); w.Code != 401 {
				t.Fatal("reused code", w.Code)
			}
			for _, proto := range []string{"", "http", "https"} {
				w := call("GET", "iris.klatt.ie", "/v1/ui/overview", proto, false, c)
				want := 401
				if proto == "https" {
					want = 200
				}
				if w.Code != want {
					t.Fatal(proto, w.Code)
				}
			}
			call("POST", "iris.klatt.ie", "/v1/ui/links", "https", true, nil)
			h.mu.Lock()
			for k := range h.links {
				code = k
			}
			h.mu.Unlock()
			now = now.Add(2 * time.Minute)
			if w := call("GET", "iris.klatt.ie", "/ui/login?code="+code, "https", false, nil); w.Code != 401 {
				t.Fatal("expired code", w.Code)
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

func TestPublicLinksOriginsAndTickets(t *testing.T) {
	h, s := setup(t)
	if err := h.SetPublicURL("https://iris.klatt.ie"); err != nil {
		t.Fatal(err)
	}
	a := register(t, s, 1)
	call := func(method, path, origin string, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://iris.klatt.ie"+path, strings.NewReader(body))
		r.RemoteAddr = "127.0.0.1:4321"
		r.Header.Set("Authorization", "Bearer test-secret")
		r.Header.Set("X-Forwarded-Proto", "https")
		r.Header.Set("Origin", origin)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	for _, origin := range []string{"https://iris.klatt.ie", "http://iris.klatt.ie", "https://evil.example"} {
		w := call("POST", "/v1/ui/links", origin, "{}")
		if origin == "https://iris.klatt.ie" {
			if w.Code != 200 || !strings.Contains(w.Body.String(), "https://iris.klatt.ie/ui/login?code=") {
				t.Fatal(w.Code, w.Body.String())
			}
		} else if w.Code != 401 {
			t.Fatal(origin, w.Code)
		}
	}
	raw, _ := json.Marshal(authority(a))
	w := call("POST", "/v1/wake-tickets", "https://iris.klatt.ie", string(raw))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "wss://iris.klatt.ie/v1/watch?") {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestLocalLoginExpiryAndForwarding(t *testing.T) {
	h, s := setup(t)
	now := time.Now()
	h.now = func() time.Time { return now }
	link := request(t, s, "POST", "/v1/ui/links", map[string]any{}, 200)["url"].(string)
	r := httptest.NewRequest("GET", link, nil)
	r.RemoteAddr = "127.0.0.1:4321"
	r.Header.Set("X-Forwarded-Proto", "https")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 303 || w.Result().Cookies()[0].Secure {
		t.Fatal("forwarded header changed local cookie", w.Code, w.Header())
	}
	link = request(t, s, "POST", "/v1/ui/links", map[string]any{}, 200)["url"].(string)
	now = now.Add(2 * time.Minute)
	r = httptest.NewRequest("GET", link, nil)
	r.RemoteAddr = "127.0.0.1:4321"
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal("expired local code accepted", w.Code)
	}
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
