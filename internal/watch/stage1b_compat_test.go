package watch

import (
	"bufio"
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
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

type watchHijackSpy struct {
	http.ResponseWriter
	onHijack func()
}

func (w *watchHijackSpy) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *watchHijackSpy) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	c, rw, err := w.ResponseWriter.(http.Hijacker).Hijack()
	if err == nil && w.onHijack != nil {
		w.onHijack()
	}
	return c, rw, err
}

func TestV0115WatchRunnerInertAgainstStage1bHub(t *testing.T) {
	clock := &watchCompatClock{wall: time.Unix(1800000000, 0), tick: 1000}
	s, err := store.Open(filepath.Join(t.TempDir(), "watch.db"), clock)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	streamAccepted := make(chan struct{}, 1)
	srv := httptest.NewUnstartedServer(nil)
	h := hub.New(s, "secret", []string{srv.Listener.Addr().String()})
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/watch" {
			w = &watchHijackSpy{
				ResponseWriter: w,
				onHijack: func() {
					select {
					case streamAccepted <- struct{}{}:
					default:
					}
				},
			}
		}
		h.ServeHTTP(w, r)
	})
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

	var tries atomic.Int64
	var unlocked atomic.Bool
	permit := make(chan time.Time)
	tryEntered := make(chan struct{}, 1)
	frames := make(chan Frame, 8)
	var out bytes.Buffer
	runner := &Runner{
		Resolver: resolver,
		Output:   &out,
		Backoff:  5 * time.Millisecond,
		after: func(d time.Duration) <-chan time.Time {
			if unlocked.Load() {
				return time.After(d)
			}
			tries.Add(1)
			select {
			case tryEntered <- struct{}{}:
			default:
			}
			return permit
		},
		received: func(f Frame) {
			select {
			case frames <- f:
			default:
			}
		},
	}

	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- runner.Run(runCtx)
	}()
	stopRunner := sync.OnceValue(func() error {
		cancel()
		return <-done
	})
	defer func() { _ = stopRunner() }()

	waitTry := func(want int64) {
		t.Helper()
		timer := time.NewTimer(5 * time.Second)
		defer timer.Stop()
		for tries.Load() < want {
			select {
			case <-tryEntered:
			case f := <-frames:
				t.Fatalf("watch.Runner unexpectedly streamed hookless succession child: %+v", f)
			case <-timer.C:
				t.Fatalf("timed out waiting for watch backoff attempt %d (got %d)", want, tries.Load())
			}
		}
	}

	// Step through two retry waits using the cancellable barrier while the child is hookless:
	// Runner stays parked inside its select on ctx.Done vs permit with zero channel buildup.
	waitTry(1)
	select {
	case permit <- time.Now():
	case <-time.After(5 * time.Second):
		t.Fatal("timed out releasing retry #1 permit")
	}
	waitTry(2)

	var agents struct {
		Agents []struct {
			Name      string `json:"name"`
			WatchSeen any    `json:"watch_seen"`
		} `json:"agents"`
	}
	if err := hc.Do(t.Context(), http.MethodGet, "/v1/agents?all=1", nil, &agents); err != nil {
		t.Fatal(err)
	}
	for _, a := range agents.Agents {
		if a.WatchSeen != nil {
			t.Fatalf("watch_seen recorded before hook attach: %+v", a)
		}
	}
	select {
	case f := <-frames:
		t.Fatalf("unexpected frame before hook attach: %+v", f)
	case <-streamAccepted:
		t.Fatal("/v1/watch accepted before hook attach")
	default:
	}

	// Genuine hook arrival unlocks the same child; release the retry barrier and wait until
	// the runner itself binds, opens /v1/watch (post monitor_cursor), and receives a live frame.
	if _, err := hc.RegisterSession(t.Context(), hubclient.SessionRequest{
		Client: "claude", Host: "host", SessionID: "S-watch-1b",
		CWD: "/repo/w", Worktree: "/repo/w", Branch: "main", Source: "hook",
		ClientPID: 701, ClientPIDStartMS: 7701,
	}); err != nil {
		t.Fatal(err)
	}
	unlocked.Store(true)
	close(permit)

	select {
	case <-streamAccepted:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for watch.Runner to open /v1/watch after hook unlock")
	}

	sender, err := hc.RegisterSession(t.Context(), hubclient.SessionRequest{
		Client: "codex", Host: "host", SessionID: "S-watch-sender",
		CWD: "/repo/w", Worktree: "/repo/w", Branch: "main", Source: "hook",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := hc.Do(t.Context(), http.MethodPost, "/v1/messages", map[string]any{
		"execution_id": sender.ExecutionID,
		"generation":   sender.Generation,
		"key":          "watch-unlock-frame",
		"to":           []string{child.Agent.Name},
		"subject":      "wake check",
		"body":         "frame delivery check",
	}, &struct{}{}); err != nil {
		t.Fatal(err)
	}

	select {
	case f := <-frames:
		if f.Kind != "inbox_changed" {
			t.Fatalf("unexpected frame kind %q after unlock: %+v", f.Kind, f)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for watch.Runner to receive frame after hook unlock")
	}

	if err := stopRunner(); err != nil {
		t.Fatalf("watch runner exited with unexpected error: %v", err)
	}
}
