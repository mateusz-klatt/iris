package watch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/binding"
	"github.com/mateusz-klatt/iris/internal/hub"
	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/proc"
	"github.com/mateusz-klatt/iris/internal/store"
)

// restartableHub serves a real hub over one address and can restart it on the same database,
// as a deploy does.
type restartableHub struct {
	t       *testing.T
	file    string
	server  *httptest.Server
	current atomic.Pointer[hub.Hub]
	store   *store.Store
}

func newRestartableHub(t *testing.T) *restartableHub {
	t.Helper()
	h := &restartableHub{t: t, file: filepath.Join(t.TempDir(), "hub.db")}
	h.server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cur := h.current.Load()
		if cur == nil {
			http.Error(w, "restarting", http.StatusServiceUnavailable)
			return
		}
		cur.ServeHTTP(w, r)
	}))
	h.server.Start()
	h.open()
	t.Cleanup(func() { h.server.Close(); _ = h.store.Close() })
	return h
}

func (h *restartableHub) open() {
	h.t.Helper()
	s, err := store.Open(h.file, nil)
	if err != nil {
		h.t.Fatal(err)
	}
	h.store = s
	real := hub.New(s, "token", []string{h.server.Listener.Addr().String()})
	real.PingInterval = 20 * time.Millisecond
	h.current.Store(real)
}

// restart stops the hub, closes the database, and opens both again.
func (h *restartableHub) restart() {
	h.t.Helper()
	h.current.Store(nil)
	h.server.CloseClientConnections()
	if err := h.store.Close(); err != nil {
		h.t.Fatal(err)
	}
	h.open()
}

// A monitor survives a hub restart: while the session recovers, the stream and its reads answer
// lost, which the monitor retries, and its heartbeat reattaches the session. Before, any lost
// answer ended iris watch with exit 0 and no line (measured in a deploy on macOS).
func TestAMonitorSurvivesAHubRestart(t *testing.T) {
	for _, hookGen := range []int64{-1, 1, 0} {
		t.Run(map[int64]string{-1: "no hook", 1: "hook with proof", 0: "hook without proof"}[hookGen], func(t *testing.T) { surviveRestart(t, hookGen) })
	}
}

func surviveRestart(t *testing.T, hookGen int64) {
	h := newRestartableHub(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	reg := store.Request{Client: "claude", Host: "host", SessionID: "s", Cwd: "/repo", Source: "hook"}
	out, err := h.store.Call(ctx, "sessions", reg)
	if err != nil {
		t.Fatal(err)
	}
	execution := out["execution_id"].(string)
	resolver := &binding.Resolver{Hub: &hubclient.Client{Config: testConfig{h.server.URL}}, Client: "claude", Host: "host", CWD: "/repo",
		Getenv: func(k string) string {
			if k == "IRIS_EXECUTION" {
				return execution
			}
			return ""
		}, Ancestor: func() (proc.Ancestor, error) { return proc.Ancestor{}, nil }}
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	go resolver.Maintain(runCtx, "watch", 50*time.Millisecond, func() bool { return true }, stop)
	output := &lines{}
	done := make(chan error, 1)
	go func() {
		done <- (&Runner{Resolver: resolver, Output: output, Backoff: 10 * time.Millisecond}).Run(runCtx)
	}()
	time.Sleep(200 * time.Millisecond)
	h.restart()
	if hookGen >= 0 {
		again := reg
		again.Generation = hookGen
		_, _ = h.store.Call(ctx, "sessions", again) // without proof it is refused, and the heartbeat reattaches instead
	}
	select {
	case err := <-done:
		t.Fatalf("watch ended across a restart: err %v, output %q, lost %v", err, output.String(), resolver.Lost())
	case <-time.After(time.Second):
	}
	if resolver.Lost() || output.String() != "" {
		t.Fatalf("lost %v, output %q", resolver.Lost(), output.String())
	}
}
