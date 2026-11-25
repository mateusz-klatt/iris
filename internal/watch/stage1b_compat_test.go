package watch

import (
	"bytes"
	"context"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/binding"
	"github.com/mateusz-klatt/iris/internal/hub"
	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/proc"
	"github.com/mateusz-klatt/iris/internal/store"
)

type watchCompatClock struct {
	mu   sync.Mutex
	wall time.Time
	tick int64
}

func (c *watchCompatClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.wall
}

func (c *watchCompatClock) Tick() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.tick
}

func (c *watchCompatClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.wall = c.wall.Add(d)
	c.tick += int64(d / time.Millisecond)
}

type watchCompatConfig struct {
	url string
}

func (c watchCompatConfig) HubURL() string       { return c.url }
func (watchCompatConfig) Token() (string, error) { return "secret", nil }

func TestV0115WatchRunnerInertAgainstStage1bHub(t *testing.T) {
	clock := &watchCompatClock{wall: time.Unix(1800000000, 0), tick: 1000}
	s, err := store.Open(filepath.Join(t.TempDir(), "watch.db"), clock)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	srv := httptest.NewUnstartedServer(nil)
	srv.Config.Handler = hub.New(s, "secret", []string{srv.Listener.Addr().String()})
	srv.Start()
	defer srv.Close()

	hc := &hubclient.Client{Config: watchCompatConfig{url: srv.URL}}
	parent, err := hc.RegisterSession(t.Context(), hubclient.SessionRequest{
		Client: "claude", Host: "host", SessionID: "S-watch-1b",
		CWD: "/repo/w", Worktree: "/repo/w", Branch: "main", Source: "hook",
		ClientPID: 701, ClientPIDStartMS: 7701,
	})
	if err != nil {
		t.Fatal(err)
	}
	clock.Advance(16 * time.Minute)
	if _, err := s.Call(t.Context(), "expire", store.Request{}); err != nil {
		t.Fatal(err)
	}
	child, err := hc.Succeed(t.Context(), hubclient.SucceedRequest{
		Client: "claude", Host: "host", SessionID: "S-watch-1b", Source: "shim",
		PriorGeneration: parent.Generation, PriorExecutionID: parent.ExecutionID,
		ClientPID: 701, ClientPIDStartMS: 7701,
	})
	if err != nil {
		t.Fatal(err)
	}

	resolver := &binding.Resolver{
		Hub:    hc,
		Client: "claude",
		Host:   "host",
		CWD:    "/repo/w",
		Getenv: func(k string) string {
			if k == "CLAUDE_CODE_SESSION_ID" {
				return "S-watch-1b"
			}
			return ""
		},
		Ancestor: func() (proc.Ancestor, error) {
			return proc.Ancestor{PID: 701, StartMS: 7701, Name: "claude"}, nil
		},
		Grace: 5 * time.Millisecond,
	}

	ticks := make(chan time.Time, 16)
	frames := make(chan Frame, 8)
	var out bytes.Buffer
	runner := &Runner{
		Resolver: resolver,
		Output:   &out,
		Backoff:  5 * time.Millisecond,
		after: func(time.Duration) <-chan time.Time {
			ticks <- time.Now()
			ch := make(chan time.Time, 1)
			ch <- time.Now()
			return ch
		},
		received: func(f Frame) {
			frames <- f
		},
	}

	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- runner.Run(runCtx)
	}()

	// Wait through two retry ticks while the child is still hookless: watch.Runner must stay
	// unattached (never opening a stream or setting watch_seen_ms).
	for range 2 {
		select {
		case <-ticks:
		case f := <-frames:
			cancel()
			t.Fatalf("watch.Runner unexpectedly streamed hookless succession child: %+v", f)
		case <-time.After(5 * time.Second):
			cancel()
			t.Fatal("timed out waiting for watch backoff while hookless")
		}
	}
	var agents struct {
		Agents []struct {
			Name      string `json:"name"`
			WatchSeen any    `json:"watch_seen"`
		} `json:"agents"`
	}
	if err := hc.Do(t.Context(), "GET", "/v1/agents?all=1", nil, &agents); err != nil {
		cancel()
		t.Fatal(err)
	}
	for _, a := range agents.Agents {
		if a.WatchSeen != nil {
			cancel()
			t.Fatalf("watch_seen recorded before hook attach: %+v", a)
		}
	}

	// Genuine hook arrival unlocks the same child; watch.Runner binds and streams immediately.
	if _, err := hc.RegisterSession(t.Context(), hubclient.SessionRequest{
		Client: "claude", Host: "host", SessionID: "S-watch-1b",
		CWD: "/repo/w", Worktree: "/repo/w", Branch: "main", Source: "hook",
		ClientPID: 701, ClientPIDStartMS: 7701,
	}); err != nil {
		cancel()
		t.Fatal(err)
	}

	for deadline := time.Now().Add(5 * time.Second); ; {
		if b, err := resolver.Resolve(t.Context(), ""); err == nil && b.ExecutionID == child.ExecutionID && b.HookSeen {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("watch resolver did not bind unlocked child %s", child.ExecutionID)
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("watch runner exited with unexpected error: %v", err)
	}
}
