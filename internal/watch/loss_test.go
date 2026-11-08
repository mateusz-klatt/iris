package watch

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/binding"
	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/proc"
	"github.com/mateusz-klatt/iris/internal/store"
)

// deadOutput is a monitor whose process is gone: its output can no longer be written.
type deadOutput struct{}

func (deadOutput) Write([]byte) (int, error) { return 0, errors.New("the client is gone") }

// Gap review B2: reconnect tests kept one Runner, with its memory. Here the monitor process is
// lost after the hub persisted the event: once before it could print the line, and once while
// it held the frame in its coalescing window. A new monitor for the same session, started with
// nothing else happening on the hub, must still say there is mail.
func TestANewMonitorGetsTheWakeTheLostOneMissed(t *testing.T) {
	for _, tc := range []struct {
		name   string
		dies   bool // the output fails, as a lost client's does
		window time.Duration
	}{
		{"lost before printing", true, 0},
		{"lost while holding a frame", false, 5 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newRestartableHub(t)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			out, err := h.store.Call(ctx, "sessions", store.Request{Client: "claude", Host: "host", SessionID: "s", Cwd: "/repo", Source: "hook"})
			if err != nil {
				t.Fatal(err)
			}
			execution := out["execution_id"].(string)
			recipient := out["agent"].(map[string]any)["name"].(string)
			sender, err := h.store.Call(ctx, "sessions", store.Request{Client: "codex", Host: "host", SessionID: "sender", Cwd: "/repo2", Source: "hook"})
			if err != nil {
				t.Fatal(err)
			}
			monitor := func(run context.Context, output interface {
				Write([]byte) (int, error)
			}, window time.Duration) chan error {
				resolver := &binding.Resolver{Hub: &hubclient.Client{Config: testConfig{h.server.URL}}, Client: "claude", Host: "host", CWD: "/repo",
					Getenv: func(k string) string {
						if k == "IRIS_EXECUTION" {
							return execution
						}
						return ""
					}, Ancestor: func() (proc.Ancestor, error) { return proc.Ancestor{}, nil }}
				done := make(chan error, 1)
				go func() {
					done <- (&Runner{Resolver: resolver, Output: output, Window: window, Backoff: 10 * time.Millisecond}).Run(run)
				}()
				return done
			}

			firstCtx, kill := context.WithCancel(ctx)
			var first chan error
			firstOutput := &lines{}
			if tc.dies {
				first = monitor(firstCtx, deadOutput{}, 0)
			} else {
				first = monitor(firstCtx, firstOutput, tc.window)
			}
			time.Sleep(300 * time.Millisecond) // attached, and placed at the newest event
			send := func(key string) {
				if _, err := h.store.Call(ctx, "messages", store.Request{ExecutionID: sender["execution_id"].(string), Generation: sender["generation"].(int64), Key: key, To: []string{recipient}, Subject: key, Body: "b"}); err != nil {
					t.Fatal(err)
				}
			}
			send("first")
			if tc.dies {
				// It dies on the print, with no cursor saved for the line it never wrote.
				select {
				case err := <-first:
					if err == nil {
						t.Fatal("a monitor that could not print ended cleanly")
					}
				case <-time.After(10 * time.Second):
					t.Fatal("the first monitor never tried to print")
				}
			} else {
				// The first line prints at once and opens the window. Mail inside it is counted
				// and held for the window's end, so the cursor stays before it.
				for !strings.Contains(firstOutput.String(), "Iris: 1 new message") {
					time.Sleep(10 * time.Millisecond)
				}
				send("second")
				time.Sleep(300 * time.Millisecond)
				kill()
				if err := <-first; err != nil {
					t.Fatalf("a cancelled monitor ended with %v", err)
				}
				if got := firstOutput.String(); strings.Count(got, "\n") != 1 {
					t.Fatalf("the held line was printed: %q", got)
				}
			}
			kill()

			output := &lines{}
			secondCtx, stop := context.WithCancel(ctx)
			defer stop()
			second := monitor(secondCtx, output, 0)
			deadline := time.Now().Add(10 * time.Second)
			for !strings.Contains(output.String(), "Iris:") && time.Now().Before(deadline) {
				time.Sleep(20 * time.Millisecond)
			}
			stop()
			if err := <-second; err != nil {
				t.Fatalf("a cancelled monitor ended with %v", err)
			}
			if !strings.Contains(output.String(), "message") {
				t.Fatalf("the new monitor said %q, not that mail is waiting", output.String())
			}
		})
	}
}
