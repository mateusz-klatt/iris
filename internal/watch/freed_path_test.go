package watch

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/binding"
	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/proc"
	"github.com/mateusz-klatt/iris/internal/store"
)

// A session's first monitor prints a path freed for it before it started, and the mail waiting
// with it, once each. A replacement monitor that starts from the cursor the first one saved prints
// neither again. The replay announces the mail, so no reset counts it a second time.
func TestAFirstMonitorPrintsAPathFreedBeforeItOnce(t *testing.T) {
	h := newRestartableHub(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	gens := map[string]int64{}
	register := func(session string) (string, string) {
		out, err := h.store.Call(ctx, "sessions", store.Request{Client: "claude", Host: "host", SessionID: session, Cwd: "/repo", Source: "hook"})
		if err != nil {
			t.Fatal(err)
		}
		ex := out["execution_id"].(string)
		gens[ex] = out["generation"].(int64)
		return ex, out["agent"].(map[string]any)["name"].(string)
	}
	holder, _ := register("holder")
	waiter, waiterName := register("waiter")
	do := func(op string, r store.Request) map[string]any {
		r.Generation = gens[r.ExecutionID]
		out, err := h.store.Call(ctx, op, r)
		if err != nil {
			t.Fatalf("%s: %v", op, err)
		}
		return out
	}
	do("messages", store.Request{ExecutionID: holder, Key: "early", Body: "x", To: []string{waiterName}})
	do("touch", store.Request{ExecutionID: holder, ToolCallID: "h", Paths: []string{"a.go"}})
	if out := do("touch", store.Request{ExecutionID: waiter, ToolCallID: "w", Paths: []string{"a.go"}}); out["action"] != "deny" {
		t.Fatal(out)
	}
	do("release", store.Request{ExecutionID: holder, All: true})
	// The first monitor stops only once it has saved the cursor of the claim_available frame. It
	// prints a line before it saves the cursor behind it, and it reports a frame as received only
	// after that save, so the frame's arrival alone is not enough.
	var freedAt, saved atomic.Int64

	watchOnce := func(until func(string) bool) string {
		resolver := &binding.Resolver{Hub: &hubclient.Client{Config: testConfig{h.server.URL}}, Client: "claude", Host: "host", CWD: "/repo",
			Getenv: func(k string) string {
				if k == "IRIS_EXECUTION" {
					return waiter
				}
				return ""
			}, Ancestor: func() (proc.Ancestor, error) { return proc.Ancestor{}, nil }}
		runCtx, stop := context.WithCancel(ctx)
		defer stop()
		go resolver.Maintain(runCtx, "watch", 50*time.Millisecond, func() bool { return true }, stop)
		output := &lines{}
		done := make(chan error, 1)
		go func() {
			done <- (&Runner{Resolver: resolver, Output: output, Backoff: 10 * time.Millisecond,
				received: func(f Frame) {
					if f.Kind == "claim_available" {
						freedAt.Store(f.Cursor)
					}
				},
				checkpointed: func(c int64) { saved.Store(max(saved.Load(), c)) }}).Run(runCtx)
		}()
		deadline := time.Now().Add(10 * time.Second)
		for !until(output.String()) && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
		stop()
		if err := <-done; err != nil {
			t.Errorf("watch ended with %v", err)
		}
		return output.String()
	}
	const mail, freed = "Iris: 1 new message. Use the inbox tool.\n", "Iris: a path you were refused is free now. Use the claims tool.\n"
	first := watchOnce(func(s string) bool {
		return strings.Contains(s, mail) && freedAt.Load() > 0 && saved.Load() >= freedAt.Load()
	})
	if strings.Count(first, mail) != 1 || strings.Count(first, freed) != 1 {
		t.Fatalf("first monitor printed %q", first)
	}
	settle := time.Now().Add(time.Second)
	if again := watchOnce(func(string) bool { return time.Now().After(settle) }); again != "" {
		t.Fatalf("replacement monitor printed %q", again)
	}
}
