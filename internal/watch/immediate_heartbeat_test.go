package watch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/mateusz-klatt/iris/internal/binding"
	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/proc"
	"github.com/mateusz-klatt/iris/internal/store"
)

func TestWatchHeartbeatsImmediatelyOnReconnectWithoutMaintain(t *testing.T) {
	h := newRestartableHub(t)
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
	// Deliberately do NOT start resolver.Maintain: reconnect itself must heartbeat and reattach.
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	output := &lines{}
	done := make(chan error, 1)
	go func() {
		done <- (&Runner{Resolver: resolver, Output: output, Backoff: 10 * time.Millisecond}).Run(runCtx)
	}()

	// Wait until initial bind is established, then restart the hub.
	for deadline := time.Now().Add(5 * time.Second); ; {
		if _, err := resolver.Resolve(ctx, ""); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("watch never established initial binding")
		}
		time.Sleep(10 * time.Millisecond)
	}
	h.restart()

	// Send a message once the hub is back; watch must reattach on reconnect and announce it.
	if _, err := h.store.Call(ctx, "sessions", store.Request{Client: "codex", Host: "host", SessionID: "sender", Cwd: "/repo2", Generation: 1, Source: "hook"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.Call(ctx, "messages", store.Request{ExecutionID: sender["execution_id"].(string), Generation: 1, Key: "after-restart", To: []string{recipient}, Body: "hi"}); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); !strings.Contains(output.String(), "Iris: 1 new message"); {
		if time.Now().After(deadline) {
			t.Fatalf("watch did not reattach and deliver after restart: %q lost=%v", output.String(), resolver.Lost())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if resolver.Lost() {
		t.Fatal("watch marked recovering execution lost")
	}
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
		h := newRestartableHub(t)
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
		for deadline := time.Now().Add(5 * time.Second); ; {
			if _, err := resolver.Resolve(ctx, ""); err == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("watch never bound")
			}
			time.Sleep(10 * time.Millisecond)
		}
		// End the Execution (terminal) without starting Maintain; watch reconnect heartbeat sees lost.
		if _, err := h.store.Call(ctx, "end", store.Request{ExecutionID: ex, Generation: 1}); err != nil {
			t.Fatal(err)
		}
		for deadline := time.Now().Add(5 * time.Second); !strings.Contains(output.String(), "Iris: notifications stopped:"); {
			if time.Now().After(deadline) {
				t.Fatalf("watch never announced stopped for ended execution: %q", output.String())
			}
			time.Sleep(10 * time.Millisecond)
		}
		stop()
		if err := <-done; err != ErrLost {
			t.Fatalf("expected ErrLost, got %v", err)
		}
	})
}
