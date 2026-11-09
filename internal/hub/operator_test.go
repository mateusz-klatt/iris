package hub

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestOperatorThreadsAndSecurity(t *testing.T) {
	h, s := setup(t)
	h.console = newConsoleAuth(make([]byte, 32))
	_, cookie, csrf := routeConsoleAccount(t, h, "Thread operator", "operator")
	ui := func(method, path string, body any, status int) map[string]any {
		t.Helper()
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		return consoleRoute(t, h, s, cookie, csrf, method, path, string(raw), status)
	}
	a, b := register(t, s, 1), register(t, s, 2)
	msg := authority(a)
	msg["key"], msg["to"] = "human", []string{"human"}
	msg["subject"], msg["body"] = "Need operator", "searchable pineapple"
	sent := request(t, s, "POST", "/v1/messages", msg, 200)
	thread := sent["thread_id"].(string)
	overview := ui("GET", "/v1/ui/overview", nil, 200)
	if overview["human_unread"] != float64(1) || len(overview["agents"].([]any)) != 2 {
		t.Fatal(overview)
	}
	for _, q := range []string{"?q=pineapple", "?repository_id=" + a["repository_id"].(string), "?agent=" + a["agent"].(map[string]any)["id"].(string)} {
		threads := ui("GET", "/v1/ui/threads"+q, nil, 200)
		if len(threads["threads"].([]any)) != 1 {
			t.Fatal(threads)
		}
	}
	reply := map[string]any{"key": "reply", "to": []string{b["agent"].(map[string]any)["id"].(string)}, "reply_to": sent["message_id"], "body": "human response"}
	response := ui("POST", "/v1/ui/messages", reply, 200)
	if response["thread_id"] != thread {
		t.Fatal(response)
	}
	repeated := ui("POST", "/v1/ui/messages", reply, 200)
	if repeated["message_id"] != response["message_id"] {
		t.Fatal("operator retry doubled send")
	}
	details := ui("GET", "/v1/ui/threads/"+thread, nil, 200)
	messages := details["messages"].([]any)
	if len(messages) != 2 || messages[1].(map[string]any)["sender"] != "human" {
		t.Fatal(details)
	}
	inbox := request(t, s, "GET", "/v1/inbox?execution_id="+b["execution_id"].(string), nil, 200)
	if inbox["deliveries"].([]any)[0].(map[string]any)["sender"] != "human" {
		t.Fatal(inbox)
	}
	claim := authority(a)
	claim["key"], claim["kind"], claim["path"] = "claim", "exact", "a.go"
	granted := request(t, s, "POST", "/v1/claims", claim, 200)
	raw, _ := json.Marshal(granted["fence"])
	ui("POST", "/v1/ui/claims/"+string(raw)+"/revoke", map[string]any{}, 200)
	request(t, s, "PUT", "/v1/ui/doctor", map[string]any{"host": "mac", "checked_at": "now", "clients": []any{map[string]any{"client": "codex", "status": "needs_human"}}}, 200)
	report := ui("GET", "/v1/ui/doctor", nil, 200)
	if hosts := report["hosts"].([]any); len(hosts) != 1 || hosts[0].(map[string]any)["host"] != "mac" || hosts[0].(map[string]any)["checked_at"] != "now" {
		t.Fatal(report)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(s.URL, "http")+"/v1/ui/watch", &websocket.DialOptions{HTTPHeader: http.Header{"Origin": {s.URL}, "Cookie": {cookie.String()}}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ws.CloseNow() }()
	_, frame, err := ws.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var change map[string]any
	if err := json.Unmarshal(frame, &change); err != nil || len(change) != 2 || change["kind"] != "changed" || change["cursor"] == nil {
		t.Fatalf("UI stream carried more than a revision: %s %v", frame, err)
	}
	for _, path := range []string{"/v1/ui/overview", "/v1/ui/watch"} {
		req := httptest.NewRequest("GET", s.URL+path, nil)
		req.RemoteAddr = "127.0.0.1:1000"
		req.AddCookie(cookie)
		req.Header.Set("Origin", "https://evil.example")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != 401 {
			t.Fatal(path, w.Code)
		}
	}
	h.SetUI(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "static") }))
	req := httptest.NewRequest("GET", s.URL+"/", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Body.String() != "static" {
		t.Fatal(w.Body.String())
	}
}
func TestOneMonitorPerExecution(t *testing.T) {
	_, s := setup(t)
	a, b := register(t, s, 1), register(t, s, 2)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	endpoint := watchURL(s, b["execution_id"], 0) + "&consumer_id=iris-watch"
	options := &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer test-secret"}}, Subprotocols: []string{"iris.v1"}}
	conn, _, err := websocket.Dial(ctx, endpoint, options)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.CloseNow() }()
	other, res, err := websocket.Dial(ctx, endpoint, options)
	if other != nil {
		_ = other.CloseNow()
	}
	if err == nil || res.StatusCode != 409 {
		t.Fatal("duplicate monitor accepted")
	}
	send(t, s, a, b, "first")
	first := frame(t, conn)
	_ = conn.CloseNow()
	send(t, s, a, b, "second")
	send(t, s, a, b, "third")
	endpoint = watchURL(s, b["execution_id"], int64(first["cursor"].(float64))) + "&consumer_id=iris-watch"
	var again *websocket.Conn
	for i := 0; i < 20; i++ {
		again, _, err = websocket.Dial(ctx, endpoint, options)
		if err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = again.CloseNow() }()
	second, third := frame(t, again), frame(t, again)
	if first["cursor"].(float64) >= second["cursor"].(float64) || second["cursor"].(float64) >= third["cursor"].(float64) {
		t.Fatal(first, second, third)
	}
}
