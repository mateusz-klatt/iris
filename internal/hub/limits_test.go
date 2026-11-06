package hub

import (
	"context"
	"flag"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestRequestQuotas(t *testing.T) {
	h, s := setup(t)
	h.Limits = Limits{TokenPerMinute: 2, AddressPerMinute: 20}
	request(t, s, "GET", "/v1/status", nil, 200)
	request(t, s, "GET", "/v1/status", nil, 200)
	out := request(t, s, "GET", "/v1/status", nil, 429)
	if out["error"].(map[string]any)["code"] != "rate_limited" {
		t.Fatal(out)
	}
	// Anonymous requests share a bucket regardless of source port or proxy headers.
	for i := 0; i < 2; i++ {
		r := httptest.NewRequest("GET", "http://localhost/v1/status", nil)
		r.RemoteAddr = "192.0.2.1:" + []string{"1000", "2000"}[i]
		r.Host = s.Listener.Addr().String()
		r.Header.Set("X-Forwarded-For", []string{"192.0.2.2", "192.0.2.3"}[i])
		h.Limits.AddressPerMinute = 1
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		want := 200
		if i == 1 {
			want = 429
			if w.Header().Get("Retry-After") == "" {
				t.Fatal("missing Retry-After")
			}
		}
		if w.Code != want {
			t.Fatal(w.Code, w.Body.String())
		}
	}
}
func TestQuotaWindowAndCapacity(t *testing.T) {
	h, _ := setup(t)
	now := time.Now()
	h.now = func() time.Time { return now }
	w := httptest.NewRecorder()
	if !h.allowRate(w, "one", 1) || h.allowRate(w, "one", 1) || w.Code != 429 {
		t.Fatal(w.Code)
	}
	now = now.Add(time.Minute)
	if !h.allowRate(httptest.NewRecorder(), "one", 1) {
		t.Fatal("window did not reset")
	}
	for i := 0; i < 10000; i++ {
		h.rates[string(rune(i))] = window{start: now, count: 1}
	}
	if h.allowRate(httptest.NewRecorder(), "new", 1) {
		t.Fatal("unbounded rate state")
	}
	now = now.Add(time.Minute)
	if !h.allowRate(httptest.NewRecorder(), "new", 1) || len(h.rates) != 1 {
		t.Fatal("expired entries not reclaimed", len(h.rates))
	}
	f := flag.NewFlagSet("limits", flag.ContinueOnError)
	limits := RateLimitFlags(f)
	if err := f.Parse([]string{"--token-rate=123", "--address-rate=234", "--token-websockets=3"}); err != nil || *limits != (Limits{123, 234, 3}) || limits.Validate() != nil {
		t.Fatal(limits, err)
	}
	if (Limits{TokenPerMinute: -1}).Validate() == nil {
		t.Fatal("negative limit accepted")
	}
}
func TestWebSocketQuotaAndFailedUpgradeRelease(t *testing.T) {
	h, s := setup(t)
	h.Limits.WebSockets = 1
	session := register(t, s, 1)
	conn := dial(t, s, session["execution_id"], 0)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	headers := http.Header{"Authorization": {"Bearer test-secret"}}
	c, resp, err := websocket.Dial(ctx, watchURL(s, session["execution_id"], 0), &websocket.DialOptions{HTTPHeader: headers, Subprotocols: []string{"iris.v1"}})
	if err == nil {
		c.CloseNow()
		t.Fatal("socket cap ignored")
	}
	if resp == nil || resp.StatusCode != 429 || resp.Header.Get("Retry-After") == "" {
		t.Fatal(resp, err)
	}
	conn.CloseNow()
	// Observe release rather than relying on scheduler timing.
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		h.mu.Lock()
		n := h.socketCounts[tokenDigest("test-secret")]
		h.mu.Unlock()
		if n == 0 {
			break
		}
		select {
		case <-deadline.C:
			t.Fatal("slot leaked")
		case <-tick.C:
		}
	}
	// A normal HTTP request fails the handshake but must release its reservation.
	r, _ := http.NewRequest("GET", s.URL+"/v1/watch?execution_id="+session["execution_id"].(string), nil)
	r.Header = headers
	r.Header.Set("Sec-WebSocket-Protocol", "iris.v1")
	response, err := s.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 426 {
		t.Fatal(response.StatusCode)
	}
	dial(t, s, session["execution_id"], 0)
}

func TestAnonymousFloodCannotLockOutValidTokens(t *testing.T) {
	h, s := setup(t)
	h.Limits = Limits{TokenPerMinute: 2, AddressPerMinute: 2}
	named := request(t, s, "POST", "/v1/tokens", map[string]string{"host": "proxy-host"}, 200)["token"].(string)
	for i, token := range []string{"", "wrong", "another-wrong", "", "test-secret", named, "wrong"} {
		r := httptest.NewRequest("GET", s.URL+"/v1/status", nil)
		r.RemoteAddr = "127.0.0.1:1234"
		r.Header.Set("X-Forwarded-For", "spoofed")
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		want := 200
		if i == 2 || i == 3 || i == 6 {
			want = 429
		}
		if w.Code != want {
			t.Fatalf("request %d: got %d want %d: %s", i, w.Code, want, w.Body.String())
		}
		if want == 429 && (w.Header().Get("Retry-After") == "" || !strings.Contains(w.Body.String(), "rate_limited")) {
			t.Fatal("missing quota response", w.Header(), w.Body.String())
		}
		if (i == 4 || i == 5) && !strings.Contains(w.Body.String(), "counts") {
			t.Fatal("valid token lost authentication", w.Body.String())
		}
	}
}
