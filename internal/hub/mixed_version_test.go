package hub

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/store"
)

type mixedHubConfig struct {
	url   string
	token string
}

func (c mixedHubConfig) HubURL() string         { return c.url }
func (c mixedHubConfig) Token() (string, error) { return c.token, nil }

type lockedClock struct {
	mu   sync.Mutex
	now  time.Time
	tick int64
}

func newLockedClock(wall time.Time) *lockedClock {
	return &lockedClock{now: wall, tick: 1000}
}

func (c *lockedClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *lockedClock) Tick() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.tick
}

func (c *lockedClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	c.tick += int64(d / time.Millisecond)
}

func startMixedHub(s *store.Store) *httptest.Server {
	srv := httptest.NewUnstartedServer(nil)
	srv.Config.Handler = New(s, "secret", []string{srv.Listener.Addr().String()})
	srv.Start()
	return srv
}

func TestMixedVersionStage1bHubClientAndStoreMatrix(t *testing.T) {
	t.Run("stage1b hub with stage1b hubclient fresh and migrated", func(t *testing.T) {
		clock := newLockedClock(time.Unix(1800000000, 0))
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

		srv = startMixedHub(s)
		c := &hubclient.Client{Config: mixedHubConfig{url: srv.URL, token: "secret"}}

		// Un-opted registration omits succession_proof and classification; opted-in registration advertises both.
		unoptedHookResp, err := c.RegisterSession(t.Context(), hubclient.SessionRequest{
			Client: "claude", Host: "host", SessionID: "S-mix",
			CWD: "/repo/w", Worktree: "/repo/w", Branch: "main", Source: "hook",
			ClientPID: 301, ClientPIDStartMS: 3301,
		})
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(unoptedHookResp.Capabilities, []string{"shell_scope", "file_claims"}) || unoptedHookResp.HookSeen.Known {
			t.Fatalf("un-opted registration leaked stage 1b fields: %+v", unoptedHookResp)
		}
		if uCls, err := unoptedHookResp.Candidate().Classify(); err != nil || uCls != (hubclient.Classification{Kind: hubclient.CandidateUnclassified}) {
			t.Fatalf("unexpected un-opted hook classification: cls=%+v err=%v", uCls, err)
		}

		hookResp, err := c.RegisterSession(t.Context(), hubclient.SessionRequest{
			Client: "claude", Host: "host", SessionID: "S-mix",
			CWD: "/repo/w", Worktree: "/repo/w", Branch: "main", Source: "hook",
			ClientPID: 301, ClientPIDStartMS: 3301, SuccessionProof: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		cls, err := hookResp.Candidate().Classify()
		if err != nil {
			t.Fatal(err)
		}
		if cls.Kind != hubclient.CandidateOrdinary || !cls.HookSeen || cls.CreationSource != "hook" || cls.CreationProof != "initial" {
			t.Fatalf("unexpected hook classification: %+v", cls)
		}

		unoptedOutResp, err := c.RegisterOutsideSession(t.Context(), hubclient.SessionRequest{
			Client: "codex", Host: "host", SessionID: "S-out-mix", CWD: "/tmp/out", Source: "hook",
		})
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(unoptedOutResp.Capabilities, []string{"shell_scope", "file_claims"}) || unoptedOutResp.HookSeen.Known {
			t.Fatalf("un-opted outside registration leaked stage 1b fields: %+v", unoptedOutResp)
		}

		outResp, err := c.RegisterOutsideSession(t.Context(), hubclient.SessionRequest{
			Client: "codex", Host: "host", SessionID: "S-out-mix", CWD: "/tmp/out", Source: "hook", SuccessionProof: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		if outCls, err := outResp.Candidate().Classify(); err != nil || outCls.Kind != hubclient.CandidateOrdinary || !outCls.HookSeen {
			t.Fatalf("unexpected outside classification: cls=%+v err=%v", outCls, err)
		}

		// Calling the removed stage 1a route via hubclient gets 404 not_found before expire()/mutation.
		clock.Advance(16 * time.Minute)
		var dummy hubclient.SessionResponse
		err = c.Do(t.Context(), http.MethodPost, "/v1/sessions/succeed", hubclient.SucceedRequest{
			Client: "claude", Host: "host", SessionID: "S-mix", Source: "shim",
			PriorGeneration: hookResp.Generation, PriorExecutionID: hookResp.ExecutionID,
			ClientPID: 301, ClientPIDStartMS: 3301,
		}, &dummy)
		if !hubclient.OlderHub(err) {
			t.Fatalf("expected OlderHub on removed stage 1a route, got %v", err)
		}
		roDB, err := sql.Open("sqlite", dbPath)
		if err != nil {
			t.Fatal(err)
		}
		var preExpireState string
		if err := roDB.QueryRow("SELECT state FROM Execution WHERE id=?", hookResp.ExecutionID).Scan(&preExpireState); err != nil {
			_ = roDB.Close()
			t.Fatal(err)
		}
		_ = roDB.Close()
		if preExpireState != "active" {
			t.Fatalf("removed stage 1a route triggered expiry: state=%q", preExpireState)
		}

		// Now expire the predecessor and call stage 1b /v1/sessions/succeed/exact.
		if _, err := s.Call(t.Context(), "expire", store.Request{}); err != nil {
			t.Fatal(err)
		}
		succResp, err := c.Succeed(t.Context(), hubclient.SucceedRequest{
			Client: "claude", Host: "host", SessionID: "S-mix", Source: "shim",
			PriorGeneration: hookResp.Generation, PriorExecutionID: hookResp.ExecutionID,
			ClientPID: 301, ClientPIDStartMS: 3301,
		})
		if err != nil {
			t.Fatal(err)
		}
		succCand := succResp.Candidate()
		if succResp.SessionID != "S-mix" || succCand.SessionID != "S-mix" {
			t.Fatalf("succeed response/candidate dropped session_id: resp=%q cand=%q", succResp.SessionID, succCand.SessionID)
		}
		childCls, err := succCand.Classify()
		if err != nil {
			t.Fatal(err)
		}
		if !childCls.IsHooklessSuccession() || childCls.PriorExecutionID != hookResp.ExecutionID || childCls.PriorGeneration != hookResp.Generation {
			t.Fatalf("unexpected child classification: %+v", childCls)
		}

		// Bind without opt-in is refused with lost; bind with opt-in succeeds and classifies.
		if _, err := c.Bind(t.Context(), hubclient.BindRequest{
			Client: "claude", Host: "host", Execution: succResp.ExecutionID,
		}); !hubclient.IsCode(err, "lost") {
			t.Fatalf("expected lost without opt-in, got %v", err)
		}
		boundChild, err := c.Bind(t.Context(), hubclient.BindRequest{
			Client: "claude", Host: "host", SessionID: "S-mix", SuccessionProof: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		if bCls, err := boundChild.Classify(); err != nil || !bCls.IsHooklessSuccession() {
			t.Fatalf("unexpected bind classification: cls=%+v err=%v", bCls, err)
		}

		// Heartbeat with watch/hook/unopted-shim fails; shim+opt-in renews live and resumes recovering.
		for _, hbReq := range []hubclient.HeartbeatRequest{
			{Generation: succResp.Generation, Source: "watch", SuccessionProof: true},
			{Generation: succResp.Generation, Source: "hook", SuccessionProof: true},
			{Generation: succResp.Generation, Source: "shim"},
		} {
			if _, err := c.Heartbeat(t.Context(), succResp.ExecutionID, hbReq); !hubclient.IsCode(err, "lost") {
				t.Fatalf("expected lost for %+v, got %v", hbReq, err)
			}
		}
		liveHB, err := c.Heartbeat(t.Context(), succResp.ExecutionID, hubclient.HeartbeatRequest{
			Generation: succResp.Generation, Source: "shim", SuccessionProof: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		if lCls, err := liveHB.Classify(); err != nil || !lCls.IsHooklessSuccession() || liveHB.LeaseMS != 60000 {
			t.Fatalf("unexpected live heartbeat: cand=%+v cls=%+v err=%v", liveHB, lCls, err)
		}

		// Restart hub to put executions into recovery, then resume hookless child via Client.Heartbeat.
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
		srv = startMixedHub(reopened)
		c = &hubclient.Client{Config: mixedHubConfig{url: srv.URL, token: "secret"}}

		recHB, err := c.Heartbeat(t.Context(), succResp.ExecutionID, hubclient.HeartbeatRequest{
			Generation: succResp.Generation, Source: "shim", SuccessionProof: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		if rCls, err := recHB.Classify(); err != nil || !rCls.IsHooklessSuccession() {
			t.Fatalf("unexpected recovered classification: cls=%+v err=%v", rCls, err)
		}

		// Genuine hook attach unlocks older bind & watch paths.
		reattachHook, err := c.RegisterSession(t.Context(), hubclient.SessionRequest{
			Client: "claude", Host: "host", SessionID: "S-mix",
			CWD: "/repo/w", Worktree: "/repo/w", Branch: "main", Source: "hook",
			ClientPID: 301, ClientPIDStartMS: 3301, SuccessionProof: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		unlockCls, err := reattachHook.Candidate().Classify()
		if err != nil {
			t.Fatal(err)
		}
		if unlockCls.IsHooklessSuccession() || !unlockCls.HookSeen || unlockCls.Kind != hubclient.CandidateShimSuccession {
			t.Fatalf("hook attach did not preserve shim_succession provenance with hook_seen=true: %+v", unlockCls)
		}
		unoptedUnlocked, err := c.Bind(t.Context(), hubclient.BindRequest{Client: "claude", Host: "host", Execution: succResp.ExecutionID})
		if err != nil {
			t.Fatalf("ordinary bind failed after hook attach: %v", err)
		}
		if uCls, err := unoptedUnlocked.Classify(); err != nil || uCls != (hubclient.Classification{Kind: hubclient.CandidateUnclassified, HookSeen: true}) {
			t.Fatalf("unexpected un-opted bind classification after hook attach: cls=%+v err=%v", uCls, err)
		}
		optedUnlocked, err := c.Bind(t.Context(), hubclient.BindRequest{Client: "claude", Host: "host", Execution: succResp.ExecutionID, SuccessionProof: true})
		if err != nil {
			t.Fatalf("opted bind failed after hook attach: %v", err)
		}
		if oCls, err := optedUnlocked.Classify(); err != nil || oCls.Kind != hubclient.CandidateShimSuccession || !oCls.HookSeen {
			t.Fatalf("unexpected opted bind classification after hook attach: cls=%+v err=%v", oCls, err)
		}
		if _, err := c.Heartbeat(t.Context(), succResp.ExecutionID, hubclient.HeartbeatRequest{Generation: succResp.Generation, Source: "watch"}); err != nil {
			t.Fatalf("watch heartbeat failed after hook attach: %v", err)
		}
	})

	t.Run("migrated pre-029 DB decodes as CandidateLegacy via HTTP hubclient", func(t *testing.T) {
		clock := newLockedClock(time.Unix(1800000000, 0))
		dbPath := filepath.Join(t.TempDir(), "legacy.db")
		seed, err := store.Open(dbPath, clock)
		if err != nil {
			t.Fatal(err)
		}
		seedSrv := startMixedHub(seed)
		seedClient := &hubclient.Client{Config: mixedHubConfig{url: seedSrv.URL, token: "secret"}}
		reg, err := seedClient.RegisterSession(t.Context(), hubclient.SessionRequest{
			Client: "claude", Host: "host", SessionID: "S-leg-http",
			CWD: "/repo/w", Worktree: "/repo/w", Branch: "main", Source: "hook",
		})
		seedSrv.Close()
		if closeErr := seed.Close(); closeErr != nil {
			t.Fatal(closeErr)
		}
		if err != nil {
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
		srv := startMixedHub(upgraded)
		defer srv.Close()
		c := &hubclient.Client{Config: mixedHubConfig{url: srv.URL, token: "secret"}}

		recHBLegacy, err := c.Heartbeat(t.Context(), reg.ExecutionID, hubclient.HeartbeatRequest{
			Generation: reg.Generation, Source: "shim",
		})
		if err != nil {
			t.Fatal(err)
		}
		if legCls, err := recHBLegacy.Classify(); err != nil || legCls.Kind != hubclient.CandidateUnclassified || recHBLegacy.HookSeen.Known {
			t.Fatalf("un-opted heartbeat leaked classification: cand=%+v cls=%+v err=%v", recHBLegacy, legCls, err)
		}
		liveHBOpted, err := c.Heartbeat(t.Context(), reg.ExecutionID, hubclient.HeartbeatRequest{
			Generation: reg.Generation, Source: "shim", SuccessionProof: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		hbCls, err := liveHBOpted.Classify()
		if err != nil || hbCls.Kind != hubclient.CandidateLegacy || !hbCls.HookSeen {
			t.Fatalf("migrated heartbeat classification=%+v err=%v", hbCls, err)
		}
		unoptedBound, err := c.Bind(t.Context(), hubclient.BindRequest{Client: "claude", Host: "host", Execution: reg.ExecutionID})
		if err != nil {
			t.Fatal(err)
		}
		if uCls, err := unoptedBound.Classify(); err != nil || uCls != (hubclient.Classification{Kind: hubclient.CandidateUnclassified, HookSeen: true}) || unoptedBound.PriorExecutionID.Known {
			t.Fatalf("un-opted migrated bind leaked classification: cand=%+v cls=%+v err=%v", unoptedBound, uCls, err)
		}
		bound, err := c.Bind(t.Context(), hubclient.BindRequest{Client: "claude", Host: "host", Execution: reg.ExecutionID, SuccessionProof: true})
		if err != nil {
			t.Fatal(err)
		}
		bCls, err := bound.Classify()
		if err != nil || bCls.Kind != hubclient.CandidateLegacy || !bCls.HookSeen {
			t.Fatalf("migrated bind classification=%+v err=%v", bCls, err)
		}
	})

	t.Run("stage1b client against v0.1.15 hub and unreleased stage1a hub", func(t *testing.T) {
		var hitStage1a, hitRegister atomic.Int64
		mux := http.NewServeMux()
		mux.HandleFunc("POST /v1/bind", func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{
				"execution_id":"ex_old","generation":1,"agent":{"id":"ag_1","name":"codex-h-w"},
				"session_id":"S1","worktree":"/w","capabilities":["shell_scope","file_claims"],"hook_seen":true
			}`))
		})
		mux.HandleFunc("POST /v1/executions/{id}/heartbeat", func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"lease_ms":60000}`))
		})
		mux.HandleFunc("POST /v1/sessions", func(w http.ResponseWriter, r *http.Request) {
			hitRegister.Add(1)
			w.WriteHeader(http.StatusInternalServerError)
		})
		mux.HandleFunc("POST /v1/sessions/succeed", func(w http.ResponseWriter, r *http.Request) {
			hitStage1a.Add(1)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"execution_id":"ex_unexpected","generation":2}`))
		})
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"unknown API route","details":{}}}`))
		})

		srv := httptest.NewServer(mux)
		defer srv.Close()
		c := &hubclient.Client{Config: mixedHubConfig{url: srv.URL, token: "tok"}}

		cand, err := c.Bind(t.Context(), hubclient.BindRequest{
			Client: "codex", Host: "h", SessionID: "S1", SuccessionProof: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		if cls, err := cand.Classify(); err != nil || cls != (hubclient.Classification{Kind: hubclient.CandidateUnclassified, HookSeen: true}) {
			t.Fatalf("older hub bind cls=%+v err=%v", cls, err)
		}

		hb, err := c.Heartbeat(t.Context(), "ex_old", hubclient.HeartbeatRequest{
			Generation: 1, Source: "shim", SuccessionProof: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		if !hb.IsLeaseOnly() {
			t.Fatalf("expected lease-only heartbeat from older hub: %+v", hb)
		}

		_, err = c.Succeed(t.Context(), hubclient.SucceedRequest{
			Client: "codex", Host: "h", SessionID: "S1", Source: "shim",
			PriorGeneration: 1, PriorExecutionID: "ex_old",
		})
		if !hubclient.OlderHub(err) {
			t.Fatalf("expected OlderHub error when hub lacks /v1/sessions/succeed/exact, got %v", err)
		}
		if hitStage1a.Load() != 0 || hitRegister.Load() != 0 {
			t.Fatalf("unexpected fallback mutation calls: stage1a=%d register=%d", hitStage1a.Load(), hitRegister.Load())
		}
	})

	t.Run("v0.1.15 client HTTP calls remain inert on stage1b hub", func(t *testing.T) {
		clock := newLockedClock(time.Unix(1800000000, 0))
		s, err := store.Open(filepath.Join(t.TempDir(), "v15.db"), clock)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = s.Close() }()
		srv := startMixedHub(s)
		defer srv.Close()

		doRaw := func(path string, req map[string]any, want int) map[string]any {
			t.Helper()
			c := &hubclient.Client{Config: mixedHubConfig{url: srv.URL, token: "secret"}}
			var out map[string]any
			err := c.Do(t.Context(), http.MethodPost, path, req, &out)
			if want == http.StatusOK {
				if err != nil {
					t.Fatalf("%s: %v", path, err)
				}
				return out
			}
			if !hubclient.IsCode(err, "lost") {
				t.Fatalf("%s expected lost, got err=%v", path, err)
			}
			return nil
		}

		// Ordinary v0.1.15 hook -> bind -> heartbeat -> expire -> Choice A mcp G=0 succeeds identically.
		first := doRaw("/v1/sessions", map[string]any{
			"client": "codex", "host": "h", "session_id": "S-v15-ord", "cwd": "/w", "worktree": "/w", "source": "hook",
		}, http.StatusOK)
		bound := doRaw("/v1/bind", map[string]any{
			"client": "codex", "host": "h", "session_id": "S-v15-ord",
		}, http.StatusOK)
		if bound["execution_id"] != first["execution_id"] || bound["hook_seen"] != true {
			t.Fatalf("unexpected v0.1.15 bind reply: %+v", bound)
		}
		hb := doRaw("/v1/executions/"+first["execution_id"].(string)+"/heartbeat", map[string]any{
			"generation": 1, "source": "shim",
		}, http.StatusOK)
		if hb["lease_ms"] != float64(60000) {
			t.Fatalf("unexpected heartbeat lease: %+v", hb)
		}
		// Extra classified JSON keys are ignored by v0.1.15 callers such as Choice A fallback.
		clock.Advance(16 * time.Minute)
		fallback := doRaw("/v1/sessions", map[string]any{
			"client": "codex", "host": "h", "session_id": "S-v15-ord", "cwd": "/w", "worktree": "/w", "source": "mcp",
		}, http.StatusOK)
		if fallback["generation"] != float64(2) || fallback["execution_id"] == first["execution_id"] {
			t.Fatalf("Choice A deferred path broke for v0.1.15 client: %+v", fallback)
		}

		// Verify JSON marshal round-trip of unknown future capability preserves existing decoder tolerance.
		rawBytes, _ := json.Marshal(fallback)
		var minimal struct {
			ExecutionID string `json:"execution_id"`
			LeaseMS     int64  `json:"lease_ms"`
		}
		if err := json.Unmarshal(rawBytes, &minimal); err != nil || minimal.ExecutionID == "" || minimal.LeaseMS != 60000 {
			t.Fatalf("legacy partial decode failed: %+v err=%v", minimal, err)
		}
	})
}

// TestHTTPHeartbeatExactV0115GoldenBytes sends v0.1.15 binding.Resolver's exact wire request
// bytes (`{"generation":1,"source":"shim"}`) over real HTTP to the current hub handler and verifies
// byte-for-byte identity against responses captured verbatim from git tag v0.1.15 (commit 6deecd0)
// running v0.1.15's real store.Open, hub.New HTTP handler, and binding.Resolver with wall clock
// 1800000000 and tick 1000.
//
// Only the three randomly generated hex resource IDs from session creation (Agent.ID, ExecutionID,
// RepositoryID) are normalized to <AGENT_ID>, <EXEC_ID>, and <REPO_ID>. Everything else — key
// ordering, field set, values, and trailing newline — must match byte-for-byte across live and
// recovering states, both inside and outside a repository.
func TestHTTPHeartbeatExactV0115GoldenBytes(t *testing.T) {
	// Captured directly from v0.1.15 (6deecd0) real HTTP handler + binding.Resolver:
	//   inside/live:  {"agent":{"id":"ag_0789684ef5c176b45167a543b52a0773","name":"codex-host-w"},"capabilities":["shell_scope","file_claims"],"execution_id":"ex_f345482cbc1fee593e8c800e35308ace","generation":1,"lease_ms":60000,"repository_id":"rp_665422cd4aba2bd3d71055c14080eb6f","session_id":"S-gold-in","worktree":"/repo/w"}\n
	//   inside/rec:   {"agent":{"id":"ag_0789684ef5c176b45167a543b52a0773","name":"codex-host-w"},"attention":{"available":0,"high_water":1,"identity":null,"monitored":false,"unread":0},"capabilities":["shell_scope","file_claims"],"claims":[],"execution_id":"ex_f345482cbc1fee593e8c800e35308ace","generation":1,"handle":"ex_f345482cbc1fee593e8c800e35308ace","lease_ms":60000,"peers":[{"branch":"main","client":"codex","id":"ag_0789684ef5c176b45167a543b52a0773","name":"codex-host-w","state":"active","worktree":"/repo/w"}],"repository_id":"rp_665422cd4aba2bd3d71055c14080eb6f","session_id":"S-gold-in","unread":0,"worktree":"/repo/w"}\n
	//   outside/live: {"agent":{"id":"ag_21a82859d2eb0790b4be774c84023817","name":"codex-host-outside"},"capabilities":["shell_scope","file_claims"],"execution_id":"ex_5247fa9e20e4b4e950496af5e8f57f90","generation":1,"lease_ms":60000,"outside_git":true,"repository_id":"rp_2738b098791bfc0b46d42872147636d4","session_id":"S-gold-out","worktree":"/tmp/outside"}\n
	//   outside/rec:  {"agent":{"id":"ag_21a82859d2eb0790b4be774c84023817","name":"codex-host-outside"},"attention":{"available":0,"high_water":1,"identity":null,"monitored":false,"unread":0},"capabilities":["shell_scope","file_claims"],"claims":[],"execution_id":"ex_5247fa9e20e4b4e950496af5e8f57f90","generation":1,"handle":"ex_5247fa9e20e4b4e950496af5e8f57f90","lease_ms":60000,"outside_git":true,"peers":[{"branch":"","client":"codex","id":"ag_21a82859d2eb0790b4be774c84023817","name":"codex-host-outside","state":"active","worktree":"/tmp/outside"}],"repository_id":"rp_2738b098791bfc0b46d42872147636d4","session_id":"S-gold-out","unread":0,"worktree":"/tmp/outside"}\n
	const v0115HeartbeatReq = `{"generation":1,"source":"shim"}`

	cases := []struct {
		name     string
		outside  bool
		sid      string
		cwd      string
		wantLive string
		wantRec  string
	}{
		{
			name:     "inside repository",
			outside:  false,
			sid:      "S-gold-in",
			cwd:      "/repo/w",
			wantLive: "{\"agent\":{\"id\":\"<AGENT_ID>\",\"name\":\"codex-host-w\"},\"capabilities\":[\"shell_scope\",\"file_claims\"],\"execution_id\":\"<EXEC_ID>\",\"generation\":1,\"lease_ms\":60000,\"repository_id\":\"<REPO_ID>\",\"session_id\":\"S-gold-in\",\"worktree\":\"/repo/w\"}\n",
			wantRec:  "{\"agent\":{\"id\":\"<AGENT_ID>\",\"name\":\"codex-host-w\"},\"attention\":{\"available\":0,\"high_water\":1,\"identity\":null,\"monitored\":false,\"unread\":0},\"capabilities\":[\"shell_scope\",\"file_claims\"],\"claims\":[],\"execution_id\":\"<EXEC_ID>\",\"generation\":1,\"handle\":\"<EXEC_ID>\",\"lease_ms\":60000,\"peers\":[{\"branch\":\"main\",\"client\":\"codex\",\"id\":\"<AGENT_ID>\",\"name\":\"codex-host-w\",\"state\":\"active\",\"worktree\":\"/repo/w\"}],\"repository_id\":\"<REPO_ID>\",\"session_id\":\"S-gold-in\",\"unread\":0,\"worktree\":\"/repo/w\"}\n",
		},
		{
			name:     "outside repository",
			outside:  true,
			sid:      "S-gold-out",
			cwd:      "/tmp/outside",
			wantLive: "{\"agent\":{\"id\":\"<AGENT_ID>\",\"name\":\"codex-host-outside\"},\"capabilities\":[\"shell_scope\",\"file_claims\"],\"execution_id\":\"<EXEC_ID>\",\"generation\":1,\"lease_ms\":60000,\"outside_git\":true,\"repository_id\":\"<REPO_ID>\",\"session_id\":\"S-gold-out\",\"worktree\":\"/tmp/outside\"}\n",
			wantRec:  "{\"agent\":{\"id\":\"<AGENT_ID>\",\"name\":\"codex-host-outside\"},\"attention\":{\"available\":0,\"high_water\":1,\"identity\":null,\"monitored\":false,\"unread\":0},\"capabilities\":[\"shell_scope\",\"file_claims\"],\"claims\":[],\"execution_id\":\"<EXEC_ID>\",\"generation\":1,\"handle\":\"<EXEC_ID>\",\"lease_ms\":60000,\"outside_git\":true,\"peers\":[{\"branch\":\"\",\"client\":\"codex\",\"id\":\"<AGENT_ID>\",\"name\":\"codex-host-outside\",\"state\":\"active\",\"worktree\":\"/tmp/outside\"}],\"repository_id\":\"<REPO_ID>\",\"session_id\":\"S-gold-out\",\"unread\":0,\"worktree\":\"/tmp/outside\"}\n",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clock := newLockedClock(time.Unix(1800000000, 0))
			dbPath := filepath.Join(t.TempDir(), "golden.db")
			s, err := store.Open(dbPath, clock)
			if err != nil {
				t.Fatal(err)
			}
			activeStore := s
			var srv *httptest.Server
			t.Cleanup(func() {
				if srv != nil {
					srv.Close()
				}
				if activeStore != nil {
					_ = activeStore.Close()
				}
			})
			srv = startMixedHub(s)
			c := &hubclient.Client{Config: mixedHubConfig{url: srv.URL, token: "secret"}}

			req := hubclient.SessionRequest{
				Client: "codex", Host: "host", SessionID: tc.sid, CWD: tc.cwd, Source: "hook",
			}
			var reg hubclient.SessionResponse
			if tc.outside {
				reg, err = c.RegisterOutsideSession(t.Context(), req)
			} else {
				req.Worktree = tc.cwd
				req.Branch = "main"
				reg, err = c.RegisterSession(t.Context(), req)
			}
			if err != nil {
				t.Fatal(err)
			}

			norm := strings.NewReplacer(
				reg.Agent.ID, "<AGENT_ID>",
				reg.ExecutionID, "<EXEC_ID>",
				reg.RepositoryID, "<REPO_ID>",
			)
			postExact := func(baseURL string) string {
				t.Helper()
				httpReq, err := http.NewRequestWithContext(
					t.Context(),
					http.MethodPost,
					baseURL+"/v1/executions/"+reg.ExecutionID+"/heartbeat",
					bytes.NewReader([]byte(v0115HeartbeatReq)),
				)
				if err != nil {
					t.Fatal(err)
				}
				httpReq.Header.Set("Authorization", "Bearer secret")
				httpReq.Header.Set("Content-Type", "application/json")
				resp, err := http.DefaultClient.Do(httpReq)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = resp.Body.Close() }()
				body, err := io.ReadAll(resp.Body)
				if err != nil {
					t.Fatal(err)
				}
				if resp.StatusCode != http.StatusOK {
					t.Fatalf("status=%d body=%s", resp.StatusCode, body)
				}
				return norm.Replace(string(body))
			}

			if gotLive := postExact(srv.URL); gotLive != tc.wantLive {
				t.Fatalf("live heartbeat bytes mismatch:\ngot:  %q\nwant: %q", gotLive, tc.wantLive)
			}

			// Restart the store/hub so the Execution enters recovery, then send the exact same request bytes.
			srv.Close()
			if err := activeStore.Close(); err != nil {
				t.Fatal(err)
			}
			activeStore = nil
			reopened, err := store.Open(dbPath, clock)
			if err != nil {
				t.Fatal(err)
			}
			activeStore = reopened
			srv = startMixedHub(reopened)

			if gotRec := postExact(srv.URL); gotRec != tc.wantRec {
				t.Fatalf("recovering heartbeat bytes mismatch:\ngot:  %q\nwant: %q", gotRec, tc.wantRec)
			}
		})
	}
}

// TestHTTPUnoptedRoutesExactV0115GoldenBytes verifies that un-opted POST /v1/sessions,
// POST /v1/sessions/outside, and POST /v1/bind (all four lookup modes, inside and outside a
// repository) produce exact v0.1.15 JSON bytes, while opted-in requests ("succession_proof":true)
// advertise succession_proof in capabilities.
func TestHTTPUnoptedRoutesExactV0115GoldenBytes(t *testing.T) {
	clock := newLockedClock(time.Unix(1800000000, 0))
	s, err := store.Open(filepath.Join(t.TempDir(), "golden-routes.db"), clock)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	srv := startMixedHub(s)
	defer srv.Close()

	postBytes := func(path, rawReq string) []byte {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+path, strings.NewReader(rawReq))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer secret")
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", path, resp.StatusCode, body)
		}
		return body
	}

	assertOptedHasSuccessionProof := func(path string, req map[string]any, nestedSession bool) {
		t.Helper()
		req["succession_proof"] = true
		raw, err := json.Marshal(req)
		if err != nil {
			t.Fatal(err)
		}
		var out map[string]any
		if err := json.Unmarshal(postBytes(path, string(raw)), &out); err != nil {
			t.Fatal(err)
		}
		target := out
		if nestedSession {
			target = out["session"].(map[string]any)
		}
		caps, ok := target["capabilities"].([]any)
		if !ok || len(caps) != 3 || caps[0] != "shell_scope" || caps[1] != "file_claims" || caps[2] != "succession_proof" {
			t.Fatalf("%s opted capabilities=%#v, want [shell_scope file_claims succession_proof]", path, target["capabilities"])
		}
	}

	// 1. POST /v1/sessions (inside repo)
	sessRaw := postBytes("/v1/sessions", `{"client":"codex","host":"host","session_id":"S-gold-in","cwd":"/repo/w","worktree":"/repo/w","branch":"main","source":"hook","client_pid":701,"client_pid_start_ms":7701,"single_session":true}`)
	var sessDoc hubclient.SessionResponse
	if err := json.Unmarshal(sessRaw, &sessDoc); err != nil {
		t.Fatal(err)
	}
	normIn := strings.NewReplacer(
		sessDoc.Agent.ID, "<AGENT_ID>",
		sessDoc.ExecutionID, "<EXEC_ID>",
		sessDoc.RepositoryID, "<REPO_ID>",
	)
	wantSess := "{\"agent\":{\"id\":\"<AGENT_ID>\",\"name\":\"codex-host-w\"},\"attention\":{\"available\":0,\"high_water\":0,\"identity\":null,\"monitored\":false,\"unread\":0},\"capabilities\":[\"shell_scope\",\"file_claims\"],\"claims\":[],\"execution_id\":\"<EXEC_ID>\",\"generation\":1,\"handle\":\"<EXEC_ID>\",\"lease_ms\":900000,\"peers\":[{\"branch\":\"main\",\"client\":\"codex\",\"id\":\"<AGENT_ID>\",\"name\":\"codex-host-w\",\"state\":\"active\",\"worktree\":\"/repo/w\"}],\"repository_id\":\"<REPO_ID>\",\"session_id\":\"S-gold-in\",\"unread\":0,\"worktree\":\"/repo/w\"}\n"
	if got := normIn.Replace(string(sessRaw)); got != wantSess {
		t.Fatalf("/v1/sessions un-opted bytes mismatch:\ngot:  %q\nwant: %q", got, wantSess)
	}
	assertOptedHasSuccessionProof("/v1/sessions", map[string]any{
		"client": "codex", "host": "host", "session_id": "S-gold-in",
		"cwd": "/repo/w", "worktree": "/repo/w", "branch": "main", "source": "hook",
		"client_pid": 701, "client_pid_start_ms": 7701, "single_session": true,
	}, false)

	// 2. POST /v1/sessions/outside
	outRaw := postBytes("/v1/sessions/outside", `{"client":"codex","host":"host","session_id":"S-gold-out","cwd":"/tmp/outside","source":"hook"}`)
	var outDoc hubclient.SessionResponse
	if err := json.Unmarshal(outRaw, &outDoc); err != nil {
		t.Fatal(err)
	}
	normOut := strings.NewReplacer(
		outDoc.Agent.ID, "<AGENT_ID>",
		outDoc.ExecutionID, "<EXEC_ID>",
		outDoc.RepositoryID, "<REPO_ID>",
	)
	wantOutside := "{\"agent\":{\"id\":\"<AGENT_ID>\",\"name\":\"codex-host-outside\"},\"attention\":{\"available\":0,\"high_water\":2,\"identity\":null,\"monitored\":false,\"unread\":0},\"capabilities\":[\"shell_scope\",\"file_claims\"],\"claims\":[],\"execution_id\":\"<EXEC_ID>\",\"generation\":1,\"handle\":\"<EXEC_ID>\",\"lease_ms\":900000,\"outside_git\":true,\"peers\":[{\"branch\":\"\",\"client\":\"codex\",\"id\":\"<AGENT_ID>\",\"name\":\"codex-host-outside\",\"state\":\"active\",\"worktree\":\"/tmp/outside\"}],\"repository_id\":\"<REPO_ID>\",\"session_id\":\"S-gold-out\",\"unread\":0,\"worktree\":\"/tmp/outside\"}\n"
	if got := normOut.Replace(string(outRaw)); got != wantOutside {
		t.Fatalf("/v1/sessions/outside un-opted bytes mismatch:\ngot:  %q\nwant: %q", got, wantOutside)
	}
	assertOptedHasSuccessionProof("/v1/sessions/outside", map[string]any{
		"client": "codex", "host": "host", "session_id": "S-gold-out",
		"cwd": "/tmp/outside", "source": "hook",
	}, false)

	// 3. POST /v1/bind across all 4 lookup modes (inside) + explicit handle (outside)
	wantBind := "{\"agent\":{\"id\":\"<AGENT_ID>\",\"name\":\"codex-host-w\"},\"capabilities\":[\"shell_scope\",\"file_claims\"],\"execution_id\":\"<EXEC_ID>\",\"generation\":1,\"hook_seen\":true,\"repository_id\":\"<REPO_ID>\",\"session_id\":\"S-gold-in\",\"worktree\":\"/repo/w\"}\n"
	wantCombined := "{\"agent\":{\"id\":\"<AGENT_ID>\",\"name\":\"codex-host-w\"},\"capabilities\":[\"shell_scope\",\"file_claims\"],\"execution_id\":\"<EXEC_ID>\",\"generation\":1,\"hook_seen\":true,\"match\":\"process_session\",\"repository_id\":\"<REPO_ID>\",\"session_id\":\"S-gold-in\",\"worktree\":\"/repo/w\"}\n"
	wantBindOut := "{\"agent\":{\"id\":\"<AGENT_ID>\",\"name\":\"codex-host-outside\"},\"capabilities\":[\"shell_scope\",\"file_claims\"],\"execution_id\":\"<EXEC_ID>\",\"generation\":1,\"hook_seen\":true,\"outside_git\":true,\"repository_id\":\"<REPO_ID>\",\"session_id\":\"S-gold-out\",\"worktree\":\"/tmp/outside\"}\n"
	for _, tc := range []struct {
		name string
		req  map[string]any
		norm *strings.Replacer
		want string
	}{
		{"handle", map[string]any{"client": "codex", "host": "host", "execution": sessDoc.ExecutionID}, normIn, wantBind},
		{"session", map[string]any{"client": "codex", "host": "host", "session_id": "S-gold-in"}, normIn, wantBind},
		{"process", map[string]any{"client": "codex", "host": "host", "client_pid": 701, "client_pid_start_ms": 7701}, normIn, wantBind},
		{"combined", map[string]any{"client": "codex", "host": "host", "session_id": "S-gold-in", "client_pid": 701, "client_pid_start_ms": 7701}, normIn, wantCombined},
		{"outside", map[string]any{"client": "codex", "host": "host", "execution": outDoc.ExecutionID}, normOut, wantBindOut},
	} {
		t.Run("bind/"+tc.name, func(t *testing.T) {
			rawReq, _ := json.Marshal(tc.req)
			got := tc.norm.Replace(string(postBytes("/v1/bind", string(rawReq))))
			if got != tc.want {
				t.Fatalf("/v1/bind (%s) un-opted bytes mismatch:\ngot:  %q\nwant: %q", tc.name, got, tc.want)
			}
			assertOptedHasSuccessionProof("/v1/bind", tc.req, false)
		})
	}

	// 4. Combined POST /v1/shell/reconcile and POST /v1/shell/scoped session replies.
	shellReq := `{"client":"codex","host":"host","session_id":"S-gold-shell","cwd":"/repo/shell","worktree":"/repo/shell","source":"hook","kind":"before","status":{}}`
	shellRaw := postBytes("/v1/shell/reconcile", shellReq)
	var shellDoc hubclient.ShellResponse
	if err := json.Unmarshal(shellRaw, &shellDoc); err != nil {
		t.Fatal(err)
	}
	normShell := strings.NewReplacer(shellDoc.Session.ExecutionID, "<EXEC_ID>")
	wantShell := "{\"attention\":{\"available\":0,\"high_water\":4,\"identity\":null,\"monitored\":false,\"unread\":0},\"conflicts\":[],\"message\":\"\",\"session\":{\"capabilities\":[\"shell_scope\",\"file_claims\"],\"execution_id\":\"<EXEC_ID>\",\"generation\":1}}\n"
	if got := normShell.Replace(string(shellRaw)); got != wantShell {
		t.Fatalf("/v1/shell/reconcile un-opted bytes mismatch:\ngot:  %q\nwant: %q", got, wantShell)
	}
	assertOptedHasSuccessionProof("/v1/shell/reconcile", map[string]any{
		"client": "codex", "host": "host", "session_id": "S-gold-shell-opt",
		"cwd": "/repo/shell-opt", "worktree": "/repo/shell-opt", "source": "hook",
		"kind": "before", "status": map[string]string{},
	}, true)

	scopedReq := `{"client":"codex","host":"host","session_id":"S-gold-scoped","cwd":"/repo/scoped","worktree":"/repo/scoped","source":"hook","kind":"before","status":{},"scope":{"mode":"dirty","submodules":["lib"]}}`
	scopedRaw := postBytes("/v1/shell/scoped", scopedReq)
	var scopedDoc hubclient.ShellResponse
	if err := json.Unmarshal(scopedRaw, &scopedDoc); err != nil {
		t.Fatal(err)
	}
	normScoped := strings.NewReplacer(scopedDoc.Session.ExecutionID, "<EXEC_ID>")
	wantScoped := "{\"attention\":{\"available\":0,\"high_water\":6,\"identity\":null,\"monitored\":false,\"unread\":0},\"conflicts\":[],\"message\":\"\",\"session\":{\"capabilities\":[\"shell_scope\",\"file_claims\"],\"execution_id\":\"<EXEC_ID>\",\"generation\":1}}\n"
	if got := normScoped.Replace(string(scopedRaw)); got != wantScoped {
		t.Fatalf("/v1/shell/scoped un-opted bytes mismatch:\ngot:  %q\nwant: %q", got, wantScoped)
	}
	assertOptedHasSuccessionProof("/v1/shell/scoped", map[string]any{
		"client": "codex", "host": "host", "session_id": "S-gold-scoped-opt",
		"cwd": "/repo/scoped-opt", "worktree": "/repo/scoped-opt", "source": "hook",
		"kind": "before", "status": map[string]string{},
		"scope": map[string]any{"mode": "dirty", "submodules": []string{"lib"}},
	}, true)
}
