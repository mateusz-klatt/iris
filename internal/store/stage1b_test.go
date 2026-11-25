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

	// 1. Initial hook registration in git repo and outside git.
	hookReg := call(t, s, "sessions", Request{
		Client: "claude", Host: "host", SessionID: "S-cls",
		Cwd: "/repo/w", Worktree: "/repo/w", Branch: "main", Source: "hook",
		ClientPID: 501, ClientPIDStartMS: 9001,
	})
	hookID := hookReg["execution_id"].(string)
	assertClassifiedReply(t, hookReg, nil, "hook", "initial", true)

	outsideReg := call(t, s, "sessions_outside", Request{
		Client: "codex", Host: "host", SessionID: "S-out",
		Cwd: "/tmp/outside", Source: "hook",
	})
	assertClassifiedReply(t, outsideReg, nil, "hook", "initial", true)

	// 2. All four bind modes carry classified candidate fields.
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
			b := call(t, s, "bind", tc.req)
			assertClassifiedReply(t, b, nil, "hook", "initial", true)
		})
	}
	if b := call(t, s, "bind", Request{Client: "codex", Host: "host", Execution: singleID}); b["execution_id"] != singleID {
		t.Fatalf("unexpected handle bind: %+v", b)
	}

	// 3. Live and recovering heartbeats carry classified candidate fields.
	hbLive := call(t, s, "heartbeat", Request{ExecutionID: hookID, Generation: 1, Source: "shim"})
	assertClassifiedReply(t, hbLive, nil, "hook", "initial", true)

	s = restart(t, s, dbPath, clock)
	hbRec := call(t, s, "heartbeat", Request{ExecutionID: hookID, Generation: 1, Source: "shim"})
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

	// On first open after restart, Execution enters recovery; heartbeat resumes with nil provenance.
	legRecHB := call(t, upgraded, "heartbeat", Request{ExecutionID: legID, Generation: 1, Source: "shim"})
	assertClassifiedReply(t, legRecHB, nil, nil, nil, true)

	legLiveHB := call(t, upgraded, "heartbeat", Request{ExecutionID: legID, Generation: 1, Source: "watch"})
	assertClassifiedReply(t, legLiveHB, nil, nil, nil, true)

	for _, req := range []Request{
		{Client: "codex", Host: "host", Execution: legID},
		{Client: "codex", Host: "host", SessionID: "S-leg"},
		{Client: "codex", Host: "host", ClientPID: 701, ClientPIDStartMS: 9701},
		{Client: "codex", Host: "host", SessionID: "S-leg", ClientPID: 701, ClientPIDStartMS: 9701},
	} {
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
			ClientPID: 101, ClientPIDStartMS: 1001,
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
			b := call(t, s, "bind", req)
			assertClassifiedReply(t, b, parentID, "shim", "shim_succession", true)
		}
		hbWatch := call(t, s, "heartbeat", Request{ExecutionID: childID, Generation: 2, Source: "watch"})
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
		})
		if hookNext["generation"] != int64(3) || hookNext["execution_id"] == childID {
			t.Fatalf("expected hook G=3 after lost hookless child: %+v", hookNext)
		}
		assertClassifiedReply(t, hookNext, childID, "hook", "absent", true)
	})

	t.Run("choice A stays deferred for ordinary lost hook session", func(t *testing.T) {
		s, clock, _ := testStore(t)
		first := call(t, s, "sessions", Request{
			Client: "codex", Host: "host", SessionID: "S-choicea",
			Cwd: "/repo/w", Worktree: "/repo/w", Branch: "main", Source: "hook",
		})
		firstID := first["execution_id"].(string)
		clock.advance(16 * time.Minute)
		call(t, s, "expire", Request{})

		next := call(t, s, "sessions", Request{
			Client: "codex", Host: "host", SessionID: "S-choicea",
			Cwd: "/repo/w", Worktree: "/repo/w", Branch: "main", Source: "mcp",
		})
		if next["generation"] != int64(2) || next["execution_id"] == firstID {
			t.Fatalf("Choice A altered ordinary v0.1.15 non-hook G=0 behavior: %+v", next)
		}
		assertClassifiedReply(t, next, firstID, "mcp", "absent", false)
	})
}
