package binding

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/mateusz-klatt/iris/internal/hub"
	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/proc"
	"github.com/mateusz-klatt/iris/internal/store"
)

func TestKimiWakeRequiresTheHookSessionBoundByItsSingleSessionProcess(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "hub.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	srv := httptest.NewUnstartedServer(nil)
	srv.Config.Handler = hub.New(s, "secret", []string{srv.Listener.Addr().String()})
	srv.Start()
	defer srv.Close()
	const session = "session_dd1b42d8-af9e-40b6-b28f-23a0c4be86ad"
	registered, err := s.Call(context.Background(), "sessions", store.Request{
		Client: "kimi", SessionID: session, Host: "host", Cwd: "/repo", Source: "hook",
		ClientPID: 42, ClientPIDStartMS: 100, SingleSession: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	r := &Resolver{
		Hub: &hubclient.Client{Config: config{srv.URL}}, Client: "kimi", Host: "host", CWD: "/repo",
		Getenv: func(string) string { return "" },
		Ancestor: func() (proc.Ancestor, error) {
			return proc.Ancestor{PID: 42, StartMS: 100}, nil
		},
	}
	bound, primary, err := r.ResolveCall(context.Background(), "", "", "")
	if err != nil || !primary || bound.SessionID != session || bound.Rung != 3 {
		t.Fatalf("Kimi process binding: %+v, primary %t, %v", bound, primary, err)
	}
	if bound.ExecutionID != registered["execution_id"] {
		t.Fatalf("bound another Execution: %+v", bound)
	}
	target, ok := r.Primary()
	if !ok || target.Client != "kimi" || target.SessionID != session || target.Bound != bound || target.Host != "host" {
		t.Fatalf("Kimi wake target: %+v, %t", target, ok)
	}
	started := 0
	if ok, err := r.StartIfPrimary(target, func() error { started++; return nil }); !ok || err != nil || started != 1 {
		t.Fatalf("Kimi native admission: %t, %v, starts %d", ok, err, started)
	}
}

func TestKimiWakeRejectsFallbackUndecidedAndOverriddenBindings(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*Resolver)
	}{
		{"undecided", func(r *Resolver) { r.decided.Store(nil) }},
		{"fallback", func(r *Resolver) { r.own = true }},
		{"missing native session", func(r *Resolver) { r.bound.SessionID = "" }},
		{"handle binding", func(r *Resolver) { r.bound.Rung = 4 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &Resolver{
				Client: "kimi", Host: "host", Getenv: func(string) string { return "" },
				bound: Bound{ExecutionID: "execution", Generation: 1, SessionID: "session_dd1b42d8-af9e-40b6-b28f-23a0c4be86ad", Rung: 3},
			}
			r.decided.Store(decide("", ""))
			tc.setup(r)
			noWakeTarget(t, r)
		})
	}
}
