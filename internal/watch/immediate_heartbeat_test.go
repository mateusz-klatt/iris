package watch

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/mateusz-klatt/iris/internal/binding"
	"github.com/mateusz-klatt/iris/internal/hub"
	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/proc"
	"github.com/mateusz-klatt/iris/internal/store"
	_ "modernc.org/sqlite"
)

type watchStepClock struct {
	mu   sync.Mutex
	wall time.Time
	tick int64
}

func newWatchStepClock() *watchStepClock {
	return &watchStepClock{wall: time.Date(2026, 11, 22, 0, 0, 0, 0, time.UTC)}
}

func (c *watchStepClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.wall }
func (c *watchStepClock) Tick() int64    { c.mu.Lock(); defer c.mu.Unlock(); return c.tick }
func (c *watchStepClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.wall = c.wall.Add(d)
	c.tick += d.Milliseconds()
	c.mu.Unlock()
}

type observedReq struct {
	path   string
	source string
}

// observedWatchHub runs a real hub on a steppable clock, signals once the first /v1/watch
// socket is accepted (so tests never touch resolver.Resolve from outside Runner.Run), and
// records requests after recording is enabled.
type observedWatchHub struct {
	t          *testing.T
	file       string
	clock      *watchStepClock
	server     *httptest.Server
	current    atomic.Pointer[hub.Hub]
	store      *store.Store
	firstWatch chan struct{}
	watchOnce  sync.Once
	recording  atomic.Bool
	mu         sync.Mutex
	reqs       []observedReq
}

func newObservedWatchHub(t *testing.T) *observedWatchHub {
	t.Helper()
	h := &observedWatchHub{
		t:          t,
		file:       filepath.Join(t.TempDir(), "hub.db"),
		clock:      newWatchStepClock(),
		firstWatch: make(chan struct{}),
	}
	h.server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cur := h.current.Load()
		if cur == nil {
			http.Error(w, "restarting", http.StatusServiceUnavailable)
			return
		}
		if r.URL.Path == "/v1/watch" {
			h.watchOnce.Do(func() { close(h.firstWatch) })
		}
		if h.recording.Load() {
			rec := observedReq{path: r.URL.Path}
			if strings.HasSuffix(r.URL.Path, "/heartbeat") && r.Body != nil {
				raw, _ := io.ReadAll(r.Body)
				r.Body = io.NopCloser(bytes.NewReader(raw))
				var body map[string]any
				_ = json.Unmarshal(raw, &body)
				rec.source, _ = body["source"].(string)
			}
			h.mu.Lock()
			h.reqs = append(h.reqs, rec)
			h.mu.Unlock()
		}
		cur.ServeHTTP(w, r)
	}))
	h.server.Start()
	h.open(0)
	t.Cleanup(func() { h.server.Close(); _ = h.store.Close() })
	return h
}

func (h *observedWatchHub) open(advanceAfterOpen time.Duration) {
	h.t.Helper()
	s, err := store.Open(h.file, h.clock)
	if err != nil {
		h.t.Fatal(err)
	}
	if advanceAfterOpen > 0 {
		h.clock.Advance(advanceAfterOpen)
	}
	h.store = s
	real := hub.New(s, "token", []string{h.server.Listener.Addr().String()})
	real.PingInterval = 20 * time.Millisecond
	h.current.Store(real)
}

func (h *observedWatchHub) restart(advanceAfterOpen time.Duration) {
	h.t.Helper()
	h.current.Store(nil)
	h.server.CloseClientConnections()
	if err := h.store.Close(); err != nil {
		h.t.Fatal(err)
	}
	h.open(advanceAfterOpen)
}

func (h *observedWatchHub) backup() string {
	h.t.Helper()
	h.current.Store(nil)
	if err := h.store.Close(); err != nil {
		h.t.Fatal(err)
	}
	raw, err := os.ReadFile(h.file)
	if err != nil {
		h.t.Fatal(err)
	}
	dst := filepath.Join(h.t.TempDir(), "backup.db")
	if err := os.WriteFile(dst, raw, 0o600); err != nil {
		h.t.Fatal(err)
	}
	h.open(0)
	return dst
}

func (h *observedWatchHub) restore(backupPath string) {
	h.t.Helper()
	h.current.Store(nil)
	h.server.CloseClientConnections()
	if err := h.store.Close(); err != nil {
		h.t.Fatal(err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		_ = os.Remove(h.file + suffix)
	}
	raw, err := os.ReadFile(backupPath)
	if err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(h.file, raw, 0o600); err != nil {
		h.t.Fatal(err)
	}
	db, err := sql.Open("sqlite", h.file)
	if err != nil {
		h.t.Fatal(err)
	}
	if err := store.MarkRestore(context.Background(), db, h.clock.Now()); err != nil {
		_ = db.Close()
		h.t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		h.t.Fatal(err)
	}
	h.open(0)
}

func (h *observedWatchHub) startRecording() {
	h.mu.Lock()
	h.reqs = nil
	h.mu.Unlock()
	h.recording.Store(true)
}

func (h *observedWatchHub) recorded() []observedReq {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]observedReq(nil), h.reqs...)
}

func assertWatchHeartbeatBeforeBind(t *testing.T, reqs []observedReq, ex string) {
	t.Helper()
	wantHB := "/v1/executions/" + ex + "/heartbeat"
	hbIdx, bindIdx := -1, -1
	for i, req := range reqs {
		if req.path == wantHB && req.source == "watch" && hbIdx < 0 {
			hbIdx = i
		}
		if req.path == "/v1/bind" && bindIdx < 0 {
			bindIdx = i
		}
	}
	if hbIdx < 0 || (bindIdx >= 0 && hbIdx > bindIdx) {
		t.Fatalf("expected immediate watch heartbeat (%s, source=watch) before reconnect /v1/bind, got %+v", wantHB, reqs)
	}
}

func TestWatchHeartbeatsImmediatelyOnReconnectWithoutMaintain(t *testing.T) {
	h := newObservedWatchHub(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	out, err := h.store.Call(ctx, "sessions", store.Request{Client: "claude", Host: "host", SessionID: "s-reconn", Cwd: "/repo", Source: "hook"})
	if err != nil {
		t.Fatal(err)
	}
	execution := out["execution_id"].(string)
	recipient := out["agent"].(map[string]any)["name"].(string)
	sender, err := h.store.Call(ctx, "sessions", store.Request{Client: "codex", Host: "host", SessionID: "sender", Cwd: "/repo2", Source: "hook"})
	if err != nil {
		t.Fatal(err)
	}

	resolver := &binding.Resolver{
		Hub:             &hubclient.Client{Config: testConfig{h.server.URL}},
		Client:          "claude",
		Host:            "host",
		CWD:             "/repo",
		HeartbeatSource: "watch",
		Getenv: func(k string) string {
			if k == "IRIS_EXECUTION" {
				return execution
			}
			return ""
		},
		Ancestor: func() (proc.Ancestor, error) { return proc.Ancestor{}, nil },
	}
	// Deliberately do NOT start resolver.Maintain and do NOT call resolver.Resolve outside Runner.Run.
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	output := &lines{}
	done := make(chan error, 1)
	go func() {
		done <- (&Runner{Resolver: resolver, Output: output, Backoff: 10 * time.Millisecond}).Run(runCtx)
	}()

	select {
	case <-h.firstWatch:
	case <-time.After(5 * time.Second):
		t.Fatal("watch never established its first /v1/watch connection")
	}
	h.startRecording()
	h.restart(0)

	// Send a message once the hub is back; watch must reattach via immediate watch heartbeat before bind.
	if _, err := h.store.Call(ctx, "sessions", store.Request{Client: "codex", Host: "host", SessionID: "sender", Cwd: "/repo2", Generation: 1, Source: "hook"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.Call(ctx, "messages", store.Request{ExecutionID: sender["execution_id"].(string), Generation: 1, Key: "after-restart", To: []string{recipient}, Body: "hi"}); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); !strings.Contains(output.String(), "Iris: 1 new message"); {
		if time.Now().After(deadline) {
			t.Fatalf("watch did not reattach and deliver after restart: %q lost=%v reqs=%+v", output.String(), resolver.Lost(), h.recorded())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if resolver.Lost() {
		t.Fatal("watch marked recovering execution lost")
	}
	assertWatchHeartbeatBeforeBind(t, h.recorded(), execution)
	stop()
	if err := <-done; err != nil {
		t.Fatalf("watch ended with %v", err)
	}
}

func TestWatchHeartbeatsImmediatelyOnResetAndNeverRevivesTerminal(t *testing.T) {
	t.Run("reset sends immediate watch heartbeat before snapshot", func(t *testing.T) {
		var mu sync.Mutex
		var order []string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.URL.Path == "/v1/bind":
				_, _ = w.Write([]byte(`{"execution_id":"ex_reset","generation":2,"repository_id":"rp"}`))
			case strings.HasSuffix(r.URL.Path, "/ex_reset/heartbeat"):
				mu.Lock()
				order = append(order, "heartbeat")
				mu.Unlock()
				_, _ = w.Write([]byte(`{"lease_ms":60000}`))
			case r.URL.Path == "/v1/events":
				mu.Lock()
				order = append(order, "events")
				mu.Unlock()
				_, _ = w.Write([]byte(`{"stream":"s2","high_water":5,"pending":1,"snapshot":{"unread":1}}`))
			case r.URL.Path == "/v1/watch":
				conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"iris.v1"}})
				if err != nil {
					return
				}
				defer func() { _ = conn.CloseNow() }()
				_ = conn.Write(r.Context(), websocket.MessageText, []byte(`{"stream":"s2","cursor":5,"kind":"reset"}`))
				<-r.Context().Done()
			}
		}))
		defer srv.Close()

		resolver := &binding.Resolver{
			Hub:             &hubclient.Client{Config: testConfig{srv.URL}},
			Client:          "claude",
			Host:            "host",
			HeartbeatSource: "watch",
			Getenv:          func(k string) string { return map[string]string{"IRIS_EXECUTION": "ex_reset"}[k] },
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		out := &lines{}
		done := make(chan error, 1)
		go func() {
			done <- (&Runner{Resolver: resolver, Output: out, Backoff: 10 * time.Millisecond}).Run(ctx)
		}()
		for deadline := time.Now().Add(3 * time.Second); !strings.Contains(out.String(), "Iris: 1 new message"); {
			if time.Now().After(deadline) {
				t.Fatalf("reset did not announce mail: %q", out.String())
			}
			time.Sleep(10 * time.Millisecond)
		}
		cancel()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		defer mu.Unlock()
		if len(order) < 2 || order[0] != "heartbeat" || order[1] != "events" {
			t.Fatalf("expected heartbeat before snapshot events on reset, got %v", order)
		}
	})

	t.Run("reset heartbeat lost fails connect without reviving terminal execution", func(t *testing.T) {
		var snapshots atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.URL.Path == "/v1/bind":
				_, _ = w.Write([]byte(`{"execution_id":"ex_dead","generation":1}`))
			case strings.HasSuffix(r.URL.Path, "/ex_dead/heartbeat"):
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte(`{"error":{"code":"lost","message":"ended"}}`))
			case r.URL.Path == "/v1/events":
				snapshots.Add(1)
				_, _ = w.Write([]byte(`{}`))
			case r.URL.Path == "/v1/watch":
				conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"iris.v1"}})
				if err != nil {
					return
				}
				defer func() { _ = conn.CloseNow() }()
				_ = conn.Write(r.Context(), websocket.MessageText, []byte(`{"kind":"reset"}`))
				<-r.Context().Done()
			}
		}))
		defer srv.Close()

		resolver := &binding.Resolver{
			Hub:    &hubclient.Client{Config: testConfig{srv.URL}},
			Client: "claude", Host: "host",
			Getenv: func(k string) string { return map[string]string{"IRIS_EXECUTION": "ex_dead"}[k] },
		}
		b, err := resolver.Resolve(context.Background(), "")
		if err != nil {
			t.Fatal(err)
		}
		r := &Runner{Resolver: resolver, Output: &lines{}}
		if err := r.connect(context.Background(), b, new(int64), new(string), &Coalescer{Output: &lines{}}); !binding.IsCode(err, "lost") {
			t.Fatalf("expected lost from reset heartbeat, got %v", err)
		}
		if !resolver.Lost() || snapshots.Load() != 0 {
			t.Fatalf("terminal execution not marked lost or fetched snapshot: lost=%v snapshots=%d", resolver.Lost(), snapshots.Load())
		}
	})

	t.Run("reconnect never revives ended execution", func(t *testing.T) {
		h := newObservedWatchHub(t)
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		out, err := h.store.Call(ctx, "sessions", store.Request{Client: "claude", Host: "host", SessionID: "s-end", Cwd: "/repo", Source: "hook"})
		if err != nil {
			t.Fatal(err)
		}
		ex := out["execution_id"].(string)
		resolver := &binding.Resolver{
			Hub:             &hubclient.Client{Config: testConfig{h.server.URL}},
			Client:          "claude",
			Host:            "host",
			CWD:             "/repo",
			HeartbeatSource: "watch",
			Getenv:          func(k string) string { return map[string]string{"IRIS_EXECUTION": ex}[k] },
			Ancestor:        func() (proc.Ancestor, error) { return proc.Ancestor{}, nil },
		}
		runCtx, stop := context.WithCancel(ctx)
		defer stop()
		output := &lines{}
		done := make(chan error, 1)
		go func() {
			done <- (&Runner{Resolver: resolver, Output: output, Backoff: 10 * time.Millisecond}).Run(runCtx)
		}()
		select {
		case <-h.firstWatch:
		case <-time.After(5 * time.Second):
			t.Fatal("watch never accepted initial /v1/watch connection")
		}
		h.startRecording()
		// End the Execution (terminal) without starting Maintain; watch reconnect heartbeat sees lost.
		if _, err := h.store.Call(ctx, "end", store.Request{ExecutionID: ex, Generation: 1}); err != nil {
			t.Fatal(err)
		}
		for deadline := time.Now().Add(5 * time.Second); !strings.Contains(output.String(), "Iris: notifications stopped:"); {
			if time.Now().After(deadline) {
				t.Fatalf("watch never announced stopped for ended execution: %q reqs=%+v", output.String(), h.recorded())
			}
			time.Sleep(10 * time.Millisecond)
		}
		assertWatchHeartbeatBeforeBind(t, h.recorded(), ex)
		stop()
		if err := <-done; err != ErrLost {
			t.Fatalf("expected ErrLost, got %v", err)
		}
	})

	t.Run("post-120s refusal on watch reconnect", func(t *testing.T) {
		h := newObservedWatchHub(t)
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		out, err := h.store.Call(ctx, "sessions", store.Request{Client: "claude", Host: "host", SessionID: "s-120", Cwd: "/repo", Source: "hook"})
		if err != nil {
			t.Fatal(err)
		}
		ex := out["execution_id"].(string)
		resolver := &binding.Resolver{
			Hub:             &hubclient.Client{Config: testConfig{h.server.URL}},
			Client:          "claude",
			Host:            "host",
			CWD:             "/repo",
			HeartbeatSource: "watch",
			Getenv:          func(k string) string { return map[string]string{"IRIS_EXECUTION": ex}[k] },
			Ancestor:        func() (proc.Ancestor, error) { return proc.Ancestor{}, nil },
		}
		runCtx, stop := context.WithCancel(ctx)
		defer stop()
		output := &lines{}
		done := make(chan error, 1)
		go func() {
			done <- (&Runner{Resolver: resolver, Output: output, Backoff: 10 * time.Millisecond}).Run(runCtx)
		}()
		select {
		case <-h.firstWatch:
		case <-time.After(5 * time.Second):
			t.Fatal("watch never accepted initial /v1/watch connection")
		}
		h.startRecording()
		h.restart(121 * time.Second)
		for deadline := time.Now().Add(5 * time.Second); !strings.Contains(output.String(), "Iris: notifications stopped:"); {
			if time.Now().After(deadline) {
				t.Fatalf("watch never announced stopped past 120s: %q reqs=%+v", output.String(), h.recorded())
			}
			time.Sleep(10 * time.Millisecond)
		}
		assertWatchHeartbeatBeforeBind(t, h.recorded(), ex)
		stop()
		if err := <-done; err != ErrLost {
			t.Fatalf("expected ErrLost, got %v", err)
		}
		status, err := h.store.Call(ctx, "status", store.Request{})
		if err != nil || status["counts"].(map[string]int)["Execution"] != 1 {
			t.Fatalf("watch reconnect past 120s spawned replacement: %+v %v", status, err)
		}
	})

	t.Run("restored older generation refuses watch reconnect without downgrade", func(t *testing.T) {
		h := newObservedWatchHub(t)
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		ex1, err := h.store.Call(ctx, "sessions", store.Request{Client: "claude", Host: "host", SessionID: "s-restore", Cwd: "/repo", Source: "hook"})
		if err != nil {
			t.Fatal(err)
		}
		backupPath := h.backup()
		if _, err := h.store.Call(ctx, "heartbeat", store.Request{ExecutionID: ex1["execution_id"].(string), Generation: 1, Source: "watch"}); err != nil {
			t.Fatal(err)
		}
		if _, err := h.store.Call(ctx, "end", store.Request{ExecutionID: ex1["execution_id"].(string), Generation: 1}); err != nil {
			t.Fatal(err)
		}
		ex2, err := h.store.Call(ctx, "sessions", store.Request{Client: "claude", Host: "host", SessionID: "s-restore", Cwd: "/repo", Source: "hook"})
		if err != nil {
			t.Fatal(err)
		}
		ex2ID := ex2["execution_id"].(string)
		resolver := &binding.Resolver{
			Hub:             &hubclient.Client{Config: testConfig{h.server.URL}},
			Client:          "claude",
			Host:            "host",
			CWD:             "/repo",
			HeartbeatSource: "watch",
			Getenv:          func(k string) string { return map[string]string{"IRIS_EXECUTION": ex2ID}[k] },
			Ancestor:        func() (proc.Ancestor, error) { return proc.Ancestor{}, nil },
		}
		runCtx, stop := context.WithCancel(ctx)
		defer stop()
		output := &lines{}
		done := make(chan error, 1)
		go func() {
			done <- (&Runner{Resolver: resolver, Output: output, Backoff: 10 * time.Millisecond}).Run(runCtx)
		}()
		select {
		case <-h.firstWatch:
		case <-time.After(5 * time.Second):
			t.Fatal("watch never accepted initial /v1/watch connection")
		}
		h.startRecording()
		h.restore(backupPath)
		for deadline := time.Now().Add(5 * time.Second); !strings.Contains(output.String(), "Iris: notifications stopped:"); {
			if time.Now().After(deadline) {
				t.Fatalf("watch never announced stopped on restored older generation: %q reqs=%+v", output.String(), h.recorded())
			}
			time.Sleep(10 * time.Millisecond)
		}
		assertWatchHeartbeatBeforeBind(t, h.recorded(), ex2ID)
		stop()
		if err := <-done; err != ErrLost {
			t.Fatalf("expected ErrLost, got %v", err)
		}
	})
}
