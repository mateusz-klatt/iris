package watch

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/mateusz-klatt/iris/internal/binding"
	"github.com/mateusz-klatt/iris/internal/hub"
	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/store"
)

type realHub struct {
	store *store.Store
	url   string
}

func startHub(t *testing.T) realHub {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "hub.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	server := httptest.NewUnstartedServer(nil)
	h := hub.New(s, "token", []string{server.Listener.Addr().String()})
	h.PingInterval = 20 * time.Millisecond
	server.Config.Handler = h
	server.Start()
	t.Cleanup(server.Close)
	return realHub{s, server.URL}
}

func (h realHub) call(t *testing.T, op string, r store.Request) map[string]any {
	t.Helper()
	out, err := h.store.Call(context.Background(), op, r)
	if err != nil {
		t.Fatal(op, err)
	}
	return out
}

func (h realHub) session(t *testing.T, id string) (string, int64) {
	t.Helper()
	out := h.call(t, "sessions", store.Request{Client: "claude", Host: "host", SessionID: id, Cwd: "/repo"})
	gen, _ := out["generation"].(int64)
	return out["execution_id"].(string), gen
}

func (h realHub) agentName(t *testing.T, ex string) string {
	t.Helper()
	out := h.call(t, "validate", store.Request{ExecutionID: ex})
	raw, _ := json.Marshal(out["agent"])
	var agent struct {
		Name string `json:"name"`
	}
	_ = json.Unmarshal(raw, &agent)
	return agent.Name
}

// watchFor runs a monitor for the Execution and returns what it printed.
func (h realHub) watchFor(t *testing.T, ex string, wait time.Duration) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	out := &lines{}
	r := &Runner{Resolver: &binding.Resolver{Hub: &hubclient.Client{Config: testConfig{url: h.url}}}, Output: out, Backoff: 10 * time.Millisecond}
	_ = r.connect(ctx, binding.Bound{ExecutionID: ex}, new(int64), new(string), &Coalescer{Output: out})
	return out.String()
}

// watchUntilSaid runs a monitor for the Execution until it has printed want, and returns what it
// printed. A fixed window for mail the monitor must announce missed it on a loaded machine; the
// bound only ends a monitor that never says it. A window still serves where a monitor must stay
// quiet, since a slower monitor is only quieter.
func (h realHub) watchUntilSaid(t *testing.T, ex, want string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out := &lines{}
	r := &Runner{Resolver: &binding.Resolver{Hub: &hubclient.Client{Config: testConfig{url: h.url}}}, Output: out, Backoff: 10 * time.Millisecond}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = r.connect(ctx, binding.Bound{ExecutionID: ex}, new(int64), new(string), &Coalescer{Output: out})
	}()
	for !strings.Contains(out.String(), want) {
		select {
		case <-done:
			return out.String()
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	<-done
	return out.String()
}

func TestANewMonitorDoesNotRepeatHistory(t *testing.T) {
	h := startHub(t)
	sender, gen := h.session(t, "sender")
	reader, _ := h.session(t, "reader")
	name := h.agentName(t, reader)
	h.call(t, "messages", store.Request{ExecutionID: sender, Generation: gen, Key: "m1", To: []string{name}, Body: "old"})
	box := h.call(t, "inbox", store.Request{ExecutionID: reader})
	if len(box["deliveries"].([]map[string]any)) != 1 {
		t.Fatal(box)
	}
	// The session restarts: the same agent, a new Execution, and no mail waiting.
	restarted, _ := h.session(t, "reader")
	if got := h.watchFor(t, restarted, 300*time.Millisecond); got != "" {
		t.Fatalf("repeated history: %q", got)
	}
}

func TestANewMonitorHearsAboutMailStillWaiting(t *testing.T) {
	h := startHub(t)
	sender, gen := h.session(t, "sender")
	reader, _ := h.session(t, "reader")
	name := h.agentName(t, reader)
	watchUntilSaved := func(after int64) (string, int64) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		out := &lines{}
		var saved int64
		r := &Runner{
			Resolver: &binding.Resolver{Hub: &hubclient.Client{Config: testConfig{url: h.url}}, Client: "claude", Host: "host", Getenv: func(key string) string {
				if key == "IRIS_EXECUTION" {
					return reader
				}
				return ""
			}},
			Output:  out,
			Backoff: time.Millisecond,
			checkpointed: func(cursor int64) {
				if cursor > after {
					saved = cursor
					cancel()
				}
			},
		}
		if err := r.Run(ctx); err != nil {
			t.Fatal(err)
		}
		if ctx.Err() != context.Canceled {
			t.Fatal("monitor never completed its cursor save")
		}
		return out.String(), saved
	}
	for _, key := range []string{"m1", "m2"} {
		h.call(t, "messages", store.Request{ExecutionID: sender, Generation: gen, Key: key, To: []string{name}, Body: "waiting"})
	}
	got, saved := watchUntilSaved(0)
	if got != "Iris: 2 new messages. Use the inbox tool.\n" {
		t.Fatalf("%q", got)
	}
	// The next completed save proves the reconnect processed new mail, without
	// relying on a silence interval. The two older messages still need attention
	// but their saved cursor keeps them from being announced again.
	h.call(t, "messages", store.Request{ExecutionID: sender, Generation: gen, Key: "m3", To: []string{name}, Body: "new"})
	if got, _ := watchUntilSaved(saved); got != "Iris: 1 new message. Use the inbox tool.\n" {
		t.Fatalf("reconnect repeated waiting mail or missed new mail: %q", got)
	}
}

func TestAMonitorIsQuietAboutMailAlreadyFetched(t *testing.T) {
	h := startHub(t)
	sender, gen := h.session(t, "sender")
	reader, _ := h.session(t, "reader")
	name := h.agentName(t, reader)
	if got := h.watchFor(t, reader, 150*time.Millisecond); got != "" {
		t.Fatalf("%q", got)
	}
	h.call(t, "messages", store.Request{ExecutionID: sender, Generation: gen, Key: "m1", To: []string{name}, Body: "read first"})
	h.call(t, "inbox", store.Request{ExecutionID: reader})
	if got := h.watchFor(t, reader, 300*time.Millisecond); got != "" {
		t.Fatalf("announced fetched mail: %q", got)
	}
	h.call(t, "messages", store.Request{ExecutionID: sender, Generation: gen, Key: "m2", To: []string{name}, Body: "new"})
	if got := h.watchUntilSaid(t, reader, "1 new message"); !strings.Contains(got, "1 new message") {
		t.Fatalf("missed new mail: %q", got)
	}
}

func TestAHeldLineChecksWhatIsStillWaiting(t *testing.T) {
	cases := []struct {
		name  string
		count func() (int, bool)
		want  string
	}{
		{"fetched in the window", func() (int, bool) { return 0, true }, ""},
		{"partly fetched", func() (int, bool) { return 1, true }, "Iris: 1 new message. Use the inbox tool.\n"},
		{"hub unreachable", func() (int, bool) { return 0, false }, "Iris: 2 new messages. Use the inbox tool.\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			now := time.Unix(1000, 0)
			c := &Coalescer{Output: &out, Window: time.Minute}
			if err := c.Add(now, "inbox_changed", 1); err != nil {
				t.Fatal(err)
			}
			out.Reset()
			if err := c.Add(now.Add(time.Second), "inbox_changed", 2); err != nil {
				t.Fatal(err)
			}
			c.Recount = tc.count
			if err := c.Flush(now.Add(2 * time.Minute)); err != nil {
				t.Fatal(err)
			}
			if out.String() != tc.want {
				t.Fatalf("%q", out.String())
			}
		})
	}
}

func TestWaitingReadsTheHub(t *testing.T) {
	h := startHub(t)
	ex, _ := h.session(t, "reader")
	r := &Runner{Resolver: &binding.Resolver{Hub: &hubclient.Client{Config: testConfig{url: h.url}}}}
	if n, ok := r.waiting(context.Background(), binding.Bound{ExecutionID: ex}, 0); !ok || n != 0 {
		t.Fatal(n, ok)
	}
	for name, handler := range map[string]http.HandlerFunc{
		"older hub": func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{}`)) },
		"failure":   func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) },
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(handler)
			defer srv.Close()
			r := &Runner{Resolver: &binding.Resolver{Hub: &hubclient.Client{Config: testConfig{url: srv.URL}}}}
			if _, ok := r.waiting(context.Background(), binding.Bound{ExecutionID: "ex"}, 0); ok {
				t.Fatal("reported a count it does not have")
			}
		})
	}
}

func TestAResetAnnouncesOnlyMailStillWaiting(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/watch" {
			_, _ = w.Write([]byte(`{"stream":"new","high_water":9,"pending":1,"snapshot":{"unread":3}}`))
			return
		}
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"iris.v1"}})
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		_ = conn.Write(r.Context(), websocket.MessageText, []byte(`{"stream":"new","cursor":9,"kind":"reset"}`))
		<-r.Context().Done()
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	out := &lines{}
	r := &Runner{Resolver: &binding.Resolver{Hub: &hubclient.Client{Config: testConfig{url: srv.URL}}}}
	_ = r.connect(ctx, binding.Bound{ExecutionID: "ex"}, new(int64), new(string), &Coalescer{Output: out})
	if got := out.String(); got != "Iris: 1 new message. Use the inbox tool.\n" {
		t.Fatalf("%q", got)
	}
}
