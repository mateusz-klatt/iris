package binding

import (
	"context"
	"encoding/json"
	"github.com/mateusz-klatt/iris/internal/hub"
	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/proc"
	"github.com/mateusz-klatt/iris/internal/store"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

type config struct{ url string }

func (c config) HubURL() string         { return c.url }
func (c config) Token() (string, error) { return "secret", nil }
func TestEvidenceRungs(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "hub.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	srv := httptest.NewUnstartedServer(nil)
	srv.Config.Handler = hub.New(s, "secret", []string{srv.Listener.Addr().String()})
	srv.Start()
	defer srv.Close()
	ctx := context.Background()
	register := func(id string, single bool) string {
		out, err := s.Call(ctx, "sessions", store.Request{Client: "codex", SessionID: id, Host: "host", Cwd: "/repo", ClientPID: 42, ClientPIDStartMS: 100, SingleSession: single})
		if err != nil {
			t.Fatal(err)
		}
		return out["execution_id"].(string)
	}
	first := register("one", false)
	resolver := func(env map[string]string) *Resolver {
		return &Resolver{Hub: &hubclient.Client{Config: config{srv.URL}}, Client: "codex", Host: "host", CWD: "/repo", Getenv: func(k string) string { return env[k] }, Ancestor: func() (proc.Ancestor, error) { return proc.Ancestor{PID: 42, StartMS: 100, Name: "codex"}, nil }}
	}
	if _, err = resolver(nil).Resolve(ctx, ""); !IsCode(err, "unbound") {
		t.Fatalf("multiplexed process bound: %v", err)
	}
	b, err := resolver(map[string]string{"IRIS_EXECUTION": first, "CLAUDE_CODE_SESSION_ID": "leak"}).Resolve(ctx, "")
	if err != nil || b.Rung != 1 {
		t.Fatalf("rung1: %+v %v", b, err)
	}
	b, err = resolver(nil).Resolve(ctx, first)
	if err != nil || b.Rung != 4 {
		t.Fatalf("rung4: %+v %v", b, err)
	}
	second := register("two", true)
	b, err = resolver(nil).Resolve(ctx, "")
	if err != nil || b.ExecutionID != second || b.Rung != 3 {
		t.Fatalf("rung3: %+v %v", b, err)
	}
	register("three", true)
	if _, err = resolver(nil).Resolve(ctx, ""); !IsCode(err, "ambiguous") {
		t.Fatalf("ambiguous: %v", err)
	}
	b, err = resolver(nil).Resolve(ctx, first)
	if err != nil || b.ExecutionID != first {
		t.Fatal(b, err)
	}
}
func TestOrphanStopsBeforeRenewal(t *testing.T) {
	// A bound generation pointed at an unreachable hub must still stop immediately
	// when its original parent disappears, rather than attempting a heartbeat.
	r := &Resolver{Hub: &hubclient.Client{}, bound: Bound{ExecutionID: "ex", Generation: 1}}
	stopped := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Maintain(ctx, "shim", time.Millisecond, func() bool { return false }, func() { close(stopped) })
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("orphan kept running")
	}
}

func TestRealOrphanExits(t *testing.T) {
	mode := os.Getenv("IRIS_ORPHAN_TEST")
	if mode == "parent" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestRealOrphanExits$")
		cmd.Env = append(os.Environ(), "IRIS_ORPHAN_TEST=child")
		if cmd.Start() != nil {
			os.Exit(3)
		}
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(os.Getenv("IRIS_ORPHAN_PATH") + ".ready"); err == nil {
				os.Exit(0)
			}
			time.Sleep(time.Millisecond)
		}
		os.Exit(4)
	}
	if mode == "child" {
		parent, err := proc.WatchParent()
		if err != nil {
			os.Exit(5)
		}
		defer parent.Close()
		marker := os.Getenv("IRIS_ORPHAN_PATH")
		_ = os.WriteFile(marker+".ready", nil, 0600)
		r := &Resolver{}
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		defer cancel()
		r.Maintain(ctx, "shim", time.Millisecond, parent.Alive, func() { _ = os.WriteFile(marker+".stopped", nil, 0600) })
		os.Exit(0)
	}
	marker := filepath.Join(t.TempDir(), "orphan")
	cmd := exec.Command(os.Args[0], "-test.run=^TestRealOrphanExits$")
	cmd.Env = append(os.Environ(), "IRIS_ORPHAN_TEST=parent", "IRIS_ORPHAN_PATH="+marker)
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(marker + ".stopped"); err == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("orphan did not stop after actual parent exit")
}

// A heartbeat that finds the generation gone drops the binding, and the process keeps
// running: an MCP server or monitor that exits after a laptop sleep is not restarted by its
// client, which then shows the server as failed, and the session goes deaf.
func TestALostHeartbeatDropsTheBindingAndKeepsRunning(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		var got map[string]any
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Error(err)
		}
		if got["source"] != "shim" || got["generation"] != float64(7) {
			t.Errorf("heartbeat: %v", got)
		}
		w.WriteHeader(409)
		_, _ = w.Write([]byte(`{"error":{"code":"lost","message":"generation lost"}}`))
	}))
	defer srv.Close()
	resolver := &Resolver{Hub: &hubclient.Client{Config: config{srv.URL}}, bound: Bound{ExecutionID: "ex", Generation: 7}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		resolver.Maintain(ctx, "shim", time.Millisecond, func() bool { return true }, func() { t.Error("a lost generation stopped the process") })
		close(done)
	}()
	deadline := time.Now().Add(time.Second)
	for !resolver.Lost() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	select {
	case <-done:
		t.Fatal("Maintain ended on a lost generation")
	default:
	}
	if !resolver.Lost() || requests.Load() != 1 {
		t.Fatalf("lost %v after %d heartbeats: the lost generation must not be renewed", resolver.Lost(), requests.Load())
	}
	cancel()
	<-done
}
