package binding

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/hub"
	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/proc"
	"github.com/mateusz-klatt/iris/internal/store"
)

type compatClock struct {
	mu   sync.Mutex
	wall time.Time
	tick int64
}

func newCompatClock(wall time.Time) *compatClock {
	return &compatClock{wall: wall, tick: 1000}
}

func (c *compatClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.wall
}

func (c *compatClock) Tick() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.tick
}

func (c *compatClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.wall = c.wall.Add(d)
	c.tick += int64(d / time.Millisecond)
}

type compatHubConfig struct {
	url, token string
}

func (c compatHubConfig) HubURL() string         { return c.url }
func (c compatHubConfig) Token() (string, error) { return c.token, nil }

func startCompatHub(s *store.Store) *httptest.Server {
	srv := httptest.NewUnstartedServer(nil)
	srv.Config.Handler = hub.New(s, "secret", []string{srv.Listener.Addr().String()})
	srv.Start()
	return srv
}

func newCompatResolver(url, client, host, cwd string, env map[string]string, pid int, start int64) *Resolver {
	return &Resolver{
		Hub:    &hubclient.Client{Config: compatHubConfig{url: url, token: "secret"}},
		Client: client,
		Host:   host,
		CWD:    cwd,
		Getenv: func(k string) string { return env[k] },
		Ancestor: func() (proc.Ancestor, error) {
			return proc.Ancestor{PID: pid, StartMS: start, Name: client}, nil
		},
		Grace: 10 * time.Millisecond,
	}
}

func TestV0115BindingResolverInertAgainstStage1bHub(t *testing.T) {
	t.Run("ordinary session recovery wake and Choice A on stage-1b hub", func(t *testing.T) {
		clock := newCompatClock(time.Unix(1800000000, 0))
		dbPath := filepath.Join(t.TempDir(), "hub.db")
		s, err := store.Open(dbPath, clock)
		if err != nil {
			t.Fatal(err)
		}
		var activeStore = s
		var srv *httptest.Server
		t.Cleanup(func() {
			if srv != nil {
				srv.Close()
			}
			if activeStore != nil {
				_ = activeStore.Close()
			}
		})
		srv = startCompatHub(s)

		hookClient := &hubclient.Client{Config: compatHubConfig{url: srv.URL, token: "secret"}}
		first, err := hookClient.RegisterSession(t.Context(), hubclient.SessionRequest{
			Client: "codex", Host: "host", SessionID: "S-ord",
			CWD: "/repo/w", Worktree: "/repo/w", Branch: "main", Source: "hook",
			ClientPID: 401, ClientPIDStartMS: 4401,
		})
		if err != nil {
			t.Fatal(err)
		}

		res := newCompatResolver(srv.URL, "codex", "host", "/repo/w", nil, 401, 4401)
		b, primary, err := res.ResolveCall(t.Context(), "", "S-ord", "S-ord")
		if err != nil || !primary {
			t.Fatalf("unexpected codex ResolveCall: b=%+v primary=%t err=%v", b, primary, err)
		}
		if b.ExecutionID != first.ExecutionID || b.Generation != 1 || !b.HookSeen {
			t.Fatalf("unexpected bound state: %+v", b)
		}
		if _, ok := res.Primary(); !ok {
			t.Fatal("expected wake target for hook-seen codex session")
		}
		if err := res.Heartbeat(t.Context(), "shim"); err != nil {
			t.Fatal(err)
		}

		// Restart hub: D9 refresh reattachment via heartbeat resumes ordinary recovering session.
		if err := activeStore.Close(); err != nil {
			t.Fatal(err)
		}
		activeStore = nil
		reopened, err := store.Open(dbPath, clock)
		if err != nil {
			t.Fatal(err)
		}
		activeStore = reopened
		srv.Close()
		srv = startCompatHub(reopened)
		hookClient = &hubclient.Client{Config: compatHubConfig{url: srv.URL, token: "secret"}}
		res.Hub = hookClient

		reattached, _, err := res.ResolveCall(t.Context(), "", "S-ord", "S-ord")
		if err != nil {
			t.Fatal(err)
		}
		if reattached.ExecutionID != first.ExecutionID || !reattached.HookSeen {
			t.Fatalf("expected same execution after D9 recovery, got %+v", reattached)
		}

		// After lease expiry, Choice A on stage-1b hubs refuses v0.1.15 proofless MCP fallback over
		// lost hook history with 409 lost (highest_generation=1), while genuine no-hook fallback still registers G=1.
		clock.Advance(16 * time.Minute)
		if _, err := activeStore.Call(t.Context(), "expire", store.Request{}); err != nil {
			t.Fatal(err)
		}
		res2 := newCompatResolver(srv.URL, "claude", "host", "/repo/w", map[string]string{"CLAUDE_CODE_SESSION_ID": "S-ord-fb"}, 402, 4402)
		if _, err := hookClient.RegisterSession(t.Context(), hubclient.SessionRequest{
			Client: "claude", Host: "host", SessionID: "S-ord-fb",
			CWD: "/repo/w", Worktree: "/repo/w", Branch: "main", Source: "hook",
		}); err != nil {
			t.Fatal(err)
		}
		clock.Advance(16 * time.Minute)
		if _, err := activeStore.Call(t.Context(), "expire", store.Request{}); err != nil {
			t.Fatal(err)
		}
		if fallback, err := res2.Resolve(t.Context(), ""); !IsCode(err, "lost") {
			t.Fatalf("expected Choice A lost for proofless v0.1.15 fallback over lost hook history, got b=%+v err=%v", fallback, err)
		}
		resNoHook := newCompatResolver(srv.URL, "claude", "host", "/repo/w", map[string]string{"CLAUDE_CODE_SESSION_ID": "S-nohook-fb"}, 403, 4403)
		noHook, err := resNoHook.Resolve(t.Context(), "")
		if err != nil || noHook.Generation != 1 {
			t.Fatalf("expected genuine no-hook G=1 fallback, got b=%+v err=%v", noHook, err)
		}
	})

	t.Run("hookless succession child stays protected until hook arrival", func(t *testing.T) {
		clock := newCompatClock(time.Unix(1800000000, 0))
		dbPath := filepath.Join(t.TempDir(), "guard.db")
		s, err := store.Open(dbPath, clock)
		if err != nil {
			t.Fatal(err)
		}
		var activeStore = s
		var srv *httptest.Server
		t.Cleanup(func() {
			if srv != nil {
				srv.Close()
			}
			if activeStore != nil {
				_ = activeStore.Close()
			}
		})
		srv = startCompatHub(s)
		hc := &hubclient.Client{Config: compatHubConfig{url: srv.URL, token: "secret"}}

		parent, err := hc.RegisterSession(t.Context(), hubclient.SessionRequest{
			Client: "codex", Host: "host", SessionID: "S-child",
			CWD: "/repo/w", Worktree: "/repo/w", Branch: "main", Source: "hook",
			ClientPID: 501, ClientPIDStartMS: 5501,
		})
		if err != nil {
			t.Fatal(err)
		}
		clock.Advance(16 * time.Minute)
		if _, err := s.Call(t.Context(), "expire", store.Request{}); err != nil {
			t.Fatal(err)
		}
		child, err := hc.Succeed(t.Context(), hubclient.SucceedRequest{
			Client: "codex", Host: "host", SessionID: "S-child", Source: "shim",
			PriorGeneration: parent.Generation, PriorExecutionID: parent.ExecutionID,
			ClientPID: 501, ClientPIDStartMS: 5501,
		})
		if err != nil {
			t.Fatal(err)
		}

		// Older v0.1.15 resolvers without succession_proof cannot bind by codex thread, session,
		// or explicit handle while child is live and hookless, and wake stays disarmed.
		resCodex := newCompatResolver(srv.URL, "codex", "host", "/repo/w", nil, 501, 5501)
		if _, _, err := resCodex.ResolveCall(context.Background(), "", "S-child", "S-child"); err == nil {
			t.Fatal("v0.1.15 codex resolver unexpectedly bound hookless succession child")
		}
		if _, ok := resCodex.Primary(); ok {
			t.Fatal("Primary unexpectedly armed for hookless child")
		}
		resHandle := newCompatResolver(srv.URL, "codex", "host", "/repo/w", nil, 501, 5501)
		if _, err := resHandle.Resolve(t.Context(), child.ExecutionID); !IsCode(err, "lost") {
			t.Fatalf("expected lost on v0.1.15 handle resolve for hookless child, got %v", err)
		}

		// Genuine hook arrival sets hook_seen and immediately unlocks the v0.1.15 resolver and wake.
		if _, err := hc.RegisterSession(t.Context(), hubclient.SessionRequest{
			Client: "codex", Host: "host", SessionID: "S-child",
			CWD: "/repo/w", Worktree: "/repo/w", Branch: "main", Source: "hook",
			ClientPID: 501, ClientPIDStartMS: 5501,
		}); err != nil {
			t.Fatal(err)
		}
		bound, primary, err := resCodex.ResolveCall(t.Context(), "", "S-child", "S-child")
		if err != nil || !primary {
			t.Fatalf("expected codex resolve + primary after hook arrival: bound=%+v primary=%t err=%v", bound, primary, err)
		}
		if bound.ExecutionID != child.ExecutionID || !bound.HookSeen {
			t.Fatalf("expected v0.1.15 resolver to bind after hook arrival, got %+v", bound)
		}
		if err := resCodex.Heartbeat(t.Context(), "watch"); err != nil {
			t.Fatalf("watch heartbeat failed after hook unlock: %v", err)
		}
		if _, ok := resCodex.Primary(); !ok {
			t.Fatal("expected Primary to arm after hook arrival")
		}
	})

	t.Run("migrated pre-029 DB keeps v0.1.15 resolver working across restart", func(t *testing.T) {
		clock := newCompatClock(time.Unix(1800000000, 0))
		dbPath := filepath.Join(t.TempDir(), "legacy-res.db")
		seed, err := store.Open(dbPath, clock)
		if err != nil {
			t.Fatal(err)
		}
		seedSrv := startCompatHub(seed)
		hc := &hubclient.Client{Config: compatHubConfig{url: seedSrv.URL, token: "secret"}}
		reg, err := hc.RegisterSession(t.Context(), hubclient.SessionRequest{
			Client: "claude", Host: "host", SessionID: "S-leg-res",
			CWD: "/repo/w", Worktree: "/repo/w", Branch: "main", Source: "hook",
			ClientPID: 601, ClientPIDStartMS: 6601,
		})
		if err != nil {
			seedSrv.Close()
			_ = seed.Close()
			t.Fatal(err)
		}
		// Bind in cached resolver before restart (as a running MCP server holds in memory).
		res := newCompatResolver(seedSrv.URL, "claude", "host", "/repo/w", map[string]string{"CLAUDE_CODE_SESSION_ID": "S-leg-res"}, 601, 6601)
		if _, err := res.Resolve(t.Context(), ""); err != nil {
			seedSrv.Close()
			_ = seed.Close()
			t.Fatal(err)
		}
		seedSrv.Close()
		if err := seed.Close(); err != nil {
			t.Fatal(err)
		}

		raw, err := sql.Open("sqlite", dbPath)
		if err != nil {
			t.Fatal(err)
		}
		for _, stmt := range []string{
			`DROP INDEX IF EXISTS execution_prior`,
			`ALTER TABLE Execution DROP COLUMN prior_execution_id`,
			`ALTER TABLE Execution DROP COLUMN creation_source`,
			`ALTER TABLE Execution DROP COLUMN creation_proof`,
			`PRAGMA user_version = 28`,
		} {
			if _, err := raw.Exec(stmt); err != nil {
				_ = raw.Close()
				t.Fatalf("%s: %v", stmt, err)
			}
		}
		if err := raw.Close(); err != nil {
			t.Fatal(err)
		}

		upgraded, err := store.Open(dbPath, clock)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = upgraded.Close() }()
		srv := startCompatHub(upgraded)
		defer srv.Close()
		res.Hub = &hubclient.Client{Config: compatHubConfig{url: srv.URL, token: "secret"}}

		// Resolver refreshes cached binding through recovery heartbeat against upgraded v29 hub.
		b, err := res.Resolve(t.Context(), "")
		if err != nil {
			t.Fatal(err)
		}
		if b.ExecutionID != reg.ExecutionID || !b.HookSeen {
			t.Fatalf("migrated resolver bind mismatch: %+v", b)
		}
	})

	t.Run("un-opted heartbeat keeps v0.1.15 D9 recovery lost on G2 after restart", func(t *testing.T) {
		clock := newCompatClock(time.Unix(1800000000, 0))
		dbPath := filepath.Join(t.TempDir(), "hb-d9-pin.db")
		s, err := store.Open(dbPath, clock)
		if err != nil {
			t.Fatal(err)
		}
		var activeStore = s
		var srv *httptest.Server
		var reattachCalls atomic.Int64
		wrapHub := func(st *store.Store) *httptest.Server {
			server := httptest.NewUnstartedServer(nil)
			h := hub.New(st, "secret", []string{server.Listener.Addr().String()})
			server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/sessions" {
					reattachCalls.Add(1)
				}
				h.ServeHTTP(w, r)
			})
			server.Start()
			return server
		}
		t.Cleanup(func() {
			if srv != nil {
				srv.Close()
			}
			if activeStore != nil {
				_ = activeStore.Close()
			}
		})
		srv = wrapHub(s)
		hc := &hubclient.Client{Config: compatHubConfig{url: srv.URL, token: "secret"}}

		// Step 1: MCP G1 exists without hook_seen_ms; cursor Rung-3 resolver (no env session) binds it with HookSeen=false.
		g1, err := hc.RegisterSession(t.Context(), hubclient.SessionRequest{
			Client: "cursor", Host: "host", SessionID: "S-hb-d9",
			CWD: "/repo/w", Worktree: "/repo/w", Branch: "main", Source: "mcp",
			ClientPID: 810, ClientPIDStartMS: 8810, SingleSession: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		var memGen atomic.Int64
		res := newCompatResolver(srv.URL, "cursor", "host", "/repo/w", nil, 810, 8810)
		res.Recall = func(client, session string) (int, int, error) {
			if client == "cursor" && session == "S-hb-d9" {
				return int(memGen.Load()), 0, nil
			}
			return 0, 0, nil
		}
		b1, err := res.Resolve(t.Context(), "")
		if err != nil || b1.ExecutionID != g1.ExecutionID || b1.HookSeen || b1.Rung != 3 {
			t.Fatalf("unexpected initial cursor bind: b=%+v err=%v", b1, err)
		}

		// Step 2: A hook arrives on G1 (setting hook_seen_ms > 0 on the hub) while res still holds HookSeen=false,
		// then the unchanged v0.1.15 resolver sends a plain heartbeat without succession_proof.
		if _, err := hc.RegisterSession(t.Context(), hubclient.SessionRequest{
			Client: "cursor", Host: "host", SessionID: "S-hb-d9",
			CWD: "/repo/w", Worktree: "/repo/w", Branch: "main", Source: "hook",
			ClientPID: 810, ClientPIDStartMS: 8810, SingleSession: true,
		}); err != nil {
			t.Fatal(err)
		}
		if err := res.Heartbeat(t.Context(), "shim"); err != nil {
			t.Fatal(err)
		}

		// Step 3: G1 expires and Heartbeat confirms loss, keeping res.last={SessionID:"S-hb-d9", Rung:3, HookSeen:false}.
		clock.Advance(16 * time.Minute)
		if _, err := activeStore.Call(t.Context(), "expire", store.Request{}); err != nil {
			t.Fatal(err)
		}
		if err := res.Heartbeat(t.Context(), "shim"); !IsCode(err, "lost") || !res.Lost() {
			t.Fatalf("expected G1 loss confirmation, got err=%v lost=%t", err, res.Lost())
		}

		// Step 4: Hook starts G2 (ex2), local recall updates to gen=2, and hub restarts so G2 enters recovery.
		if _, err := hc.RegisterSession(t.Context(), hubclient.SessionRequest{
			Client: "cursor", Host: "host", SessionID: "S-hb-d9",
			CWD: "/repo/w", Worktree: "/repo/w", Branch: "main", Source: "hook",
			ClientPID: 810, ClientPIDStartMS: 8810, SingleSession: true,
		}); err != nil {
			t.Fatal(err)
		}
		memGen.Store(2)

		if err := activeStore.Close(); err != nil {
			t.Fatal(err)
		}
		activeStore = nil
		reopened, err := store.Open(dbPath, clock)
		if err != nil {
			t.Fatal(err)
		}
		activeStore = reopened
		srv.Close()
		srv = wrapHub(reopened)
		res.Hub = &hubclient.Client{Config: compatHubConfig{url: srv.URL, token: "secret"}}

		// Reset counter right before Resolve: with un-opted heartbeat omitting hook_seen, res keeps
		// last.HookSeen=false (matching v0.1.15) so Resolve must return lost and send zero /v1/sessions reattach.
		reattachCalls.Store(0)
		if b2, err := res.Resolve(t.Context(), ""); !IsCode(err, "lost") {
			t.Fatalf("v0.1.15 resolver unexpectedly resumed G2 instead of returning lost: b=%+v err=%v", b2, err)
		}
		if got := reattachCalls.Load(); got != 0 {
			t.Fatalf("v0.1.15 resolver sent %d /v1/sessions reattach requests after restart, want 0", got)
		}
	})
}
