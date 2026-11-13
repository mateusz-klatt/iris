package binding

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/hub"
	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/proc"
	"github.com/mateusz-klatt/iris/internal/store"
)

func TestAntigravityWakeBindsOnlyMatchingSingleSessionExecutionWithoutRegistering(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "hub.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	srv := httptest.NewUnstartedServer(nil)
	srv.Config.Handler = hub.New(s, "secret", []string{srv.Listener.Addr().String()})
	srv.Start()
	defer srv.Close()

	const (
		convA = "11111111-2222-3333-4444-555555555555"
		convB = "66666666-7777-8888-9999-aaaaaaaaaaaa"
	)

	r := NewAntigravityWake(&hubclient.Client{Config: config{srv.URL}}, convA, proc.Ancestor{PID: 42, StartMS: 100, Name: "agy"})
	r.Host = "host"
	r.CWD = "/repo"

	// When no hook has registered the conversation yet, LookupWake stays unbound and never registers a fallback.
	if target, ok := r.LookupWake(context.Background()); ok {
		t.Fatalf("unbound LookupWake returned target: %+v", target)
	}

	// An Execution for convA on another PID must not satisfy convA's helper for PID 42.
	if _, err := s.Call(context.Background(), "sessions", store.Request{
		Client: "antigravity", SessionID: convB, Host: "host", Cwd: "/repo2", Source: "hook",
		ClientPID: 99, ClientPIDStartMS: 200, SingleSession: true,
	}); err != nil {
		t.Fatal(err)
	}
	if target, ok := r.LookupWake(context.Background()); ok {
		t.Fatalf("other-PID session satisfied LookupWake: %+v", target)
	}

	// An MCP fallback (or retired conversation) alone on PID 42 has the wrong session_id and must be rejected.
	if _, err := s.Call(context.Background(), "sessions", store.Request{
		Client: "antigravity", SessionID: "mcp-42-100", Host: "host", Cwd: "/repo", Source: "mcp",
		ClientPID: 42, ClientPIDStartMS: 100, SingleSession: true,
	}); err != nil {
		t.Fatal(err)
	}
	if target, ok := r.LookupWake(context.Background()); ok {
		t.Fatalf("MCP fallback satisfied LookupWake: %+v", target)
	}

	// Registering convA on PID 42 alongside mcp-42-100 makes Rung 3 ambiguous, which LookupWake disambiguates by session_id.
	registered, err := s.Call(context.Background(), "sessions", store.Request{
		Client: "antigravity", SessionID: convA, Host: "host", Cwd: "/repo", Source: "hook",
		ClientPID: 42, ClientPIDStartMS: 100, SingleSession: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	target, ok := r.LookupWake(context.Background())
	if !ok || target.Client != "antigravity" || target.SessionID != convA || target.Bound.ExecutionID != registered["execution_id"] || target.Bound.Rung != 3 {
		t.Fatalf("disambiguated Antigravity wake target: %+v, %t", target, ok)
	}

	// Ending mcp-42-100 leaves a single Rung 3 match on PID 42, which a fresh helper binds directly.
	rSingle := NewAntigravityWake(&hubclient.Client{Config: config{srv.URL}}, convA, proc.Ancestor{PID: 42, StartMS: 100, Name: "agy"})
	rSingle.Host = "host"
	rSingle.CWD = "/repo"
	// End the fallback so Rung 3 returns one match directly.
	if _, err := s.Call(context.Background(), "sessions", store.Request{
		Client: "antigravity", SessionID: convA, Host: "host", Cwd: "/repo", Source: "hook",
		ClientPID: 55, ClientPIDStartMS: 300, SingleSession: true,
	}); err != nil {
		t.Fatal(err)
	}
	rDirect := NewAntigravityWake(&hubclient.Client{Config: config{srv.URL}}, convB, proc.Ancestor{PID: 99, StartMS: 200, Name: "antigravity"})
	rDirect.Host = "host"
	rDirect.CWD = "/repo2"
	directTarget, ok := rDirect.LookupWake(context.Background())
	if !ok || directTarget.SessionID != convB || directTarget.Bound.Rung != 3 {
		t.Fatalf("direct Rung 3 Antigravity wake target: %+v, %t", directTarget, ok)
	}
	started := 0
	if ok, err := rDirect.StartIfPrimary(directTarget, func() error { started++; return nil }); !ok || err != nil || started != 1 {
		t.Fatalf("StartIfPrimary: %t, %v, starts %d", ok, err, started)
	}
}

func TestAntigravityWakeRejectsInvalidAncestorsAndNonHelperBindings(t *testing.T) {
	plain := New(nil, "antigravity")
	if _, ok := plain.LookupWake(context.Background()); ok {
		t.Fatal("ordinary MCP resolver returned a wake target")
	}

	invalidAnc := NewAntigravityWake(nil, "11111111-2222-3333-4444-555555555555", proc.Ancestor{PID: 42, StartMS: 100, Name: "bash"})
	invalidAnc.Host = "host"
	if _, ok := invalidAnc.LookupWake(context.Background()); ok {
		t.Fatal("non-antigravity ancestor satisfied LookupWake")
	}

	for _, tc := range []struct {
		name  string
		setup func(*Resolver)
	}{
		{"undecided", func(r *Resolver) { r.decided.Store(nil) }},
		{"ordinary mcp without wakeSession", func(r *Resolver) { r.wakeSession = "" }},
		{"fallback own", func(r *Resolver) { r.own = true }},
		{"wrong session id", func(r *Resolver) { r.bound.SessionID = "other" }},
		{"handle rung", func(r *Resolver) { r.bound.Rung = 4 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &Resolver{
				Client:      "antigravity",
				Host:        "host",
				Getenv:      func(string) string { return "" },
				wakeSession: "11111111-2222-3333-4444-555555555555",
				bound:       Bound{ExecutionID: "ex_1", Generation: 1, SessionID: "11111111-2222-3333-4444-555555555555", Rung: 3},
			}
			r.decided.Store(decide("", ""))
			tc.setup(r)
			noWakeTarget(t, r)
		})
	}
}

func TestAntigravityWakeMaintainBindsRenewsAndStopsWithoutRenewingOnClear(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "hub.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	srv := httptest.NewUnstartedServer(nil)
	srv.Config.Handler = hub.New(s, "secret", []string{srv.Listener.Addr().String()})
	srv.Start()
	defer srv.Close()

	const conv = "11111111-2222-3333-4444-555555555555"
	if _, err := s.Call(context.Background(), "sessions", store.Request{
		Client: "antigravity", SessionID: conv, Host: "host", Cwd: "/repo", Source: "hook",
		ClientPID: 42, ClientPIDStartMS: 100, SingleSession: true,
	}); err != nil {
		t.Fatal(err)
	}

	r := NewAntigravityWake(&hubclient.Client{Config: config{srv.URL}}, conv, proc.Ancestor{PID: 42, StartMS: 100, Name: "agy"})
	r.Host = "host"
	r.CWD = "/repo"

	var alive atomic.Bool
	alive.Store(true)
	var ticks atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		r.Maintain(ctx, "shim", 5*time.Millisecond, func() bool {
			if ticks.Add(1) >= 2 {
				return alive.Load()
			}
			return true
		}, func() {
			cancel()
		})
	}()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if target, ok := r.Primary(); ok && target.SessionID == conv {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, ok := r.Primary(); !ok {
		t.Fatal("Maintain did not bind the Antigravity wake session")
	}

	alive.Store(false)
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("Maintain did not stop promptly when alive became false")
	}
}

func TestAntigravityWakeMaintainDoesNotHeartbeatWhenStaleDuringLookup(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "hub.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	var alive atomic.Bool
	alive.Store(true)
	var binds, heartbeats atomic.Int32

	srv := httptest.NewUnstartedServer(nil)
	base := hub.New(s, "secret", []string{srv.Listener.Addr().String()})
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/v1/bind" {
			binds.Add(1)
			// Simulate /clear switching to conversation B while A's /v1/bind response is in flight.
			alive.Store(false)
		}
		if strings.HasSuffix(req.URL.Path, "/heartbeat") {
			heartbeats.Add(1)
		}
		base.ServeHTTP(w, req)
	})
	srv.Start()
	defer srv.Close()

	const conv = "11111111-2222-3333-4444-555555555555"
	if _, err := s.Call(context.Background(), "sessions", store.Request{
		Client: "antigravity", SessionID: conv, Host: "host", Cwd: "/repo", Source: "hook",
		ClientPID: 42, ClientPIDStartMS: 100, SingleSession: true,
	}); err != nil {
		t.Fatal(err)
	}

	r := NewAntigravityWake(&hubclient.Client{Config: config{srv.URL}}, conv, proc.Ancestor{PID: 42, StartMS: 100, Name: "agy"})
	r.Host = "host"
	r.CWD = "/repo"

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		r.Maintain(ctx, "shim", 5*time.Millisecond, alive.Load, cancel)
	}()

	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("Maintain did not stop when session went stale during /v1/bind")
	}
	if binds.Load() == 0 {
		t.Fatal("expected /v1/bind lookup before stale transition")
	}
	if got := heartbeats.Load(); got != 0 {
		t.Fatalf("stale session sent %d heartbeats after /v1/bind", got)
	}
}
