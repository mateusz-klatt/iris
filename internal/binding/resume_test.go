package binding

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/hub"
	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/proc"
	"github.com/mateusz-klatt/iris/internal/store"
)

// splitHub is a real hub, and the hook's registration for one Claude process.
type splitHub struct {
	s    *store.Store
	url  string
	hook store.Request
}

func newSplitHub(t *testing.T) *splitHub {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "hub.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	srv := httptest.NewUnstartedServer(nil)
	srv.Config.Handler = hub.New(s, "secret", []string{srv.Listener.Addr().String()})
	srv.Start()
	t.Cleanup(srv.Close)
	return &splitHub{s: s, url: srv.URL, hook: store.Request{Client: "claude", Host: "host", SessionID: "session-a", Cwd: "/repo", Source: "hook", ClientPID: 4242, ClientPIDStartMS: 77}}
}

// registerHook registers the session as its SessionStart hook does, under session A.
func (h *splitHub) registerHook(t *testing.T) string {
	t.Helper()
	out, err := h.s.Call(context.Background(), "sessions", h.hook)
	if err != nil {
		t.Fatal(err)
	}
	return out["execution_id"].(string)
}

// mcp is the MCP server of the same Claude process, which on resume was started with session B.
func (h *splitHub) mcp(grace time.Duration) *Resolver {
	return &Resolver{Hub: &hubclient.Client{Config: config{h.url}}, Client: "claude", Host: "host", CWD: "/repo", Grace: grace,
		Getenv: func(k string) string {
			if k == "CLAUDE_CODE_SESSION_ID" {
				return "session-b"
			}
			return ""
		}, Ancestor: func() (proc.Ancestor, error) { return proc.Ancestor{PID: 4242, StartMS: 77}, nil }}
}

func (h *splitHub) active(t *testing.T) int {
	t.Helper()
	agents, err := h.s.Call(context.Background(), "ui_overview", store.Request{})
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, a := range agents["agents"].([]map[string]any) {
		if a["state"] == "active" {
			n++
		}
	}
	return n
}

// One session is one Execution and one mailbox, whichever of its hook and its MCP server comes
// first, and whether or not its hooks run at all (D6).
func TestOneSessionIsOneExecutionWhateverComesFirst(t *testing.T) {
	ctx := context.Background()
	t.Run("the hook first", func(t *testing.T) {
		h := newSplitHub(t)
		hook := h.registerHook(t)
		b, err := h.mcp(time.Minute).Resolve(ctx, "")
		if err != nil || b.ExecutionID != hook || b.Rung != 3 || h.active(t) != 1 {
			t.Fatalf("bound %+v %v, the hook's is %s; %d active", b, err, hook, h.active(t))
		}
	})
	t.Run("the MCP first, the hook within the grace period", func(t *testing.T) {
		h := newSplitHub(t)
		r := h.mcp(10 * time.Second)
		got := make(chan Bound, 1)
		go func() {
			b, err := r.Resolve(ctx, "")
			if err != nil {
				t.Error(err)
			}
			got <- b
		}()
		time.Sleep(300 * time.Millisecond) // SessionStart lands after the MCP's first tool call
		hook := h.registerHook(t)
		if b := <-got; b.ExecutionID != hook || r.own || h.active(t) != 1 {
			t.Fatalf("bound %+v own %v, the hook's is %s; %d active", b, r.own, hook, h.active(t))
		}
	})
	t.Run("no hook, and one that comes after all", func(t *testing.T) {
		h := newSplitHub(t)
		r := h.mcp(200 * time.Millisecond)
		mine, err := r.Resolve(ctx, "")
		if err != nil || !r.own || h.active(t) != 1 {
			t.Fatalf("with no hook: %+v %v own %v", mine, err, r.own)
		}
		// Mail reaches the stray mailbox before the hook registers.
		sender, err := h.s.Call(ctx, "sessions", store.Request{Client: "codex", Host: "host", SessionID: "peer", Cwd: "/repo", Source: "hook"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = h.s.Call(ctx, "messages", store.Request{ExecutionID: sender["execution_id"].(string), Generation: sender["generation"].(int64), Key: "k", To: []string{mine.Agent.Name}, Subject: "while stray", Body: "b"}); err != nil {
			t.Fatal(err)
		}
		// A hook slower than the grace period: the heartbeat moves to its Execution, ends this
		// process's own, and hands it the stray mail, so the session is not left split.
		hook := h.registerHook(t)
		runCtx, stop := context.WithCancel(ctx)
		defer stop()
		go r.Maintain(runCtx, "shim", 10*time.Millisecond, func() bool { return true }, func() {})
		for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
			if b, err := r.Resolve(ctx, ""); err == nil && b.ExecutionID == hook {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("never moved to the hook's Execution")
			}
		}
		if r.own || h.active(t) != 2 { // the hook's and the sender's
			t.Fatalf("own %v; %d active, want the hook's and the sender's", r.own, h.active(t))
		}
		inbox, err := h.s.Call(ctx, "inbox", store.Request{ExecutionID: hook})
		if got := inbox["deliveries"].([]map[string]any); err != nil || len(got) != 1 || got[0]["subject"] != "while stray" {
			t.Fatalf("the hook's inbox after the move: %v %v", inbox, err)
		}
	})
}
