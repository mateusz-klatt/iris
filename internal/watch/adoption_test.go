package watch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/binding"
	"github.com/mateusz-klatt/iris/internal/hub"
	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/proc"
	"github.com/mateusz-klatt/iris/internal/store"
)

type adoptionRoundTrip func(*http.Request) (*http.Response, error)

func (f adoptionRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Keeping an unconfirmed fallback alive must not pin a monitor to its old socket:
// the hook's mailbox needs attention even while every handover attempt fails.
func TestAMonitorFollowsAdoptionBeforeHandoverIsConfirmed(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, err := store.Open(filepath.Join(t.TempDir(), "hub.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	var ends atomic.Int32
	server := httptest.NewUnstartedServer(nil)
	real := hub.New(s, "token", []string{server.Listener.Addr().String()})
	server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/end") {
			ends.Add(1)
			http.Error(w, "handover unavailable", http.StatusServiceUnavailable)
			return
		}
		real.ServeHTTP(w, r)
	})
	server.Start()
	defer server.Close()
	opened := make(chan string, 4)
	client := &hubclient.Client{Config: testConfig{server.URL}, HTTP: &http.Client{Transport: adoptionRoundTrip(func(req *http.Request) (*http.Response, error) {
		response, err := http.DefaultTransport.RoundTrip(req)
		if err == nil && req.URL.Path == "/v1/watch" && response.StatusCode == http.StatusSwitchingProtocols {
			opened <- req.URL.Query().Get("execution_id")
		}
		return response, err
	})}}
	resolver := &binding.Resolver{Hub: client, Client: "claude", Host: "host", CWD: t.TempDir(), Getenv: func(key string) string {
		if key == "CLAUDE_CODE_SESSION_ID" {
			return "fallback"
		}
		return ""
	}, Ancestor: func() (proc.Ancestor, error) { return proc.Ancestor{PID: 42, StartMS: 77}, nil }}
	fallback, err := resolver.Resolve(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	output := &lines{}
	checkpoint := make(chan struct{}, 4)
	runner := &Runner{Resolver: resolver, Output: output, Backoff: time.Millisecond, checkpointed: func(int64) { checkpoint <- struct{}{} }}
	run, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- runner.Run(run) }()
	defer func() {
		stop()
		if err := <-done; err != nil {
			t.Errorf("monitor ended after adoption: %v", err)
		}
	}()
	select {
	case ex := <-opened:
		if ex != fallback.ExecutionID {
			t.Fatalf("initial socket %s, want fallback %s", ex, fallback.ExecutionID)
		}
	case <-ctx.Done():
		t.Fatal("the fallback socket never opened")
	}
	call := func(op string, req store.Request) map[string]any {
		t.Helper()
		out, err := s.Call(ctx, op, req)
		if err != nil {
			t.Fatalf("%s: %v", op, err)
		}
		return out
	}
	hook := call("sessions", store.Request{Client: "claude", Host: "host", SessionID: "hook", Cwd: resolver.CWD, Source: "hook", ClientPID: 42, ClientPIDStartMS: 77})
	target := hook["execution_id"].(string)
	sender := call("sessions", store.Request{Client: "codex", Host: "host", SessionID: "sender", Cwd: resolver.CWD})
	call("messages", store.Request{ExecutionID: sender["execution_id"].(string), Generation: 1, Key: "target-mail", To: []string{hook["agent"].(map[string]any)["name"].(string)}, Body: "mail for the hook"})
	maintained := make(chan struct{})
	go func() {
		defer close(maintained)
		resolver.Maintain(run, "watch", 20*time.Millisecond, func() bool { return true }, func() { t.Error("live parent was stopped") })
	}()
	defer func() { stop(); <-maintained }()
	select {
	case ex := <-opened:
		if ex != target {
			t.Fatalf("adopted socket %s, want hook %s", ex, target)
		}
	case <-ctx.Done():
		t.Fatalf("monitor stayed on the fallback socket during failed handover: %d attempts, output %q", ends.Load(), output.String())
	}
	select {
	case <-checkpoint:
	case <-ctx.Done():
		t.Fatalf("adopted mailbox was not announced: %q", output.String())
	}
	if got := output.String(); got != "Iris: 1 new message. Use the inbox tool.\n" {
		t.Fatalf("adoption emitted unrelated transitions or missed mail: %q", got)
	}
	if ends.Load() == 0 || resolver.Lost() {
		t.Fatalf("handover was not pending on a live binding: attempts %d, lost %v", ends.Load(), resolver.Lost())
	}
	call("validate", store.Request{ExecutionID: fallback.ExecutionID, Generation: int64(fallback.Generation)})
	call("validate", store.Request{ExecutionID: target, Generation: 1})
}
