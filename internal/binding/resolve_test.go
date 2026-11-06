package binding

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"regexp"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/proc"
)

// scriptedHub answers /v1/bind with codes in order and records every request.
type scriptedHub struct {
	binds    []string // error code, or "" for success
	register string   // error code for /v1/sessions, or ""
	bound    []map[string]any
	session  *hubclient.SessionRequest
}

func (h *scriptedHub) fail(w http.ResponseWriter, code string) {
	w.WriteHeader(http.StatusConflict)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": code, "message": code}})
}

func (h *scriptedHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/v1/bind":
		var got map[string]any
		_ = json.NewDecoder(r.Body).Decode(&got)
		h.bound = append(h.bound, got)
		code := ""
		if len(h.binds) > 0 {
			code, h.binds = h.binds[0], h.binds[1:]
		}
		if code != "" {
			h.fail(w, code)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"execution_id": "ex_bound", "generation": 3})
	case "/v1/sessions":
		var got hubclient.SessionRequest
		_ = json.NewDecoder(r.Body).Decode(&got)
		h.session = &got
		if h.register != "" {
			h.fail(w, h.register)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"execution_id": "ex_new", "generation": 1})
	default:
		http.NotFound(w, r)
	}
}

func resolverFor(t *testing.T, hub *scriptedHub, client string, env map[string]string, ancestor proc.Ancestor) *Resolver {
	t.Helper()
	srv := httptest.NewServer(hub)
	t.Cleanup(srv.Close)
	return &Resolver{
		Hub:      &hubclient.Client{Config: config{srv.URL}},
		Client:   client,
		Host:     "host",
		CWD:      t.TempDir(),
		Getenv:   func(k string) string { return env[k] },
		Ancestor: func() (proc.Ancestor, error) { return ancestor, nil },
	}
}

func TestNewDescribesThisProcess(t *testing.T) {
	r := New(&hubclient.Client{}, "codex")
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if r.Client != "codex" || r.CWD != cwd || r.Host == "" || r.Getenv == nil || r.Ancestor == nil {
		t.Fatalf("%+v", r)
	}
}

func TestKeyIsRandomHex(t *testing.T) {
	first, second := Key(), Key()
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(first) || first == second {
		t.Fatal(first, second)
	}
}

func TestBoundExecutionIsReused(t *testing.T) {
	hub := &scriptedHub{}
	r := resolverFor(t, hub, "codex", map[string]string{"IRIS_EXECUTION": "ex_bound"}, proc.Ancestor{})
	ctx := context.Background()
	b, err := r.Resolve(ctx, "")
	if err != nil || b.Rung != 1 {
		t.Fatalf("%+v %v", b, err)
	}
	for _, handle := range []string{"", "ex_bound"} {
		if again, err := r.Resolve(ctx, handle); err != nil || again.ExecutionID != b.ExecutionID {
			t.Fatalf("%+v %v", again, err)
		}
	}
	if len(hub.bound) != 1 {
		t.Fatalf("asked the hub %d times", len(hub.bound))
	}
	if _, err := r.Resolve(ctx, "ex_other"); err != nil {
		t.Fatal(err)
	}
	if len(hub.bound) != 2 {
		t.Fatal("another handle must be asked about")
	}
}

func TestSessionVariableBinds(t *testing.T) {
	env := map[string]string{"CLAUDE_CODE_SESSION_ID": "s1"}
	hub := &scriptedHub{}
	b, err := resolverFor(t, hub, "claude", env, proc.Ancestor{}).Resolve(context.Background(), "")
	if err != nil || b.Rung != 2 || hub.bound[0]["session_id"] != "s1" {
		t.Fatalf("%+v %v %v", b, err, hub.bound)
	}
}

func TestUnknownSessionRegisters(t *testing.T) {
	env := map[string]string{"GROK_SESSION_ID": "s2"}
	hub := &scriptedHub{binds: []string{"unbound"}}
	b, err := resolverFor(t, hub, "grok", env, proc.Ancestor{}).Resolve(context.Background(), "")
	if err != nil || b.ExecutionID != "ex_new" || b.Rung != 2 {
		t.Fatalf("%+v %v", b, err)
	}
	if hub.session.SessionID != "s2" || hub.session.Source != "mcp" || hub.session.SingleSession || hub.session.Worktree != "" {
		t.Fatalf("%+v", hub.session)
	}
}

func TestRegisteredSessionNamesItsRepository(t *testing.T) {
	r := resolverFor(t, &scriptedHub{binds: []string{"unbound"}}, "grok", map[string]string{"GROK_SESSION_ID": "s2"}, proc.Ancestor{})
	if out, err := exec.Command("git", "-C", r.CWD, "init", "-q", "-b", "trunk").CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	hub := &scriptedHub{binds: []string{"unbound"}}
	srv := httptest.NewServer(hub)
	defer srv.Close()
	r.Hub = &hubclient.Client{Config: config{srv.URL}}
	if _, err := r.Resolve(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if hub.session.Worktree == "" || hub.session.CommonDir == "" || hub.session.Branch != "trunk" {
		t.Fatalf("%+v", hub.session)
	}
}

func TestSessionFailuresAreNotMasked(t *testing.T) {
	env := map[string]string{"COPILOT_AGENT_SESSION_ID": "s3"}
	hub := &scriptedHub{binds: []string{"denied"}}
	if _, err := resolverFor(t, hub, "copilot", env, proc.Ancestor{}).Resolve(context.Background(), ""); !IsCode(err, "denied") {
		t.Fatal(err)
	}
	if hub.session != nil {
		t.Fatal("registered after a refusal")
	}
	failing := &scriptedHub{binds: []string{"unbound"}, register: "denied"}
	if _, err := resolverFor(t, failing, "copilot", env, proc.Ancestor{}).Resolve(context.Background(), ""); !IsCode(err, "denied") {
		t.Fatal(err)
	}
}

func TestLostResolverNeverRebinds(t *testing.T) {
	hub := &scriptedHub{}
	r := resolverFor(t, hub, "codex", map[string]string{"IRIS_EXECUTION": "ex"}, proc.Ancestor{})
	r.lost = true
	if _, err := r.Resolve(context.Background(), ""); !IsCode(err, "lost") {
		t.Fatal(err)
	}
	if len(hub.bound) != 0 {
		t.Fatal("asked the hub")
	}
}

func TestAncestorEvidence(t *testing.T) {
	ctx := context.Background()
	single := proc.Ancestor{PID: 42, StartMS: 100, Name: "agy"}
	t.Run("hub failure stops the walk", func(t *testing.T) {
		hub := &scriptedHub{binds: []string{"denied"}}
		if _, err := resolverFor(t, hub, "antigravity", nil, single).Resolve(ctx, "ex_h"); !IsCode(err, "denied") {
			t.Fatal(err)
		}
		if len(hub.bound) != 1 {
			t.Fatal("kept asking after a refusal")
		}
	})
	t.Run("handle settles an unbound process", func(t *testing.T) {
		hub := &scriptedHub{binds: []string{"unbound"}}
		b, err := resolverFor(t, hub, "antigravity", nil, single).Resolve(ctx, "ex_h")
		if err != nil || b.Rung != 4 || hub.bound[1]["execution"] != "ex_h" {
			t.Fatalf("%+v %v %v", b, err, hub.bound)
		}
	})
	t.Run("single session client registers itself", func(t *testing.T) {
		hub := &scriptedHub{binds: []string{"unbound"}}
		b, err := resolverFor(t, hub, "antigravity", nil, single).Resolve(ctx, "")
		if err != nil || b.ExecutionID != "ex_new" || b.Rung != 3 {
			t.Fatalf("%+v %v", b, err)
		}
		if hub.session.SessionID != "mcp-42-100" || !hub.session.SingleSession || hub.session.ClientPID != 42 || hub.session.ClientPIDStartMS != 100 {
			t.Fatalf("%+v", hub.session)
		}
	})
}

func TestWithoutEvidenceOnlyAHandleBinds(t *testing.T) {
	ctx := context.Background()
	for name, ancestor := range map[string]proc.Ancestor{"no process": {}, "no start time": {PID: 42}} {
		t.Run(name, func(t *testing.T) {
			hub := &scriptedHub{}
			b, err := resolverFor(t, hub, "codex", nil, ancestor).Resolve(ctx, "ex_h")
			if err != nil || b.Rung != 4 {
				t.Fatalf("%+v %v", b, err)
			}
			if _, err := resolverFor(t, &scriptedHub{}, "codex", nil, ancestor).Resolve(ctx, ""); !IsCode(err, "unbound") {
				t.Fatal(err)
			}
		})
	}
}

func TestMaintainStopsWithItsContext(t *testing.T) {
	r := &Resolver{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		r.Maintain(ctx, "shim", time.Hour, func() bool { return true }, func() { t.Error("stopped as an orphan") })
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("kept running")
	}
}

func TestMaintainEndsQuietlyOnceLost(t *testing.T) {
	r := &Resolver{lost: true, bound: Bound{ExecutionID: "ex"}}
	done := make(chan struct{})
	go func() {
		r.Maintain(context.Background(), "shim", time.Millisecond, func() bool { return true }, func() { t.Error("second stop") })
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("kept running")
	}
}

func TestMaintainSurvivesAnUnboundTickAndAHubFailure(t *testing.T) {
	ticks := make(chan struct{}, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		ticks <- struct{}{}
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	r := &Resolver{Hub: &hubclient.Client{Config: config{srv.URL}}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	parent := make(chan struct{}, 1)
	alive := func() bool {
		select {
		case parent <- struct{}{}:
		default:
		}
		return true
	}
	done := make(chan struct{})
	go func() {
		r.Maintain(ctx, "shim", time.Millisecond, alive, func() { t.Error("a hub failure stopped the session") })
		close(done)
	}()
	<-parent // At least one tick has nothing to renew.
	r.mu.Lock()
	r.bound = Bound{ExecutionID: "ex", Generation: 1}
	r.mu.Unlock()
	for range 2 {
		select {
		case <-ticks:
		case <-time.After(10 * time.Second):
			t.Fatal("no heartbeat after a failure")
		}
	}
	cancel()
	<-done
}

func TestIsCodeNeedsAHubError(t *testing.T) {
	if IsCode(errors.New("lost"), "lost") {
		t.Fatal("plain error")
	}
}
