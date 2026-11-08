package watch

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/binding"
	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/proc"
	"github.com/mateusz-klatt/iris/internal/store"
)

// A monitor whose generation is found lost, as after a laptop sleeps past its lease, says once
// that notifications stopped and keeps going: a plugin monitor that exits is not restarted, and
// the session would go deaf. When the session's hook has registered it again, the monitor binds
// to that Execution, never to the lost one and never to one of its own, says notifications
// resumed, and follows it from its own cursor (D9).
func TestAMonitorResumesOnTheSessionsNextExecution(t *testing.T) {
	h := newRestartableHub(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := h.store.Call(ctx, "sessions", store.Request{Client: "claude", Host: "host", SessionID: "s", Cwd: "/repo", Source: "hook"})
	if err != nil {
		t.Fatal(err)
	}
	first := store.Request{ExecutionID: out["execution_id"].(string), Generation: out["generation"].(int64)}
	resolver := &binding.Resolver{Hub: &hubclient.Client{Config: testConfig{h.server.URL}}, Client: "claude", Host: "host", CWD: "/repo",
		Getenv: func(k string) string {
			if k == "CLAUDE_CODE_SESSION_ID" {
				return "s"
			}
			return ""
		}, Ancestor: func() (proc.Ancestor, error) { return proc.Ancestor{}, nil }}
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	go resolver.Maintain(runCtx, "watch", 20*time.Millisecond, func() bool { return true }, func() { t.Error("the monitor was stopped") })
	output := &lines{}
	done := make(chan error, 1)
	go func() {
		done <- (&Runner{Resolver: resolver, Output: output, Backoff: 10 * time.Millisecond}).Run(runCtx)
	}()
	bound := func() string {
		b, err := resolver.Resolve(ctx, "")
		if err != nil {
			return ""
		}
		return b.ExecutionID
	}
	for deadline := time.Now().Add(5 * time.Second); bound() != first.ExecutionID; {
		if time.Now().After(deadline) {
			t.Fatal("never bound the first Execution")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// The session's Execution goes; the next heartbeat finds it lost. The monitor never
	// registers the session itself, which would split it from its hook, so it waits until
	// the hook has registered again.
	if _, err = h.store.Call(ctx, "end", first); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); !strings.Contains(output.String(), "Iris: notifications stopped: "); {
		if time.Now().After(deadline) {
			t.Fatalf("never said it stopped: %q", output.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err = h.store.Call(ctx, "sessions", store.Request{Client: "claude", Host: "host", SessionID: "s", Cwd: "/repo", Source: "hook"}); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); ; {
		select {
		case err := <-done:
			t.Fatalf("the monitor ended: %v, output %q", err, output.String())
		default:
		}
		if now := bound(); now != "" && now != first.ExecutionID {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("never bound afresh: output %q", output.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	for deadline := time.Now().Add(5 * time.Second); !strings.Contains(output.String(), "Iris: notifications resumed."); {
		if time.Now().After(deadline) {
			t.Fatalf("never said it resumed: %q", output.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := output.String(); strings.Count(got, "Iris: notifications stopped: ") != 1 || strings.Index(got, "stopped") > strings.Index(got, "resumed") {
		t.Fatalf("output %q", got)
	}
	stop()
	if err = <-done; err != nil {
		t.Fatalf("a resumed monitor ended with %v", err)
	}
}

// scriptedLossHub binds to ex1 until its heartbeat is lost, then offers ex1 again for a few
// binds, which the monitor must refuse and so announce the gap, and then ex2. The stream
// always refuses, so the monitor keeps going round its loop.
type scriptedLossHub struct {
	mu      sync.Mutex
	lost    bool
	refused int
}

func (h *scriptedLossHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	switch {
	case r.URL.Path == "/v1/bind":
		ex := "ex1"
		if h.lost {
			if h.refused++; h.refused > 3 {
				ex = "ex2"
			}
		}
		_, _ = w.Write([]byte(`{"execution_id":"` + ex + `","generation":1,"repository_id":"r","agent":{"name":"claude-host"}}`))
	case strings.HasSuffix(r.URL.Path, "/ex1/heartbeat"):
		h.lost = true
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":{"code":"lost","message":"gone"}}`))
	case strings.HasSuffix(r.URL.Path, "/heartbeat"):
		_, _ = w.Write([]byte(`{}`))
	default:
		w.WriteHeader(http.StatusServiceUnavailable)
	}
}

func sessionResolver(url string) *binding.Resolver {
	return &binding.Resolver{Hub: &hubclient.Client{Config: testConfig{url}}, Client: "claude", Host: "host", CWD: "/repo",
		Getenv: func(k string) string {
			if k == "CLAUDE_CODE_SESSION_ID" {
				return "s"
			}
			return ""
		}, Ancestor: func() (proc.Ancestor, error) { return proc.Ancestor{}, nil }}
}

// A gap the monitor announced is closed by saying notifications resumed, once, on the new
// Execution; a resumption line it cannot print ends it like any line it cannot print.
func TestAnAnnouncedGapEndsWithResumed(t *testing.T) {
	srv := httptest.NewServer(&scriptedLossHub{})
	defer srv.Close()
	resolver := sessionResolver(srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	go resolver.Maintain(ctx, "watch", 5*time.Millisecond, func() bool { return true }, func() { t.Error("stopped") })
	output := &lines{}
	done := make(chan error, 1)
	go func() { done <- (&Runner{Resolver: resolver, Output: output, Backoff: time.Millisecond}).Run(ctx) }()
	for deadline := time.Now().Add(10 * time.Second); !strings.Contains(output.String(), "Iris: notifications resumed."); {
		if time.Now().After(deadline) {
			t.Fatalf("never resumed: %q", output.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
	got := output.String()
	if strings.Count(got, "Iris: notifications stopped: ") != 1 || strings.Count(got, "Iris: notifications resumed.") != 1 || strings.Index(got, "stopped") > strings.Index(got, "resumed") {
		t.Fatalf("output %q", got)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("a resumed monitor ended with %v", err)
	}

	broken := httptest.NewServer(&scriptedLossHub{})
	defer broken.Close()
	resolver = sessionResolver(broken.URL)
	ctx2, cancel2 := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel2()
	go resolver.Maintain(ctx2, "watch", 5*time.Millisecond, func() bool { return true }, func() {})
	// The stop line prints; the resumption is the first write that fails.
	out := &failAfter{ok: 1}
	if err := (&Runner{Resolver: resolver, Output: out, Backoff: time.Millisecond}).Run(ctx2); err == nil || errors.Is(err, ErrLost) {
		t.Fatalf("a resumption it could not print: %v", err)
	}
}

// A loss the monitor had not yet seen when its caller ends it is said then, once.
func TestALossNotYetSeenIsSaidAtTheEnd(t *testing.T) {
	srv := httptest.NewServer(&scriptedLossHub{})
	defer srv.Close()
	resolver := sessionResolver(srv.URL)
	if _, err := resolver.Resolve(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go resolver.Maintain(ctx, "watch", time.Millisecond, func() bool { return true }, func() {})
	for deadline := time.Now().Add(5 * time.Second); !resolver.Lost(); {
		if time.Now().After(deadline) {
			t.Fatal("the heartbeat never found the loss")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	output := &lines{}
	if err := (&Runner{Resolver: resolver, Output: output}).Run(ctx); !errors.Is(err, ErrLost) || strings.Count(output.String(), "Iris: notifications stopped: ") != 1 {
		t.Fatalf("err %v, output %q", err, output.String())
	} // Said to an output that is gone, it ends with that failure rather than as cleanly lost.
	if err := (&Runner{Resolver: resolver, Output: &failAfter{}}).Run(ctx); err == nil || errors.Is(err, ErrLost) {
		t.Fatalf("a stop line it could not print: %v", err)
	}
}

// failAfter accepts ok writes, then fails every one.
type failAfter struct {
	mu sync.Mutex
	ok int
}

func (f *failAfter) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ok == 0 {
		return 0, errors.New("the client is gone")
	}
	f.ok--
	return len(p), nil
}
