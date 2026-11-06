package hub

import (
	"context"
	"encoding/json"
	"github.com/coder/websocket"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestOperatorThreadsAndSecurity(t *testing.T) {
	h, s := setup(t)
	a, b := register(t, s, 1), register(t, s, 2)
	msg := authority(a)
	msg["key"] = "human"
	msg["to"] = []string{"human"}
	msg["subject"] = "Need operator"
	msg["body"] = "searchable pineapple"
	sent := request(t, s, "POST", "/v1/messages", msg, 200)
	thread := sent["thread_id"].(string)
	overview := request(t, s, "GET", "/v1/ui/overview", nil, 200)
	if overview["human_unread"] != float64(1) || len(overview["agents"].([]any)) != 2 {
		t.Fatal(overview)
	}
	for _, q := range []string{"?q=pineapple", "?repository_id=" + a["repository_id"].(string), "?agent=" + a["agent"].(map[string]any)["name"].(string)} {
		threads := request(t, s, "GET", "/v1/ui/threads"+q, nil, 200)
		if len(threads["threads"].([]any)) != 1 {
			t.Fatal(threads)
		}
	}
	reply := map[string]any{"key": "reply", "to": []string{b["agent"].(map[string]any)["name"].(string)}, "reply_to": sent["message_id"], "body": "human response"}
	response := request(t, s, "POST", "/v1/ui/messages", reply, 200)
	if response["thread_id"] != thread {
		t.Fatal(response)
	}
	repeated := request(t, s, "POST", "/v1/ui/messages", reply, 200)
	if repeated["message_id"] != response["message_id"] {
		t.Fatal("operator retry doubled send")
	}
	details := request(t, s, "GET", "/v1/ui/threads/"+thread, nil, 200)
	messages := details["messages"].([]any)
	if len(messages) != 2 || messages[1].(map[string]any)["sender"] != "human" {
		t.Fatal(details)
	}
	inbox := request(t, s, "GET", "/v1/inbox?execution_id="+b["execution_id"].(string), nil, 200)
	if inbox["deliveries"].([]any)[0].(map[string]any)["sender"] != "human" {
		t.Fatal(inbox)
	}
	claim := authority(a)
	claim["key"] = "claim"
	claim["kind"] = "exact"
	claim["path"] = "a.go"
	granted := request(t, s, "POST", "/v1/claims", claim, 200)
	raw, _ := json.Marshal(granted["fence"])
	request(t, s, "POST", "/v1/ui/claims/"+string(raw)+"/revoke", map[string]any{}, 200)
	request(t, s, "PUT", "/v1/ui/doctor", map[string]any{"checked_at": "now", "clients": []any{map[string]any{"client": "codex", "status": "needs_human"}}}, 200)
	report := request(t, s, "GET", "/v1/ui/doctor", nil, 200)
	if report["checked_at"] != "now" {
		t.Fatal(report)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(s.URL, "http")+"/v1/ui/watch", &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer test-secret"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	_, frame, err := ws.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(frame), "pineapple") || strings.Contains(string(frame), "body") {
		t.Fatal("UI stream carried content")
	}
	link := request(t, s, "POST", "/v1/ui/links", map[string]any{}, 200)["url"].(string)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := client.Get(link)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 303 || len(res.Cookies()) != 1 {
		t.Fatal(res.Status)
	}
	cookie := res.Cookies()[0]
	if !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode {
		t.Fatal(cookie)
	}
	res, err = client.Get(link)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 401 {
		t.Fatal("reused login", res.Status)
	}
	req := httptest.NewRequest("GET", s.URL+"/v1/ui/overview", nil)
	req.AddCookie(cookie)
	req.RemoteAddr = "192.0.2.1:1000"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 401 {
		t.Fatal("remote cookie accepted", w.Code)
	}
	req.RemoteAddr = "127.0.0.1:1000"
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, path := range []string{"/ui/login?code=bad", "/v1/ui/overview", "/v1/ui/watch", "/"} {
		req = httptest.NewRequest("GET", s.URL+path, nil)
		req.Header.Set("Origin", "https://evil.example")
		w = httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != 401 {
			t.Fatal(path, w.Code)
		}
	}
	h.SetUI(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "static") }))
	req = httptest.NewRequest("GET", s.URL+"/", nil)
	w = httptest.NewRecorder()
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
	defer conn.CloseNow()
	other, res, err := websocket.Dial(ctx, endpoint, options)
	if other != nil {
		other.CloseNow()
	}
	if err == nil || res.StatusCode != 409 {
		t.Fatal("duplicate monitor accepted")
	}
	send(t, s, a, b, "first")
	first := frame(t, conn)
	conn.CloseNow()
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
	defer again.CloseNow()
	second, third := frame(t, again), frame(t, again)
	if first["cursor"].(float64) >= second["cursor"].(float64) || second["cursor"].(float64) >= third["cursor"].(float64) {
		t.Fatal(first, second, third)
	}
}
