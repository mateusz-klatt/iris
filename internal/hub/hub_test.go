package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/mateusz-klatt/iris/internal/store"
)

func setup(t *testing.T) (*Hub, *httptest.Server) {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "hub.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	server := httptest.NewUnstartedServer(nil)
	h := New(s, "test-secret", []string{server.Listener.Addr().String()})
	server.Config.Handler = h
	server.Start()
	t.Cleanup(server.Close)
	return h, server
}
func request(t *testing.T, s *httptest.Server, method, path string, value any, status int) map[string]any {
	t.Helper()
	var body io.Reader
	if value != nil {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		body = bytes.NewReader(raw)
	}
	r, err := http.NewRequest(method, s.URL+path, body)
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Authorization", "Bearer test-secret")
	resp, err := s.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err = json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != status {
		t.Fatalf("%s %s: status %d, wanted %d: %v", method, path, resp.StatusCode, status, out)
	}
	return out
}
func register(t *testing.T, s *httptest.Server, n int) map[string]any {
	return request(t, s, "POST", "/v1/sessions", map[string]any{"client": "codex", "session_id": fmt.Sprint(n), "host": "host", "cwd": fmt.Sprintf("/repo/%d", n), "worktree": fmt.Sprintf("/repo/%d", n), "remote_url": "https://example.com/repo.git", "root_commit": "root"}, 200)
}
func authority(session map[string]any) map[string]any {
	return map[string]any{"execution_id": session["execution_id"], "generation": session["generation"]}
}
func watchURL(s *httptest.Server, ex any, after int64) string {
	return "ws" + strings.TrimPrefix(s.URL, "http") + "/v1/watch?execution_id=" + url.QueryEscape(ex.(string)) + fmt.Sprintf("&after=%d", after)
}
func dial(t *testing.T, s *httptest.Server, ex any, after int64) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, watchURL(s, ex, after), &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": []string{"Bearer test-secret"}}, Subprotocols: []string{"iris.v1"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.CloseNow() })
	return conn
}
func frame(t *testing.T, conn *websocket.Conn) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, raw, err := conn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err = json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if len(out) != 3 || out["stream"] == nil || out["cursor"] == nil || out["kind"] == nil {
		t.Fatalf("content-bearing or malformed frame: %s", raw)
	}
	return out
}
func send(t *testing.T, s *httptest.Server, a, b map[string]any, key string) map[string]any {
	r := authority(a)
	r["key"] = key
	r["to"] = []string{b["agent"].(map[string]any)["name"].(string)}
	r["subject"] = "PRIVATE SUBJECT"
	r["body"] = "PRIVATE BODY"
	return request(t, s, "POST", "/v1/messages", r, 200)
}
func TestHTTPContract(t *testing.T) {
	_, s := setup(t)
	a, b := register(t, s, 1), register(t, s, 2)
	ex := a["execution_id"].(string)
	request(t, s, "POST", "/v1/bind", map[string]any{"execution": ex, "client": "codex", "host": "host"}, 200)
	request(t, s, "POST", "/v1/executions/"+ex+"/heartbeat", map[string]any{"generation": 1, "source": "shim"}, 200)
	r := authority(a)
	r["key"] = "claim"
	r["kind"] = "exact"
	r["path"] = "src/a.go"
	claim := request(t, s, "POST", "/v1/claims", r, 200)
	r = authority(b)
	r["tool_call_id"] = "touch"
	r["paths"] = []string{"src/a.go", "free"}
	if request(t, s, "POST", "/v1/claims/touch", r, 200)["action"] != "deny" {
		t.Fatal("missing deny")
	}
	request(t, s, "GET", "/v1/claims?repository_id="+a["repository_id"].(string), nil, 200)
	request(t, s, "GET", "/v1/agents?repository_id="+a["repository_id"].(string), nil, 200)
	r = authority(a)
	r["fence"] = claim["fence"]
	r["token"] = claim["token"]
	request(t, s, "POST", "/v1/claims/release", r, 200)
	sent := send(t, s, a, b, "mail")
	id := sent["deliveries"].([]any)[0].(map[string]any)["id"].(string)
	inbox := request(t, s, "GET", "/v1/inbox?execution_id="+b["execution_id"].(string), nil, 200)
	delivery := inbox["deliveries"].([]any)[0].(map[string]any)
	r = authority(b)
	r["attempt"] = delivery["attempt"]
	request(t, s, "POST", "/v1/deliveries/"+id+"/ack", r, 200)
	request(t, s, "POST", "/v1/deliveries/"+id+"/ack", r, 200)
	request(t, s, "GET", "/v1/events?execution_id="+b["execution_id"].(string), nil, 200)
	request(t, s, "POST", "/v1/executions/"+ex+"/end", map[string]any{"generation": 1}, 200)
	request(t, s, "POST", "/v1/executions/"+ex+"/heartbeat", map[string]any{"generation": 1, "source": "shim"}, 409)
}
func TestWatchReconnectIsolationReset(t *testing.T) {
	h, s := setup(t)
	a, b := register(t, s, 1), register(t, s, 2)
	c := register(t, s, 3)
	conn := dial(t, s, b["execution_id"], 0)
	send(t, s, a, b, "one")
	one := frame(t, conn)
	if one["kind"] != "inbox_changed" {
		t.Fatal(one)
	}
	cursor := int64(one["cursor"].(float64))
	conn.CloseNow()
	send(t, s, a, c, "other-recipient")
	send(t, s, a, b, "two")
	send(t, s, a, b, "three")
	again := dial(t, s, b["execution_id"], cursor)
	two, three := frame(t, again), frame(t, again)
	if two["kind"] != "inbox_changed" || two["cursor"].(float64) >= three["cursor"].(float64) {
		t.Fatal(two, three)
	}
	again.CloseNow()
	// Watching never fetches or acknowledges messages.
	inbox := request(t, s, "GET", "/v1/inbox?execution_id="+b["execution_id"].(string), nil, 200)
	if len(inbox["deliveries"].([]any)) != 3 {
		t.Fatal(inbox)
	}
	for _, v := range inbox["deliveries"].([]any) {
		if v.(map[string]any)["attempt"] != float64(1) {
			t.Fatal(v)
		}
	}
	if err := h.store.PruneEvents(context.Background(), int64(two["cursor"].(float64))); err != nil {
		t.Fatal(err)
	}
	resetConn := dial(t, s, b["execution_id"], cursor)
	if frame(t, resetConn)["kind"] != "reset" {
		t.Fatal("reset missing")
	}
	snapshot := request(t, s, "GET", fmt.Sprintf("/v1/events?execution_id=%s&after=%d", b["execution_id"], cursor), nil, 200)
	if snapshot["reset"] != true || snapshot["snapshot"] == nil {
		t.Fatal(snapshot)
	}
}
func TestWakeTicketsRotateScopeExpireAndDie(t *testing.T) {
	h, s := setup(t)
	a, b := register(t, s, 1), register(t, s, 2)
	issue := func() map[string]any { return request(t, s, "POST", "/v1/wake-tickets", authority(b), 200) }
	ticket := issue()
	expired := issue()
	protocols := func(v map[string]any) []string {
		p := v["protocols"].([]any)
		return []string{p[0].(string), p[1].(string)}
	}
	try := func(rawURL string, p []string, ok bool) *websocket.Conn {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		conn, resp, err := websocket.Dial(ctx, rawURL, &websocket.DialOptions{Subprotocols: p})
		if ok {
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { conn.CloseNow() })
		} else {
			if err == nil {
				conn.CloseNow()
				t.Fatal("invalid ticket accepted")
			}
			if resp != nil && resp.Body != nil {
				resp.Body.Close()
			}
		}
		return conn
	}
	try(ticket["url"].(string), protocols(ticket), false)
	try(watchURL(s, a["execution_id"], 0), protocols(expired), false)
	now := time.Now()
	h.now = func() time.Time { return now.Add(3 * time.Minute) }
	try(expired["url"].(string), protocols(expired), false)
	h.now = time.Now
	valid := issue()
	conn := try(valid["url"].(string), protocols(valid), true)
	try(valid["url"].(string), protocols(valid), false)
	send(t, s, a, b, "ticket-mail")
	if frame(t, conn)["kind"] != "inbox_changed" {
		t.Fatal("missing wake")
	}
	conn.CloseNow()
	dead := issue()
	request(t, s, "POST", "/v1/executions/"+b["execution_id"].(string)+"/end", map[string]any{"generation": 1}, 200)
	try(dead["url"].(string), protocols(dead), false)
}
func TestTransportAndInputGuards(t *testing.T) {
	_, s := setup(t)
	for _, tc := range []struct {
		path, host, origin, auth string
		status                   int
	}{
		{"/v1/status", "", "", "", 200}, {"/v1/status", "evil.example", "", "", 401}, {"/v1/status", "", "https://evil.example", "", 401},
		{"/v1/agents", "", "", "", 401}, {"/v1/status", "", s.URL, "", 200}, {"/v1/status", "", "null", "", 401},
	} {
		r, _ := http.NewRequest("GET", s.URL+tc.path, nil)
		if tc.host != "" {
			r.Host = tc.host
		}
		if tc.origin != "" {
			r.Header.Set("Origin", tc.origin)
		}
		if tc.auth != "" {
			r.Header.Set("Authorization", tc.auth)
		}
		resp, err := s.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != tc.status {
			t.Fatalf("%+v: %d", tc, resp.StatusCode)
		}
		if resp.Header.Get("Access-Control-Allow-Origin") != "" {
			t.Fatal("CORS enabled")
		}
	}
	request(t, s, "GET", "/v1/events?after=bad", nil, 400)
	r, _ := http.NewRequest("POST", s.URL+"/v1/sessions", strings.NewReader(`{} {}`))
	r.Header.Set("Authorization", "Bearer test-secret")
	resp, err := s.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatal(resp.StatusCode)
	}
	request(t, s, "POST", "/v1/messages", map[string]any{"body": strings.Repeat("x", 70<<10)}, 413)
}

func TestConnectedWakeTicketRevocation(t *testing.T) {
	h, s := setup(t)
	b := register(t, s, 1)
	base := time.Now()
	var seconds atomic.Int64
	h.now = func() time.Time { return base.Add(time.Duration(seconds.Load()) * time.Second) }
	open := func() *websocket.Conn {
		ticket := request(t, s, "POST", "/v1/wake-tickets", authority(b), 200)
		raw := ticket["protocols"].([]any)
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		conn, _, err := websocket.Dial(ctx, ticket["url"].(string), &websocket.DialOptions{Subprotocols: []string{raw[0].(string), raw[1].(string)}})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { conn.CloseNow() })
		return conn
	}
	closed := func(conn *websocket.Conn) {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_, _, err := conn.Read(ctx)
		if websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
			t.Fatalf("ticket socket was not revoked: %v", err)
		}
	}
	rotated := open()
	request(t, s, "POST", "/v1/wake-tickets", authority(b), 200)
	closed(rotated)
	expired := open()
	seconds.Store(121)
	closed(expired)
	ended := open()
	request(t, s, "POST", "/v1/executions/"+b["execution_id"].(string)+"/end", map[string]any{"generation": 1}, 200)
	closed(ended)
}

func monitor(t *testing.T, s *httptest.Server, ex any, after int64) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, watchURL(s, ex, after)+"&consumer_id=iris-watch", &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": []string{"Bearer test-secret"}}, Subprotocols: []string{"iris.v1"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.CloseNow() })
	return conn
}

func TestNewMonitorHearsOnlyWhatIsWaiting(t *testing.T) {
	_, s := setup(t)
	a, b := register(t, s, 1), register(t, s, 2)
	send(t, s, a, b, "one")
	send(t, s, a, b, "two")
	conn := monitor(t, s, b["execution_id"], 0)
	first := frame(t, conn)
	if first["kind"] != "reset" {
		t.Fatal(first)
	}
	send(t, s, a, b, "three")
	next := frame(t, conn)
	if next["kind"] != "inbox_changed" || next["cursor"].(float64) <= first["cursor"].(float64) {
		t.Fatal(next)
	}
}

func TestEventForFetchedMailDoesNotAskForAttention(t *testing.T) {
	_, s := setup(t)
	a, b := register(t, s, 1), register(t, s, 2)
	conn := monitor(t, s, b["execution_id"], 0)
	conn.CloseNow()
	send(t, s, a, b, "one")
	request(t, s, "GET", "/v1/inbox?execution_id="+b["execution_id"].(string), nil, 200)
	again := dial(t, s, b["execution_id"], 0)
	if got := frame(t, again); got["kind"] != "inbox_read" {
		t.Fatal(got)
	}
}
