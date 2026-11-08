package binding

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
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
	if len(hub.bound) != 3 {
		t.Fatalf("asked the hub %d times", len(hub.bound))
	}
	for _, request := range hub.bound[1:] {
		if request["execution"] != b.ExecutionID || request["session_id"] != nil || request["client_pid"] != nil {
			t.Fatalf("refresh repeated inference instead of checking the bound Execution: %v", request)
		}
	}
	if _, err := r.Resolve(ctx, "ex_other"); err != nil {
		t.Fatal(err)
	}
	if len(hub.bound) != 4 {
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

// A session id with nothing bound to it is not registered here: on resume Claude Code gives
// an MCP server another session id than its hooks, so the hook's Execution, registered under
// session A, is found through the client process the MCP shares with it, though the MCP was
// started with session B.
func TestAnUnknownSessionFindsTheHooksExecutionThroughTheClient(t *testing.T) {
	env := map[string]string{"CLAUDE_CODE_SESSION_ID": "session-b"}
	hub := &scriptedHub{binds: []string{"unbound", ""}}
	b, err := resolverFor(t, hub, "claude", env, proc.Ancestor{PID: 42, StartMS: 100}).Resolve(context.Background(), "")
	if err != nil || b.ExecutionID != "ex_bound" || b.Rung != 3 || hub.session != nil {
		t.Fatalf("%+v %v registered %+v", b, err, hub.session)
	}
	if len(hub.bound) != 2 || hub.bound[0]["session_id"] != "session-b" || hub.bound[1]["session_id"] != nil || hub.bound[1]["client_pid"] != float64(42) {
		t.Fatalf("binds %v", hub.bound)
	}
	// With nothing to bind to once the grace period is over, as when hooks never ran, it
	// registers the session it can name, as an Execution of its own.
	none := &scriptedHub{binds: []string{"unbound"}}
	r := resolverFor(t, none, "claude", env, proc.Ancestor{})
	if b, err := r.Resolve(context.Background(), ""); err != nil || b.ExecutionID != "ex_new" || !r.own || none.session.SessionID != "session-b" || none.session.Source != "mcp" {
		t.Fatalf("%+v %v own %v registered %+v", b, err, r.own, none.session)
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
}

// After a loss, the next call binds afresh through the usual rungs, to the session's current
// Execution, and is no longer lost.
func TestAFreshBindReplacesALostOne(t *testing.T) {
	hub := &scriptedHub{}
	r := resolverFor(t, hub, "claude", map[string]string{"CLAUDE_CODE_SESSION_ID": "s"}, proc.Ancestor{})
	r.lost, r.gone = true, map[string]bool{"ex_old": true}
	b, err := r.Resolve(context.Background(), "")
	if err != nil || b.ExecutionID != "ex_bound" || r.Lost() {
		t.Fatalf("%+v %v lost %v", b, err, r.Lost())
	}
}

// A lost Execution is never bound again, by whatever route it comes back: the environment, a
// handle argument, or a hub answer naming it. Until another binds, calls answer lost.
func TestALostExecutionIsNeverBoundAgain(t *testing.T) {
	hub := &scriptedHub{binds: []string{"unbound"}}
	r := resolverFor(t, hub, "codex", map[string]string{"IRIS_EXECUTION": "ex_old"}, proc.Ancestor{})
	r.lost, r.gone = true, map[string]bool{"ex_old": true}
	if _, err := r.Resolve(context.Background(), "ex_old"); !IsCode(err, "lost") {
		t.Fatalf("a lost handle gave %v", err)
	}
	for _, req := range hub.bound {
		if req["execution"] == "ex_old" {
			t.Fatalf("asked the hub for the lost Execution: %v", req)
		}
	}
	// The hub answers with an Execution already lost here: still lost, and nothing is bound.
	again := resolverFor(t, &scriptedHub{}, "claude", map[string]string{"CLAUDE_CODE_SESSION_ID": "s"}, proc.Ancestor{})
	again.lost, again.gone = true, map[string]bool{"ex_bound": true}
	if _, err := again.Resolve(context.Background(), ""); !IsCode(err, "lost") || again.bound.ExecutionID != "" {
		t.Fatalf("bound a lost Execution: %v %+v", err, again.bound)
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
	t.Run("a single-session client with no hook registers its process", func(t *testing.T) {
		hub := &scriptedHub{binds: []string{"unbound"}}
		r := resolverFor(t, hub, "antigravity", nil, single)
		b, err := r.Resolve(ctx, "")
		if err != nil || b.Rung != 3 || !r.own || hub.session.SessionID != "mcp-42-100" || !hub.session.SingleSession || hub.session.ClientPID != 42 {
			t.Fatalf("%+v %v %+v", b, err, hub.session)
		}
	})
	t.Run("a multiplexing process with no hook stays unbound", func(t *testing.T) {
		hub := &scriptedHub{binds: []string{"unbound"}}
		if _, err := resolverFor(t, hub, "codex", nil, single).Resolve(ctx, ""); !IsCode(err, "unbound") || hub.session != nil {
			t.Fatalf("%v registered %+v", err, hub.session)
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

// Once lost, the binding is empty, so there is nothing to renew until a fresh bind.
func TestMaintainRenewsNothingWhileLost(t *testing.T) {
	r := &Resolver{lost: true}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	r.Maintain(ctx, "shim", time.Millisecond, func() bool { return true }, func() { t.Error("stopped") })
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

// Lost reports the heartbeat's verdict, and only that: a fresh resolver is not lost.
func TestLostReportsOnlyTheHeartbeatsVerdict(t *testing.T) {
	if (&Resolver{}).Lost() {
		t.Fatal("a fresh resolver reads as lost")
	}
	if !(&Resolver{lost: true}).Lost() {
		t.Fatal("a resolver the heartbeat found lost reads as live")
	}
}

// After a loss the resolver never registers a session itself: right after a hub restart the
// next tool call usually comes before the session's hook has registered again, and a
// registration then would give this process an Execution of its own, beside the hook's. It
// answers lost until a bind finds the session's Execution.
func TestAfterALossTheResolverWaitsForTheSessionsOwnExecution(t *testing.T) {
	hub := &scriptedHub{binds: []string{"unbound"}}
	r := resolverFor(t, hub, "claude", map[string]string{"CLAUDE_CODE_SESSION_ID": "s"}, proc.Ancestor{})
	r.lost, r.gone = true, map[string]bool{"ex_old": true}
	if _, err := r.Resolve(context.Background(), ""); !IsCode(err, "lost") || hub.session != nil {
		t.Fatalf("registered after a loss: %v %+v", err, hub.session)
	}
	// Once the hook has registered, the bind finds that Execution.
	b, err := r.Resolve(context.Background(), "")
	if err != nil || b.ExecutionID != "ex_bound" || r.Lost() || hub.session != nil {
		t.Fatalf("%+v %v lost %v registered %+v", b, err, r.Lost(), hub.session)
	}
}
