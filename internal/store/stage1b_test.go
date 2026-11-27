package store

import (
	"database/sql"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func readHookSeenMS(t *testing.T, s *Store, id string) int64 {
	t.Helper()
	var seen int64
	if err := s.db.QueryRow("SELECT hook_seen_ms FROM Execution WHERE id=?", id).Scan(&seen); err != nil {
		t.Fatal(err)
	}
	return seen
}

func assertLegacyReply(t *testing.T, reply map[string]any, expectHookSeen bool) {
	t.Helper()
	caps, ok := reply["capabilities"].([]string)
	if !ok {
		t.Fatalf("capabilities type=%T value=%#v, want []string", reply["capabilities"], reply["capabilities"])
	}
	if !slices.Equal(caps, []string{"shell_scope", "file_claims"}) {
		t.Fatalf("un-opted capabilities=%v, want legacy [shell_scope file_claims]", caps)
	}
	for _, key := range []string{"prior_execution_id", "creation_source", "creation_proof"} {
		if _, exists := reply[key]; exists {
			t.Fatalf("un-opted reply leaked stage 1b key %q: %+v", key, reply)
		}
	}
	if _, exists := reply["hook_seen"]; exists != expectHookSeen {
		t.Fatalf("un-opted reply hook_seen presence=%t want=%t: %+v", exists, expectHookSeen, reply)
	}
}

func assertLegacyHeartbeatReply(t *testing.T, reply map[string]any) {
	t.Helper()
	assertLegacyReply(t, reply, false)
}

func assertClassifiedReply(t *testing.T, reply map[string]any, wantPrior, wantSource, wantProof any, wantHookSeen bool) {
	t.Helper()
	caps, ok := reply["capabilities"].([]string)
	if !ok {
		t.Fatalf("capabilities type=%T value=%#v, want []string", reply["capabilities"], reply["capabilities"])
	}
	for _, want := range []string{"shell_scope", "file_claims", "succession_proof"} {
		if !slices.Contains(caps, want) {
			t.Fatalf("capabilities=%v missing %q", caps, want)
		}
	}
	for _, key := range []string{"prior_execution_id", "creation_source", "creation_proof", "hook_seen"} {
		if _, exists := reply[key]; !exists {
			t.Fatalf("reply missing explicit key %q: %+v", key, reply)
		}
	}
	if reply["prior_execution_id"] != wantPrior {
		t.Fatalf("prior_execution_id=%#v, want %#v (reply=%+v)", reply["prior_execution_id"], wantPrior, reply)
	}
	if reply["creation_source"] != wantSource {
		t.Fatalf("creation_source=%#v, want %#v (reply=%+v)", reply["creation_source"], wantSource, reply)
	}
	if reply["creation_proof"] != wantProof {
		t.Fatalf("creation_proof=%#v, want %#v (reply=%+v)", reply["creation_proof"], wantProof, reply)
	}
	if reply["hook_seen"] != wantHookSeen {
		t.Fatalf("hook_seen=%#v, want %t (reply=%+v)", reply["hook_seen"], wantHookSeen, reply)
	}
}

func TestStage1bClassifiedRepliesAcrossRoutesAndMigratedRows(t *testing.T) {
	s, clock, dbPath := testStore(t)

	// 1. Initial hook registration in git repo and outside git: un-opted stays legacy; opted emits classification.
	hookRegUnopted := call(t, s, "sessions", Request{
		Client: "claude", Host: "host", SessionID: "S-cls",
		Cwd: "/repo/w", Worktree: "/repo/w", Branch: "main", Source: "hook",
		ClientPID: 501, ClientPIDStartMS: 9001,
	})
	assertLegacyReply(t, hookRegUnopted, false)
	hookReg := call(t, s, "sessions", Request{
		Client: "claude", Host: "host", SessionID: "S-cls",
		Cwd: "/repo/w", Worktree: "/repo/w", Branch: "main", Source: "hook",
		ClientPID: 501, ClientPIDStartMS: 9001, SuccessionProof: true,
	})
	hookID := hookReg["execution_id"].(string)
	assertClassifiedReply(t, hookReg, nil, "hook", "initial", true)

	outsideRegUnopted := call(t, s, "sessions_outside", Request{
		Client: "codex", Host: "host", SessionID: "S-out",
		Cwd: "/tmp/outside", Source: "hook",
	})
	assertLegacyReply(t, outsideRegUnopted, false)
	outsideReg := call(t, s, "sessions_outside", Request{
		Client: "codex", Host: "host", SessionID: "S-out",
		Cwd: "/tmp/outside", Source: "hook", SuccessionProof: true,
	})
	assertClassifiedReply(t, outsideReg, nil, "hook", "initial", true)

	// 2. All four bind modes stay legacy un-opted and carry classified candidate fields when opted in.
	single := call(t, s, "sessions", Request{
		Client: "codex", Host: "host", SessionID: "S-comb",
		Cwd: "/repo/w", Worktree: "/repo/w", Branch: "main", Source: "hook",
		ClientPID: 601, ClientPIDStartMS: 9601, SingleSession: true,
	})
	singleID := single["execution_id"].(string)
	for _, tc := range []struct {
		name string
		req  Request
	}{
		{"explicit handle", Request{Client: "claude", Host: "host", Execution: hookID}},
		{"session", Request{Client: "claude", Host: "host", SessionID: "S-cls"}},
		{"process", Request{Client: "claude", Host: "host", ClientPID: 501, ClientPIDStartMS: 9001}},
		{"combined", Request{Client: "codex", Host: "host", SessionID: "S-comb", ClientPID: 601, ClientPIDStartMS: 9601}},
	} {
		t.Run("bind/"+tc.name, func(t *testing.T) {
			unopted := call(t, s, "bind", tc.req)
			assertLegacyReply(t, unopted, true)
			optedReq := tc.req
			optedReq.SuccessionProof = true
			b := call(t, s, "bind", optedReq)
			assertClassifiedReply(t, b, nil, "hook", "initial", true)
		})
	}
	if b := call(t, s, "bind", Request{Client: "codex", Host: "host", Execution: singleID}); b["execution_id"] != singleID {
		t.Fatalf("unexpected handle bind: %+v", b)
	}

	// 3. Heartbeats stay byte-compatible without SuccessionProof, and emit classification with SuccessionProof=true.
	hbLiveLegacy := call(t, s, "heartbeat", Request{ExecutionID: hookID, Generation: 1, Source: "shim"})
	assertLegacyHeartbeatReply(t, hbLiveLegacy)
	hbLive := call(t, s, "heartbeat", Request{ExecutionID: hookID, Generation: 1, Source: "shim", SuccessionProof: true})
	assertClassifiedReply(t, hbLive, nil, "hook", "initial", true)

	s = restart(t, s, dbPath, clock)
	hbRecLegacy := call(t, s, "heartbeat", Request{ExecutionID: hookID, Generation: 1, Source: "shim"})
	assertLegacyHeartbeatReply(t, hbRecLegacy)
	hbRec := call(t, s, "heartbeat", Request{ExecutionID: hookID, Generation: 1, Source: "shim", SuccessionProof: true})
	assertClassifiedReply(t, hbRec, nil, "hook", "initial", true)

	// 4. Succeed creates and replays a classified hookless shim succession child.
	clock.advance(16 * time.Minute)
	call(t, s, "expire", Request{})
	succReq := Request{
		Client: "claude", Host: "host", SessionID: "S-cls", Source: "shim",
		PriorGeneration: 1, PriorExecutionID: hookID,
		ClientPID: 501, ClientPIDStartMS: 9001,
	}
	child := call(t, s, "succeed", succReq)
	childID := child["execution_id"].(string)
	assertClassifiedReply(t, child, hookID, "shim", "shim_succession", false)

	replayed := call(t, s, "succeed", succReq)
	if replayed["execution_id"] != childID {
		t.Fatalf("replay changed child: %+v vs %+v", child, replayed)
	}
	assertClassifiedReply(t, replayed, hookID, "shim", "shim_succession", false)

	// 5. Legacy pre-029 rows upgraded by migration 029 return explicit nil provenance.
	legacyDB := filepath.Join(t.TempDir(), "legacy.db")
	legacyClock := &fakeClock{wall: time.Unix(1800000000, 0)}
	seed, err := Open(legacyDB, legacyClock)
	if err != nil {
		t.Fatal(err)
	}
	leg := call(t, seed, "sessions", Request{
		Client: "codex", Host: "host", SessionID: "S-leg",
		Cwd: "/repo/leg", Worktree: "/repo/leg", Branch: "main", Source: "hook",
		ClientPID: 701, ClientPIDStartMS: 9701, SingleSession: true,
	})
	legID := leg["execution_id"].(string)
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}

	raw, err := sql.Open("sqlite", legacyDB)
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

	upgraded, err := Open(legacyDB, legacyClock)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = upgraded.Close() }()

	// On first open after restart, Execution enters recovery; legacy heartbeat stays compatible,
	// while SuccessionProof=true resumes with nil provenance.
	legRecLegacy := call(t, upgraded, "heartbeat", Request{ExecutionID: legID, Generation: 1, Source: "shim"})
	assertLegacyHeartbeatReply(t, legRecLegacy)

	// Reopen again so opted-in recovering heartbeat is exercised too, then test live both ways.
	upgraded = restart(t, upgraded, legacyDB, legacyClock)
	legRecHB := call(t, upgraded, "heartbeat", Request{ExecutionID: legID, Generation: 1, Source: "shim", SuccessionProof: true})
	assertClassifiedReply(t, legRecHB, nil, nil, nil, true)

	legLiveLegacy := call(t, upgraded, "heartbeat", Request{ExecutionID: legID, Generation: 1, Source: "watch"})
	assertLegacyHeartbeatReply(t, legLiveLegacy)

	legLiveHB := call(t, upgraded, "heartbeat", Request{ExecutionID: legID, Generation: 1, Source: "watch", SuccessionProof: true})
	assertClassifiedReply(t, legLiveHB, nil, nil, nil, true)

	for _, req := range []Request{
		{Client: "codex", Host: "host", Execution: legID},
		{Client: "codex", Host: "host", SessionID: "S-leg"},
		{Client: "codex", Host: "host", ClientPID: 701, ClientPIDStartMS: 9701},
		{Client: "codex", Host: "host", SessionID: "S-leg", ClientPID: 701, ClientPIDStartMS: 9701},
	} {
		unopted := call(t, upgraded, "bind", req)
		assertLegacyReply(t, unopted, true)
		req.SuccessionProof = true
		b := call(t, upgraded, "bind", req)
		assertClassifiedReply(t, b, nil, nil, nil, true)
	}
}

func TestStage1bCompatibilityGuardsAndHookUnlock(t *testing.T) {
	t.Run("bind opt-in and hook unlock across all modes", func(t *testing.T) {
		s, clock, _ := testStore(t)
		parent := call(t, s, "sessions", Request{
			Client: "claude", Host: "host", SessionID: "S-bind",
			Cwd: "/repo/w", Worktree: "/repo/w", Branch: "main", Source: "hook",
			ClientPID: 101, ClientPIDStartMS: 1001,
		})
		parentID := parent["execution_id"].(string)
		clock.advance(16 * time.Minute)
		call(t, s, "expire", Request{})

		child := call(t, s, "succeed", Request{
			Client: "claude", Host: "host", SessionID: "S-bind", Source: "shim",
			PriorGeneration: 1, PriorExecutionID: parentID,
			ClientPID: 101, ClientPIDStartMS: 1001,
		})
		childID := child["execution_id"].(string)

		// Without succession_proof opt-in, handle/session/process binds return lost;
		// combined bind still sees hook_seen_ms == 0 and stays unbound.
		for _, tc := range []struct {
			name string
			req  Request
			code string
		}{
			{"handle without opt-in", Request{Client: "claude", Host: "host", Execution: childID}, "lost"},
			{"session without opt-in", Request{Client: "claude", Host: "host", SessionID: "S-bind"}, "lost"},
			{"process without opt-in", Request{Client: "claude", Host: "host", ClientPID: 101, ClientPIDStartMS: 1001}, "lost"},
			{"combined before hook", Request{Client: "claude", Host: "host", SessionID: "S-bind", ClientPID: 101, ClientPIDStartMS: 1001, SuccessionProof: true}, "unbound"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				expectError(t, s, "bind", tc.req, tc.code)
			})
		}

		// With succession_proof=true, handle/session/process bind returns classified candidate.
		for _, tc := range []struct {
			name string
			req  Request
		}{
			{"handle with opt-in", Request{Client: "claude", Host: "host", Execution: childID, SuccessionProof: true}},
			{"session with opt-in", Request{Client: "claude", Host: "host", SessionID: "S-bind", SuccessionProof: true}},
			{"process with opt-in", Request{Client: "claude", Host: "host", ClientPID: 101, ClientPIDStartMS: 1001, SuccessionProof: true}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				b := call(t, s, "bind", tc.req)
				assertClassifiedReply(t, b, parentID, "shim", "shim_succession", false)
			})
		}

		// Live heartbeat allows ONLY source="shim" + SuccessionProof=true.
		before := readExecutionRow(t, s, childID)
		for _, tc := range []struct {
			name   string
			source string
			proof  bool
		}{
			{"hook without opt-in", "hook", false},
			{"hook with opt-in", "hook", true},
			{"watch without opt-in", "watch", false},
			{"watch with opt-in", "watch", true},
			{"shim without opt-in", "shim", false},
		} {
			t.Run("live_hb_refused/"+tc.name, func(t *testing.T) {
				expectError(t, s, "heartbeat", Request{
					ExecutionID: childID, Generation: 2, Source: tc.source, SuccessionProof: tc.proof,
				}, "lost")
				after := readExecutionRow(t, s, childID)
				if after.deadline != before.deadline || readHookSeenMS(t, s, childID) != 0 {
					t.Fatalf("refused heartbeat mutated row: before=%+v after=%+v", before, after)
				}
			})
		}
		clock.advance(10 * time.Second)
		hbOK := call(t, s, "heartbeat", Request{
			ExecutionID: childID, Generation: 2, Source: "shim", SuccessionProof: true,
		})
		assertClassifiedReply(t, hbOK, parentID, "shim", "shim_succession", false)
		if after := readExecutionRow(t, s, childID); after.deadline <= before.deadline || readHookSeenMS(t, s, childID) != 0 {
			t.Fatalf("shim opt-in heartbeat failed to renew cleanly: before=%+v after=%+v", before, after)
		}

		// Genuine hook arrival unlocks ordinary bind/watch/heartbeat while keeping creation provenance.
		hookAttach := call(t, s, "sessions", Request{
			Client: "claude", Host: "host", SessionID: "S-bind",
			Cwd: "/repo/w", Worktree: "/repo/w", Branch: "main", Source: "hook",
			ClientPID: 101, ClientPIDStartMS: 1001, SuccessionProof: true,
		})
		if hookAttach["execution_id"] != childID || hookAttach["generation"] != int64(2) {
			t.Fatalf("hook created wrong row: %+v", hookAttach)
		}
		assertClassifiedReply(t, hookAttach, parentID, "shim", "shim_succession", true)
		if readHookSeenMS(t, s, childID) == 0 {
			t.Fatalf("hook_seen_ms remained zero after hook attach")
		}
		for _, req := range []Request{
			{Client: "claude", Host: "host", Execution: childID},
			{Client: "claude", Host: "host", SessionID: "S-bind"},
			{Client: "claude", Host: "host", ClientPID: 101, ClientPIDStartMS: 1001},
		} {
			unopted := call(t, s, "bind", req)
			assertLegacyReply(t, unopted, true)
			req.SuccessionProof = true
			b := call(t, s, "bind", req)
			assertClassifiedReply(t, b, parentID, "shim", "shim_succession", true)
		}
		hbWatchLegacy := call(t, s, "heartbeat", Request{ExecutionID: childID, Generation: 2, Source: "watch"})
		assertLegacyHeartbeatReply(t, hbWatchLegacy)
		hbWatch := call(t, s, "heartbeat", Request{ExecutionID: childID, Generation: 2, Source: "watch", SuccessionProof: true})
		assertClassifiedReply(t, hbWatch, parentID, "shim", "shim_succession", true)
	})

	t.Run("recovering heartbeat and non-hook registration guards", func(t *testing.T) {
		s, clock, dbPath := testStore(t)
		parent := call(t, s, "sessions", Request{
			Client: "codex", Host: "host", SessionID: "S-rec-g",
			Cwd: "/repo/w", Worktree: "/repo/w", Branch: "main", Source: "hook",
		})
		parentID := parent["execution_id"].(string)
		clock.advance(16 * time.Minute)
		call(t, s, "expire", Request{})

		child := call(t, s, "succeed", Request{
			Client: "codex", Host: "host", SessionID: "S-rec-g", Source: "shim",
			PriorGeneration: 1, PriorExecutionID: parentID,
		})
		childID := child["execution_id"].(string)

		// Non-hook registrations cannot attach to a live hookless succession child on either route.
		beforeLive := databaseState(t, s)
		for _, source := range []string{"", "mcp", "shim", "watch"} {
			for _, op := range []string{"sessions", "sessions_outside"} {
				req := Request{
					Client: "codex", Host: "host", SessionID: "S-rec-g",
					Cwd: "/repo/w", Source: source,
				}
				if op == "sessions" {
					req.Worktree = "/repo/w"
				}
				out, err := s.Call(t.Context(), op, req)
				if out != nil {
					t.Fatalf("%s(%q) unexpectedly succeeded: %+v", op, source, out)
				}
				assertRegistrationLost(t, err, 2)
			}
		}
		if databaseState(t, s) != beforeLive {
			t.Fatalf("refused non-hook attach mutated DB")
		}

		// Restart hub: child enters recovery; only shim+SuccessionProof resumes via heartbeat,
		// while non-hook registration stays refused on both routes.
		s = restart(t, s, dbPath, clock)
		beforeRec := databaseState(t, s)
		for _, source := range []string{"hook", "watch", "shim"} {
			expectError(t, s, "heartbeat", Request{
				ExecutionID: childID, Generation: 2, Source: source,
				SuccessionProof: source != "shim", // hook/watch even WITH opt-in must still fail!
			}, "lost")
		}
		for _, base := range []Request{
			{Client: "codex", Host: "host", SessionID: "S-rec-g", Cwd: "/repo/w", Source: "mcp"},
			{Client: "codex", Host: "host", SessionID: "S-rec-g", Cwd: "/repo/w", Source: "shim", Generation: 2, ClientPID: 202, ClientPIDStartMS: 2002},
		} {
			for _, op := range []string{"sessions", "sessions_outside"} {
				req := base
				if op == "sessions" {
					req.Worktree = "/repo/w"
				}
				out, err := s.Call(t.Context(), op, req)
				if out != nil {
					t.Fatalf("%s recovery attach succeeded: %+v", op, out)
				}
				assertRegistrationLost(t, err, 2)
			}
		}
		if databaseState(t, s) != beforeRec {
			t.Fatalf("refused recovery calls mutated DB")
		}

		// Shim heartbeat with opt-in resumes recovery cleanly.
		resumed := call(t, s, "heartbeat", Request{
			ExecutionID: childID, Generation: 2, Source: "shim", SuccessionProof: true,
		})
		assertClassifiedReply(t, resumed, parentID, "shim", "shim_succession", false)

		// Restart again so we also verify genuine hook recovery (Generation: 2, Source: "hook") unlocks.
		s = restart(t, s, dbPath, clock)
		hookRec := call(t, s, "sessions", Request{
			Client: "codex", Host: "host", SessionID: "S-rec-g",
			Cwd: "/repo/w", Worktree: "/repo/w", Branch: "main", Source: "hook", Generation: 2,
			SuccessionProof: true,
		})
		assertClassifiedReply(t, hookRec, parentID, "shim", "shim_succession", true)
	})

	t.Run("loss and predecessor pruning keep non-hook g0 refused", func(t *testing.T) {
		s, clock, _ := testStore(t)
		parent := call(t, s, "sessions_outside", Request{
			Client: "codex", Host: "host", SessionID: "S-prune", Cwd: "/tmp/out", Source: "hook",
		})
		parentID := parent["execution_id"].(string)
		clock.advance(16 * time.Minute)
		call(t, s, "expire", Request{})

		child := call(t, s, "succeed", Request{
			Client: "codex", Host: "host", SessionID: "S-prune", Source: "shim",
			PriorGeneration: 1, PriorExecutionID: parentID,
		})
		childID := child["execution_id"].(string)

		// Expire the hookless succession child AND delete its predecessor row to simulate retention sweep.
		clock.advance(2 * time.Minute)
		call(t, s, "expire", Request{})
		if _, err := s.db.Exec("DELETE FROM Execution WHERE id=?", parentID); err != nil {
			t.Fatal(err)
		}

		before := databaseState(t, s)
		for _, source := range []string{"", "mcp", "shim", "watch"} {
			for _, op := range []string{"sessions", "sessions_outside"} {
				out, err := s.Call(t.Context(), op, Request{
					Client: "codex", Host: "host", SessionID: "S-prune",
					Cwd: "/tmp/out", Source: source,
				})
				if out != nil {
					t.Fatalf("%s(%q) replaced lost hookless child: %+v", op, source, out)
				}
				assertRegistrationLost(t, err, 2)
			}
		}
		if databaseState(t, s) != before {
			t.Fatalf("refused G=0 replacement mutated DB")
		}

		// A genuine hook G=0 registration still succeeds at G=3.
		hookNext := call(t, s, "sessions_outside", Request{
			Client: "codex", Host: "host", SessionID: "S-prune", Cwd: "/tmp/out", Source: "hook",
			SuccessionProof: true,
		})
		if hookNext["generation"] != int64(3) || hookNext["execution_id"] == childID {
			t.Fatalf("expected hook G=3 after lost hookless child: %+v", hookNext)
		}
		assertClassifiedReply(t, hookNext, childID, "hook", "absent", true)
	})

	t.Run("choice A refuses non-hook g0 over ordinary lost hook session", func(t *testing.T) {
		s, clock, _ := testStore(t)
		first := call(t, s, "sessions", Request{
			Client: "codex", Host: "host", SessionID: "S-choicea",
			Cwd: "/repo/w", Worktree: "/repo/w", Branch: "main", Source: "hook",
		})
		firstID := first["execution_id"].(string)
		clock.advance(16 * time.Minute)
		call(t, s, "expire", Request{})

		before := databaseState(t, s)
		for _, source := range []string{"", "mcp", "shim", "watch"} {
			for _, op := range []string{"sessions", "sessions_outside"} {
				req := Request{
					Client: "codex", Host: "host", SessionID: "S-choicea",
					Cwd: "/repo/w", Source: source,
				}
				if op == "sessions" {
					req.Worktree = "/repo/w"
				}
				out, err := s.Call(t.Context(), op, req)
				if out != nil {
					t.Fatalf("Choice A allowed %s(%q) over lost hook history: %+v", op, source, out)
				}
				assertRegistrationLost(t, err, 1)
			}
		}
		if databaseState(t, s) != before {
			t.Fatal("refused Choice A non-hook g0 registration mutated DB")
		}

		// A genuine hook still advances S-choicea to G=2 (legacy un-opted; classified when opted in).
		nextUnopted := call(t, s, "sessions", Request{
			Client: "codex", Host: "host", SessionID: "S-choicea",
			Cwd: "/repo/w", Worktree: "/repo/w", Branch: "main", Source: "hook",
		})
		if nextUnopted["generation"] != int64(2) || nextUnopted["execution_id"] == firstID {
			t.Fatalf("hook registration after loss failed: %+v", nextUnopted)
		}
		assertLegacyReply(t, nextUnopted, false)
		next := call(t, s, "sessions", Request{
			Client: "codex", Host: "host", SessionID: "S-choicea",
			Cwd: "/repo/w", Worktree: "/repo/w", Branch: "main", Source: "hook",
			SuccessionProof: true,
		})
		if next["generation"] != int64(2) || next["execution_id"] != nextUnopted["execution_id"] {
			t.Fatalf("opted hook attach mismatch: %+v", next)
		}
		assertClassifiedReply(t, next, firstID, "hook", "absent", true)
	})

	t.Run("standalone touch and execution-addressed reconcile never unlock hookless child", func(t *testing.T) {
		s, clock, _ := testStore(t)
		parent := call(t, s, "sessions", Request{
			Client: "codex", Host: "host", SessionID: "S-touch-guard",
			Cwd: "/repo/w", Worktree: "/repo/w", Branch: "main", Source: "hook",
		})
		parentID := parent["execution_id"].(string)
		clock.advance(16 * time.Minute)
		call(t, s, "expire", Request{})

		child := call(t, s, "succeed", Request{
			Client: "codex", Host: "host", SessionID: "S-touch-guard", Source: "shim",
			PriorGeneration: 1, PriorExecutionID: parentID,
		})
		childID := child["execution_id"].(string)
		childRef := Request{ExecutionID: childID, Generation: 2}

		call(t, s, "touch", touchRequest(childRef, "tc_touch", "a.go"))
		call(t, s, "touch_files", filesTouch(childRef, "tc_files", ClaimGroup{
			Worktree: "/repo/w", Paths: []string{"b.go"},
		}))
		call(t, s, "reconcile", full(childRef, "before", map[string]string{}))
		call(t, s, "reconcile_scoped", reduced(childRef, "before", map[string]string{}, "lib"))

		if seen := readHookSeenMS(t, s, childID); seen != 0 {
			t.Fatalf("hook_seen_ms=%d after touch/reconcile, want 0", seen)
		}
		expectError(t, s, "bind", Request{Client: "codex", Host: "host", Execution: childID}, "lost")
		expectError(t, s, "heartbeat", Request{ExecutionID: childID, Generation: 2, Source: "watch"}, "lost")

		// Expiring the still-hookless child does NOT make it eligible for another succession hop.
		clock.advance(2 * time.Minute)
		call(t, s, "expire", Request{})
		out, err := s.Call(t.Context(), "succeed", Request{
			Client: "codex", Host: "host", SessionID: "S-touch-guard", Source: "shim",
			PriorGeneration: 2, PriorExecutionID: childID,
		})
		if out != nil {
			t.Fatalf("succeed accepted hookless child unlocked by touch/reconcile: %+v", out)
		}
		assertRegistrationLost(t, err, 2)

		// Contrast: session-addressed shell reconcile refuses source="mcp", while source="hook"
		// registers the hookless child cleanly and sets hook_seen_ms.
		s2, clock2, _ := testStore(t)
		p2 := call(t, s2, "sessions", Request{
			Client: "codex", Host: "host", SessionID: "S-shell-hook",
			Cwd: "/repo/w", Worktree: "/repo/w", Branch: "main", Source: "hook",
		})
		clock2.advance(16 * time.Minute)
		call(t, s2, "expire", Request{})
		c2 := call(t, s2, "succeed", Request{
			Client: "codex", Host: "host", SessionID: "S-shell-hook", Source: "shim",
			PriorGeneration: 1, PriorExecutionID: p2["execution_id"].(string),
		})
		c2ID := c2["execution_id"].(string)

		nonHookShell := full(Request{Client: "codex", Host: "host", SessionID: "S-shell-hook", Cwd: "/repo/w", Worktree: "/repo/w", Source: "mcp"}, "before", map[string]string{})
		if out, err := s2.Call(t.Context(), "reconcile", nonHookShell); out != nil {
			t.Fatalf("non-hook session reconcile attached: %+v", out)
		} else {
			assertRegistrationLost(t, err, 2)
		}
		if readHookSeenMS(t, s2, c2ID) != 0 {
			t.Fatal("non-hook shell reconcile set hook_seen_ms")
		}

		hookShell := nonHookShell
		hookShell.Source = "hook"
		shellOut := call(t, s2, "reconcile", hookShell)
		sess := shellOut["session"].(map[string]any)
		if sess["execution_id"] != c2ID || readHookSeenMS(t, s2, c2ID) == 0 {
			t.Fatalf("hook session reconcile failed to unlock child: out=%+v hook_seen=%d", shellOut, readHookSeenMS(t, s2, c2ID))
		}
	})
}

func TestMigratedOrdinaryNullProvenanceTouchAndReconcileSetHookSeen(t *testing.T) {
	legacyDB := filepath.Join(t.TempDir(), "ordinary-mig.db")
	clock := &fakeClock{wall: time.Unix(1800000000, 0)}
	seed, err := Open(legacyDB, clock)
	if err != nil {
		t.Fatal(err)
	}

	type target struct {
		name string
		sid  string
		id   string
	}
	targets := []target{
		{name: "touch", sid: "S-mig-touch"},
		{name: "touch_files", sid: "S-mig-touch-files"},
		{name: "reconcile", sid: "S-mig-reconcile"},
		{name: "reconcile_scoped", sid: "S-mig-reconcile-scoped"},
	}
	for i := range targets {
		res := call(t, seed, "sessions", Request{
			Client: "codex", Host: "host", SessionID: targets[i].sid,
			Cwd: "/repo/w", Worktree: "/repo/w", Branch: "main", Source: "mcp",
		})
		targets[i].id = res["execution_id"].(string)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}

	raw, err := sql.Open("sqlite", legacyDB)
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

	upgraded, err := Open(legacyDB, clock)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = upgraded.Close() }()

	for _, tc := range targets {
		t.Run(tc.name, func(t *testing.T) {
			var prior, src, proof sql.NullString
			var seen int64
			if err := upgraded.db.QueryRow("SELECT prior_execution_id,creation_source,creation_proof,hook_seen_ms FROM Execution WHERE id=?", tc.id).Scan(&prior, &src, &proof, &seen); err != nil {
				t.Fatal(err)
			}
			if prior.Valid || src.Valid || proof.Valid || seen != 0 {
				t.Fatalf("unexpected pre-op state: prior=%+v src=%+v proof=%+v seen=%d", prior, src, proof, seen)
			}

			// Resume from recovery first using legacy un-opted heartbeat.
			rec := call(t, upgraded, "heartbeat", Request{ExecutionID: tc.id, Generation: 1, Source: "shim"})
			assertLegacyHeartbeatReply(t, rec)

			ref := Request{ExecutionID: tc.id, Generation: 1}
			clock.advance(5 * time.Second)
			switch tc.name {
			case "touch":
				call(t, upgraded, "touch", touchRequest(ref, "tc_mig", "a.go"))
			case "touch_files":
				call(t, upgraded, "touch_files", filesTouch(ref, "tc_mig_files", ClaimGroup{
					Worktree: "/repo/w", Paths: []string{"b.go"},
				}))
			case "reconcile":
				call(t, upgraded, "reconcile", full(ref, "before", map[string]string{}))
			case "reconcile_scoped":
				call(t, upgraded, "reconcile_scoped", reduced(ref, "before", map[string]string{}, "lib"))
			}

			if after := readHookSeenMS(t, upgraded, tc.id); after == 0 {
				t.Fatalf("%s failed to set hook_seen_ms on migrated NULL-provenance row", tc.name)
			}
			unoptedBind := call(t, upgraded, "bind", Request{Client: "codex", Host: "host", Execution: tc.id})
			assertLegacyReply(t, unoptedBind, true)
			b := call(t, upgraded, "bind", Request{Client: "codex", Host: "host", Execution: tc.id, SuccessionProof: true})
			assertClassifiedReply(t, b, nil, nil, nil, true)
		})
	}
}

func TestShellSessionSuccessionProofEmitsBoundAndPreservesHookSeen(t *testing.T) {
	for _, tc := range []struct {
		name string
		op   string
	}{
		{name: "reconcile", op: "reconcile"},
		{name: "reconcile_scoped", op: "reconcile_scoped"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, clock, _ := testStore(t)
			parent := call(t, s, "sessions", Request{
				Client: "claude", Host: "host", Cwd: "/repo/w", Worktree: "/repo/w",
				RemoteURL: "https://example.com/team/project.git", RootCommit: "root",
				SessionID: "S-comb", Source: "hook", SuccessionProof: true,
			})
			parentID := parent["execution_id"].(string)
			clock.advance(16 * time.Minute)
			call(t, s, "expire", Request{})

			child := call(t, s, "succeed", Request{
				Client: "claude", Host: "host", SessionID: "S-comb", Source: "shim",
				PriorGeneration: 1, PriorExecutionID: parentID, SuccessionProof: true,
			})
			childID := child["execution_id"].(string)

			req := Request{
				Client: "claude", Host: "host", Cwd: "/repo/w", Worktree: "/repo/w",
				RemoteURL: "https://example.com/team/project.git", RootCommit: "root",
				SessionID: "S-comb", Source: "hook", Kind: "before",
				Status: map[string]string{}, SuccessionProof: true,
			}
			if tc.op == "reconcile_scoped" {
				req.Scope = &ShellScope{Mode: "dirty", Submodules: []string{"sub"}}
			}
			clock.advance(time.Second)
			out := call(t, s, tc.op, req)
			sess := out["session"].(map[string]any)
			assertClassifiedReply(t, sess, parentID, "shim", "shim_succession", true)
			if sess["execution_id"] != childID || sess["session_id"] != "S-comb" || sess["worktree"] != "/repo/w" || sess["outside_git"] != nil {
				t.Fatalf("unexpected shell session bound fields: %+v", sess)
			}
			caps, _ := sess["capabilities"].([]string)
			if !slices.Equal(caps, []string{"shell_scope", "file_claims", "succession_proof"}) {
				t.Fatalf("capabilities=%#v, want [shell_scope file_claims succession_proof]", sess["capabilities"])
			}
			if seen := readHookSeenMS(t, s, childID); seen == 0 {
				t.Fatalf("expected hook_seen_ms > 0 for %s", childID)
			}

			// Un-opted sibling session stays legacy-shaped without classification keys.
			unopted := req
			unopted.SessionID = "S-unopted"
			unopted.Cwd, unopted.Worktree = "/repo/w2", "/repo/w2"
			unopted.SuccessionProof = false
			unoptedOut := call(t, s, tc.op, unopted)
			assertLegacyReply(t, unoptedOut["session"].(map[string]any), false)
		})
	}
}
