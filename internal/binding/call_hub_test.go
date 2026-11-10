package binding

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mateusz-klatt/iris/internal/hub"
	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/proc"
	"github.com/mateusz-klatt/iris/internal/store"
)

// Binding by a call's ids against the real hub and store (ADR 017): the hook's Execution is what
// a call naming its session binds, a sub-agent's lookups leave its lease mode as they find it, and
// a session that is recovering after a restart answers as one never registered.
func TestCallBindingAgainstTheHub(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.db")
	s, err := store.Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	serve := func(s *store.Store) *httptest.Server {
		srv := httptest.NewUnstartedServer(nil)
		srv.Config.Handler = hub.New(s, "secret", []string{srv.Listener.Addr().String()})
		srv.Start()
		return srv
	}
	srv := serve(s)
	ctx := context.Background()
	// A hook registers, or renews, a Codex session, and says the lease the hub gave it.
	hook := func(session string) (string, int64) {
		t.Helper()
		out, err := s.Call(ctx, "sessions", store.Request{Client: "codex", SessionID: session, Host: "host", Cwd: "/repo", Source: "hook"})
		if err != nil {
			t.Fatal(err)
		}
		return out["execution_id"].(string), out["lease_ms"].(int64)
	}
	shim := func(srv *httptest.Server) *Resolver {
		return &Resolver{Hub: &hubclient.Client{Config: config{srv.URL}}, Client: "codex", Host: "host", CWD: "/repo",
			Getenv: func(string) string { return "" }, Ancestor: func() (proc.Ancestor, error) { return proc.Ancestor{}, nil }}
	}

	ex, lease := hook("S")
	if lease != 900000 {
		t.Fatalf("a hook's first lease is %d", lease)
	}
	root := shim(srv)
	b, primary, err := root.ResolveCall(ctx, "", "S", "S")
	if err != nil || b.ExecutionID != ex || b.Rung != 2 || !primary {
		t.Fatalf("%+v primary %v %v", b, primary, err)
	}
	// The root's heartbeat keeps the hook's Execution, and puts it on the 60-second lease.
	var beat map[string]any
	if err = root.Hub.Do(ctx, "POST", "/v1/executions/"+ex+"/heartbeat", map[string]any{"generation": b.Generation, "source": "shim"}, &beat); err != nil || beat["lease_ms"] != float64(60000) {
		t.Fatalf("%v %v", beat, err)
	}
	if _, lease = hook("S"); lease != 60000 {
		t.Fatalf("after a shim's heartbeat a hook renews for %d", lease)
	}

	// A sub-agent's call binds the root's Execution and leaves its lease mode as it was: in
	// heartbeat mode it stays on 60 seconds, and one only hooks renewed stays on 15 minutes.
	sub := shim(srv)
	if b, primary, err = sub.ResolveCall(ctx, "", "S", "T"); err != nil || b.ExecutionID != ex || primary {
		t.Fatalf("%+v primary %v %v", b, primary, err)
	}
	if _, lease = hook("S"); lease != 60000 {
		t.Fatalf("a sub-agent's lookup changed the lease mode: %d", lease)
	}
	quiet, _ := hook("H")
	if b, _, err = sub.ResolveCall(ctx, "", "H", "T2"); err != nil || b.ExecutionID != quiet {
		t.Fatalf("%+v %v", b, err)
	}
	if _, lease = hook("H"); lease != 900000 {
		t.Fatalf("a sub-agent's lookup put a hook-only Execution on %d", lease)
	}

	// With no id and no other evidence a shim is unbound, as today, with today's text; a call
	// naming a session no hook registered gets decision 7's.
	if _, _, err = shim(srv).ResolveCall(ctx, "", "", ""); !IsCode(err, "unbound") || !strings.Contains(err.Error(), "Pass the execution handle") {
		t.Fatal(err)
	}
	if _, _, err = shim(srv).ResolveCall(ctx, "", "unregistered", "unregistered"); !isText(err, errUnusable) {
		t.Fatal(err)
	}

	// After a hub restart S is recovering until something reattaches it, and a fresh shim's call
	// naming it gets the same text as one naming a session never registered.
	srv.Close()
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	if s, err = store.Open(path, nil); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	restarted := serve(s)
	defer restarted.Close()
	if _, _, err = shim(restarted).ResolveCall(ctx, "", "S", "S"); !isText(err, errUnusable) {
		t.Fatal(err)
	}
}
