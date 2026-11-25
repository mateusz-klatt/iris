package store

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// A generation-zero registration from iris mcp or iris watch carries no retry_generation.
// After the hook session has ended, that request must not open generation G+1. The hub reports
// lost with highest_generation, the same floor a retried hook registration uses.

func TestFloorBypassHubRefusesProofLessMCPAndWatch(t *testing.T) {
	for _, source := range []string{"mcp", "watch"} {
		t.Run(source+"/ended", func(t *testing.T) {
			s, _, _ := testStore(t)
			registration := Request{Client: "claude", Host: "host", SessionID: "S", Cwd: "/repo/w", Worktree: "/repo/w", Source: "hook"}
			first := call(t, s, "sessions", registration)
			id := first["execution_id"].(string)
			call(t, s, "end", Request{ExecutionID: id, Generation: 1})
			var state string
			var generation int64
			if err := s.db.QueryRow("SELECT state, generation FROM Execution WHERE id=?", id).Scan(&state, &generation); err != nil {
				t.Fatal(err)
			}
			if state != "ended" || generation != 1 {
				t.Fatalf("setup left state %s generation %d", state, generation)
			}
			before := databaseState(t, s)
			registration.Source = source
			registration.Generation = 0
			registration.RetryGeneration = 0
			out, err := s.Call(t.Context(), "sessions", registration)
			if err == nil {
				t.Fatalf("%s after end created %+v", source, out)
			}
			assertRegistrationLost(t, err, 1)
			if out != nil || databaseState(t, s) != before {
				t.Fatalf("%s after end changed retained history: %v", source, out)
			}
		})
	}
}

func TestChoiceARefusesNonHookGenerationZeroOverHookHistory(t *testing.T) {
	registerOps := func(client, host, session, source string, retryGen int64) []struct {
		op  string
		req Request
	} {
		return []struct {
			op  string
			req Request
		}{
			{"sessions", Request{Client: client, Host: host, SessionID: session, Cwd: "/repo/w", Worktree: "/repo/w", Source: source, RetryGeneration: retryGen}},
			{"sessions_outside", Request{Client: client, Host: host, SessionID: session, Cwd: "/repo/w", Source: source, RetryGeneration: retryGen}},
			{"reconcile", full(Request{Client: client, Host: host, SessionID: session, Cwd: "/repo/w", Worktree: "/repo/w", Source: source, RetryGeneration: retryGen}, "before", map[string]string{})},
			{"reconcile_scoped", reduced(Request{Client: client, Host: host, SessionID: session, Cwd: "/repo/w", Worktree: "/repo/w", Source: source, RetryGeneration: retryGen}, "before", map[string]string{}, "sub")},
		}
	}

	for _, state := range []string{"active", "lost", "ended"} {
		for _, source := range []string{"mcp", "shim", "watch", ""} {
			for _, retryGen := range []int64{0, 1} {
				name := fmt.Sprintf("%s/%q/retry=%d", state, source, retryGen)
				t.Run(name, func(t *testing.T) {
					s, clock, _ := testStore(t)
					first := call(t, s, "sessions", Request{
						Client: "claude", Host: "host", SessionID: "S",
						Cwd: "/repo/w", Worktree: "/repo/w", Source: "hook",
					})
					id := first["execution_id"].(string)
					switch state {
					case "lost":
						clock.advance(15*time.Minute + time.Millisecond)
						call(t, s, "expire", Request{})
					case "ended":
						call(t, s, "end", Request{ExecutionID: id, Generation: 1})
					}
					before := databaseState(t, s)
					for _, tc := range registerOps("claude", "host", "S", source, retryGen) {
						out, err := s.Call(t.Context(), tc.op, tc.req)
						if out != nil {
							t.Fatalf("%s(%q) over %s hook history returned authority: %+v", tc.op, source, state, out)
						}
						assertRegistrationLost(t, err, 1)
					}
					if databaseState(t, s) != before {
						t.Fatalf("%s(%q) over %s hook history changed retained DB state", name, source, state)
					}
				})
			}
		}
	}

	// Genuine hook G=0 registration over lost/ended hook history continues to advance to G=2.
	for _, state := range []string{"lost", "ended"} {
		t.Run("hook_advances/"+state, func(t *testing.T) {
			s, clock, _ := testStore(t)
			hookReq := Request{Client: "codex", Host: "host", SessionID: "S-hook", Cwd: "/repo/w", Worktree: "/repo/w", Source: "hook"}
			first := call(t, s, "sessions", hookReq)
			if state == "lost" {
				clock.advance(16 * time.Minute)
				call(t, s, "expire", Request{})
			} else {
				call(t, s, "end", Request{ExecutionID: first["execution_id"].(string), Generation: 1})
			}
			next := call(t, s, "sessions", hookReq)
			if next["generation"] != int64(2) || next["execution_id"] == first["execution_id"] {
				t.Fatalf("hook after %s failed to advance: %+v", state, next)
			}
		})
	}
}

func TestChoiceAUpgradedHistoryOverNonHookGenerationPlusOne(t *testing.T) {
	for _, tc := range []struct {
		name           string
		gState         string
		gPlusOneState  string
		legacyNullProv bool
	}{
		{"hook_lost_then_mcp_live_attach", "lost", "active", false},
		{"hook_ended_then_mcp_live_attach", "ended", "active", false},
		{"hook_lost_then_mcp_lost_insert", "lost", "lost", false},
		{"hook_ended_then_mcp_lost_insert", "ended", "lost", false},
		{"hook_lost_then_pre029_null_live_attach", "lost", "active", true},
		{"hook_lost_then_pre029_null_lost_insert", "lost", "lost", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, clock, _ := testStore(t)
			first := call(t, s, "sessions", Request{
				Client: "codex", Host: "host", SessionID: "S-upg",
				Cwd: "/repo/w", Worktree: "/repo/w", Branch: "main", Source: "hook",
			})
			firstID := first["execution_id"].(string)
			agentID := first["agent"].(map[string]any)["id"].(string)
			repoID := first["repository_id"].(string)
			if tc.gState == "ended" {
				call(t, s, "end", Request{ExecutionID: firstID, Generation: 1})
			} else {
				clock.advance(16 * time.Minute)
				call(t, s, "expire", Request{})
			}

			// Seed G+1 = 2 as an ordinary pre-upgrade unhooked MCP / legacy row (hook_seen_ms = 0).
			g2ID := id("ex_")
			tick := clock.Tick()
			var prior, src, proof any = firstID, "mcp", "absent"
			if tc.legacyNullProv {
				prior, src, proof = nil, nil, nil
			}
			if _, err := s.db.Exec(
				`INSERT INTO Execution(id,agent_id,repository_id,host_id,client,session_id,generation,worktree,branch,boot_epoch,deadline_tick_ms,lease_renewed_tick_ms,state,client_pid,client_pid_start_ms,single_session,heartbeat_mode,hook_seen_ms,prior_execution_id,creation_source,creation_proof) VALUES(?,?,?,?,?,?,2,'/repo/w','main',?,?,?, ?,0,0,0,1,0,?,?,?)`,
				g2ID, agentID, repoID, "host", "codex", "S-upg", s.epoch, tick+60000, tick, tc.gPlusOneState, prior, src, proof,
			); err != nil {
				t.Fatal(err)
			}

			before := databaseState(t, s)
			for _, source := range []string{"mcp", "shim", "watch", ""} {
				for _, callSpec := range []struct {
					op  string
					req Request
				}{
					{"sessions", Request{Client: "codex", Host: "host", SessionID: "S-upg", Cwd: "/repo/w", Worktree: "/repo/w", Source: source}},
					{"sessions_outside", Request{Client: "codex", Host: "host", SessionID: "S-upg", Cwd: "/repo/w", Source: source}},
					{"reconcile", full(Request{Client: "codex", Host: "host", SessionID: "S-upg", Cwd: "/repo/w", Worktree: "/repo/w", Source: source}, "before", map[string]string{})},
					{"reconcile_scoped", reduced(Request{Client: "codex", Host: "host", SessionID: "S-upg", Cwd: "/repo/w", Worktree: "/repo/w", Source: source}, "before", map[string]string{}, "sub")},
				} {
					out, err := s.Call(t.Context(), callSpec.op, callSpec.req)
					if out != nil {
						t.Fatalf("%s(%q) bypassed Choice A via unhooked G+1: %+v", callSpec.op, source, out)
					}
					assertRegistrationLost(t, err, 2)
				}
			}
			if databaseState(t, s) != before {
				t.Fatal("refused Choice A over upgraded G/G+1 history mutated DB")
			}

			// A genuine hook still attaches to live G+1 or creates G+2 over lost G+1.
			hookOut := call(t, s, "sessions", Request{
				Client: "codex", Host: "host", SessionID: "S-upg",
				Cwd: "/repo/w", Worktree: "/repo/w", Branch: "main", Source: "hook",
				SuccessionProof: true,
			})
			if tc.gPlusOneState == "active" {
				if hookOut["execution_id"] != g2ID || hookOut["generation"] != int64(2) || hookOut["hook_seen"] != true {
					t.Fatalf("hook did not attach to live unhooked G+1: %+v", hookOut)
				}
			} else if hookOut["execution_id"] == g2ID || hookOut["generation"] != int64(3) || hookOut["hook_seen"] != true {
				t.Fatalf("hook did not create G+2 over lost unhooked G+1: %+v", hookOut)
			}
		})
	}
}

func TestChoiceAProcessHistoryAndHighestGenerationPinning(t *testing.T) {
	t.Run("synthetic mcp-<pid>-<start> refused across active lost and ended hook process history", func(t *testing.T) {
		for _, tc := range []struct {
			client string
			single bool
		}{
			{"cursor", true},
			{"antigravity", true},
			{"kimi", true},
			{"codex", true},
			{"claude", false},
		} {
			for _, state := range []string{"active", "lost", "ended"} {
				t.Run(fmt.Sprintf("%s/%s", tc.client, state), func(t *testing.T) {
					s, clock, _ := testStore(t)
					const pid, start = int64(420), int64(10420)
					hookReq := Request{
						Client: tc.client, Host: "host", SessionID: "real-sess",
						Cwd: "/repo/w", Worktree: "/repo/w", Source: "hook",
						ClientPID: pid, ClientPIDStartMS: start, SingleSession: tc.single,
					}
					first := call(t, s, "sessions", hookReq)
					// Advance to G=2 for lost/ended to pin that synthetic highest_generation stays 0!
					switch state {
					case "lost":
						clock.advance(16 * time.Minute)
						call(t, s, "expire", Request{})
						second := call(t, s, "sessions", hookReq)
						clock.advance(16 * time.Minute)
						call(t, s, "expire", Request{})
						_ = second
					case "ended":
						call(t, s, "end", Request{ExecutionID: first["execution_id"].(string), Generation: 1})
						second := call(t, s, "sessions", hookReq)
						call(t, s, "end", Request{ExecutionID: second["execution_id"].(string), Generation: 2})
					}

					before := databaseState(t, s)
					synthID := fmt.Sprintf("mcp-%d-%d", pid, start)
					for _, source := range []string{"mcp", "shim", "watch", ""} {
						for _, callSpec := range []struct {
							op  string
							req Request
						}{
							{"sessions", Request{Client: tc.client, Host: "host", SessionID: synthID, Cwd: "/repo/w", Worktree: "/repo/w", Source: source, ClientPID: pid, ClientPIDStartMS: start, SingleSession: tc.single}},
							{"sessions_outside", Request{Client: tc.client, Host: "host", SessionID: synthID, Cwd: "/repo/w", Source: source, ClientPID: pid, ClientPIDStartMS: start, SingleSession: tc.single}},
							{"reconcile", full(Request{Client: tc.client, Host: "host", SessionID: synthID, Cwd: "/repo/w", Worktree: "/repo/w", Source: source, ClientPID: pid, ClientPIDStartMS: start, SingleSession: tc.single}, "before", map[string]string{})},
							{"reconcile_scoped", reduced(Request{Client: tc.client, Host: "host", SessionID: synthID, Cwd: "/repo/w", Worktree: "/repo/w", Source: source, ClientPID: pid, ClientPIDStartMS: start, SingleSession: tc.single}, "before", map[string]string{}, "sub")},
						} {
							out, err := s.Call(t.Context(), callSpec.op, callSpec.req)
							if out != nil {
								t.Fatalf("%s(%q) bypassed process Choice A: %+v", callSpec.op, source, out)
							}
							assertRegistrationLost(t, err, 0)
						}
					}
					if databaseState(t, s) != before {
						t.Fatal("refused process Choice A registration mutated DB")
					}
				})
			}
		}
	})

	t.Run("synthetic attach and insert pin own highest_generation after hook registers", func(t *testing.T) {
		s, clock, _ := testStore(t)
		const pid, start = int64(515), int64(9515)
		synthReq := Request{
			Client: "claude", Host: "host", SessionID: "mcp-515-9515",
			Cwd: "/repo/w", Worktree: "/repo/w", Source: "mcp",
			ClientPID: pid, ClientPIDStartMS: start,
		}
		// Genuine initial D6 fallback before any hook reaches the hub.
		synth := call(t, s, "sessions", synthReq)
		if synth["generation"] != int64(1) {
			t.Fatalf("unexpected synthetic fallback: %+v", synth)
		}

		// Now hook runs on the same Claude process through G=3.
		hookReq := Request{
			Client: "claude", Host: "host", SessionID: "native-s",
			Cwd: "/repo/w", Worktree: "/repo/w", Source: "hook",
			ClientPID: pid, ClientPIDStartMS: start,
		}
		for g := 1; g <= 3; g++ {
			ex := call(t, s, "sessions", hookReq)
			if g < 3 {
				call(t, s, "end", Request{ExecutionID: ex["execution_id"].(string), Generation: int64(g)})
			}
		}

		// Re-attaching the live unhooked synthetic session with non-hook G=0 is refused via process
		// Choice A and returns highest_generation=1 (its own session scope, NOT native-s's 3).
		beforeAttach := databaseState(t, s)
		if out, err := s.Call(t.Context(), "sessions", synthReq); out != nil {
			t.Fatalf("synthetic attach bypassed process Choice A: %+v", out)
		} else {
			assertRegistrationLost(t, err, 1)
		}
		if databaseState(t, s) != beforeAttach {
			t.Fatal("refused synthetic attach mutated DB")
		}

		// Expiring the synthetic session tests the insert path too, still pinning highest_generation=1.
		clock.advance(2 * time.Minute)
		call(t, s, "expire", Request{})
		beforeInsert := databaseState(t, s)
		if out, err := s.Call(t.Context(), "sessions", synthReq); out != nil {
			t.Fatalf("synthetic insert bypassed process Choice A: %+v", out)
		} else {
			assertRegistrationLost(t, err, 1)
		}
		if databaseState(t, s) != beforeInsert {
			t.Fatal("refused synthetic insert mutated DB")
		}
	})

	t.Run("process isolation D6 fallback handover and D9 shim reattachment preserved", func(t *testing.T) {
		s, clock, file := testStore(t)
		const pid, start = int64(700), int64(7700)
		call(t, s, "sessions", Request{
			Client: "cursor", Host: "host", SessionID: "hook-700",
			Cwd: "/repo/w", Worktree: "/repo/w", Source: "hook",
			ClientPID: pid, ClientPIDStartMS: start, SingleSession: true,
		})

		// Different PID, reused PID with different start_ms, different client, different host,
		// multiplexed (single_session=false), and a distinct native Claude Rung-2 session ID
		// with single_session=false after another session ended all remain allowed for D6 bootstrap.
		claudeEnded := call(t, s, "sessions", Request{
			Client: "claude", Host: "host", SessionID: "claude-ended-s1",
			Cwd: "/repo/w", Worktree: "/repo/w", Source: "hook",
			ClientPID: 800, ClientPIDStartMS: 8800, SingleSession: false,
		})
		call(t, s, "end", Request{ExecutionID: claudeEnded["execution_id"].(string), Generation: 1})
		for _, tc := range []struct {
			name string
			req  Request
		}{
			{"different pid", Request{Client: "cursor", Host: "host", SessionID: "mcp-701-7700", Cwd: "/repo/w", Worktree: "/repo/w", Source: "mcp", ClientPID: pid + 1, ClientPIDStartMS: start, SingleSession: true}},
			{"same pid different start_ms", Request{Client: "cursor", Host: "host", SessionID: "mcp-700-7701", Cwd: "/repo/w", Worktree: "/repo/w", Source: "mcp", ClientPID: pid, ClientPIDStartMS: start + 1, SingleSession: true}},
			{"different client", Request{Client: "kimi", Host: "host", SessionID: "mcp-kimi", Cwd: "/repo/w", Worktree: "/repo/w", Source: "mcp", ClientPID: pid, ClientPIDStartMS: start, SingleSession: true}},
			{"different host", Request{Client: "cursor", Host: "other-host", SessionID: "mcp-other-host", Cwd: "/repo/w", Worktree: "/repo/w", Source: "mcp", ClientPID: pid, ClientPIDStartMS: start, SingleSession: true}},
			{"multiplexed codex", Request{Client: "codex", Host: "host", SessionID: "codex-multi", Cwd: "/repo/w", Worktree: "/repo/w", Source: "mcp", ClientPID: pid, ClientPIDStartMS: start, SingleSession: false}},
			{"distinct native claude rung 2 after ended", Request{Client: "claude", Host: "host", SessionID: "claude-fresh-s2", Cwd: "/repo/w", Worktree: "/repo/w", Source: "mcp", ClientPID: 800, ClientPIDStartMS: 8800, SingleSession: false}},
		} {
			got := call(t, s, "sessions", tc.req)
			if got["generation"] != int64(1) {
				t.Fatalf("%s unexpected generation: %+v", tc.name, got)
			}
		}
		// While synthetic mcp-<pid>-<start> and SingleSession=true on that Claude process remain refused.
		for _, refused := range []Request{
			{Client: "claude", Host: "host", SessionID: "mcp-800-8800", Cwd: "/repo/w", Worktree: "/repo/w", Source: "mcp", ClientPID: 800, ClientPIDStartMS: 8800, SingleSession: false},
			{Client: "claude", Host: "host", SessionID: "claude-r3-s", Cwd: "/repo/w", Worktree: "/repo/w", Source: "mcp", ClientPID: 800, ClientPIDStartMS: 8800, SingleSession: true},
		} {
			if out, err := s.Call(t.Context(), "sessions", refused); out != nil {
				t.Fatalf("expected process Choice A refusal for %+v, got %+v", refused, out)
			} else {
				assertRegistrationLost(t, err, 0)
			}
		}

		// Genuine hookless D6 fallback attaches, advances on expiry when still unhooked, and hands over mail.
		noHookReq := Request{
			Client: "cursor", Host: "host", SessionID: "mcp-999-999",
			Cwd: "/repo/w", Worktree: "/repo/w", RemoteURL: "https://example.com/team/project.git", RootCommit: "root",
			Source: "mcp", ClientPID: 999, ClientPIDStartMS: 999, SingleSession: true,
		}
		g1 := call(t, s, "sessions", noHookReq)
		g1Attach := call(t, s, "sessions", noHookReq)
		if g1Attach["execution_id"] != g1["execution_id"] {
			t.Fatalf("no-hook attach created second execution: %+v vs %+v", g1, g1Attach)
		}
		clock.advance(2 * time.Minute)
		call(t, s, "expire", Request{})
		g2 := call(t, s, "sessions", noHookReq)
		if g2["generation"] != int64(2) || g2["execution_id"] == g1["execution_id"] {
			t.Fatalf("no-hook replacement after expiry failed: %+v", g2)
		}
		sender, _ := session(t, s, 88)
		call(t, s, "messages", Request{
			ExecutionID: sender.ExecutionID, Generation: sender.Generation,
			Key: "handover-msg", To: []string{g2["agent"].(map[string]any)["name"].(string)}, Body: "deliver me",
		})
		lateHook := call(t, s, "sessions", Request{
			Client: "cursor", Host: "host", SessionID: "native-999",
			Cwd: "/repo/w", Worktree: "/repo/w", RemoteURL: "https://example.com/team/project.git", RootCommit: "root",
			Source: "hook", ClientPID: 999, ClientPIDStartMS: 999, SingleSession: true,
		})
		call(t, s, "end", Request{ExecutionID: g2["execution_id"].(string), Generation: 2, HandOver: lateHook["execution_id"].(string)})
		inbox := call(t, s, "inbox", Request{ExecutionID: lateHook["execution_id"].(string), Generation: 1})["deliveries"].([]map[string]any)
		if len(inbox) != 1 {
			t.Fatalf("D6 handover lost mail: %+v", inbox)
		}

		// Positive-generation source="shim" D9 reattachment after hub restart remains intact (Rung 2 and Rung 3).
		r2 := call(t, s, "sessions", Request{Client: "claude", Host: "host", SessionID: "r2-d9", Cwd: "/repo/w", Worktree: "/repo/w", Source: "hook"})
		s = restart(t, s, file, clock)
		r2Back := call(t, s, "sessions", Request{Client: "claude", Host: "host", SessionID: "r2-d9", Cwd: "/repo/w", Generation: 1, Source: "shim"})
		if r2Back["execution_id"] != r2["execution_id"] {
			t.Fatalf("Rung 2 D9 reattach failed: %+v", r2Back)
		}
		r3Back := call(t, s, "sessions", Request{Client: "cursor", Host: "host", SessionID: "native-999", Cwd: "/repo/w", Generation: 1, Source: "shim", ClientPID: 999, ClientPIDStartMS: 999})
		if r3Back["execution_id"] != lateHook["execution_id"] {
			t.Fatalf("Rung 3 D9 reattach failed: %+v", r3Back)
		}
	})
}

func TestChoiceAStorageFaultsRollBackWithoutDomainRefusal(t *testing.T) {
	s, _, file := testStore(t)
	req := Request{
		Client: "cursor", Host: "host", SessionID: "mcp-fault-77",
		Cwd: "/repo/w", Worktree: "/repo/w", Source: "mcp",
		ClientPID: 77, ClientPIDStartMS: 770, SingleSession: true,
	}
	fault := installFaultDriver(t, s, file)
	fault.discover = true
	if _, err := s.Call(t.Context(), "sessions", req); !errors.Is(err, errStorageFault) {
		t.Fatalf("discover failure=%v, want errStorageFault", err)
	}
	trace := append([]string(nil), fault.trace...)
	fault.discover = false
	var choiceASteps []int
	for i, q := range trace {
		if strings.Contains(q, "hook_seen_ms>0") {
			choiceASteps = append(choiceASteps, i+1)
		}
	}
	if len(choiceASteps) != 2 {
		t.Fatalf("expected 2 Choice A EXISTS queries in trace, got %v (%v)", choiceASteps, trace)
	}
	before := databaseState(t, s)
	for _, step := range choiceASteps {
		fault.count, fault.at = 0, step
		out, err := s.Call(t.Context(), "sessions", req)
		fault.at = 0
		if !errors.Is(err, errStorageFault) || out != nil || databaseState(t, s) != before {
			t.Fatalf("Choice A fault at step %d exposed domain error or state: out=%+v err=%v", step, out, err)
		}
	}
}

func TestEndedSessionWithoutHookHistoryRefusesNonHookGenerationZero(t *testing.T) {
	s, _, _ := testStore(t)
	registration := Request{Client: "claude", Host: "host", SessionID: "S", Cwd: "/repo/w", Worktree: "/repo/w", Source: "mcp"}
	first := call(t, s, "sessions", registration)
	id := first["execution_id"].(string)
	var hookSeen int64
	var state string
	if err := s.db.QueryRow("SELECT hook_seen_ms, state FROM Execution WHERE id=?", id).Scan(&hookSeen, &state); err != nil {
		t.Fatal(err)
	}
	if hookSeen != 0 || state != "active" {
		t.Fatalf("setup recorded a hook or was not active: hook_seen=%d state=%s", hookSeen, state)
	}
	call(t, s, "end", Request{ExecutionID: id, Generation: 1})
	if err := s.db.QueryRow("SELECT hook_seen_ms, state FROM Execution WHERE id=?", id).Scan(&hookSeen, &state); err != nil || hookSeen != 0 || state != "ended" {
		t.Fatalf("ended row hook_seen=%d state=%s err=%v", hookSeen, state, err)
	}
	for _, source := range []string{"mcp", "shim", "watch", ""} {
		registration.Source = source
		registration.Generation = 0
		registration.RetryGeneration = 0
		before := databaseState(t, s)
		out, err := s.Call(t.Context(), "sessions", registration)
		if out != nil {
			t.Fatalf("%q revived an ended session: %+v", source, out)
		}
		assertRegistrationLost(t, err, 1)
		var lost *Error
		if !errors.As(err, &lost) || lost.Message != "non-hook generation-zero registration cannot succeed an ended session" {
			t.Fatalf("%q refusal: %v", source, err)
		}
		if databaseState(t, s) != before {
			t.Fatalf("%q changed retained rows", source)
		}
	}
	registration.Source = "hook"
	out := call(t, s, "sessions", registration)
	if out["generation"] != int64(2) || out["execution_id"] == id {
		t.Fatalf("hook did not advance the ended session: %+v", out)
	}
	if err := s.db.QueryRow("SELECT state FROM Execution WHERE id=?", id).Scan(&state); err != nil || state != "ended" {
		t.Fatalf("hook changed the ended row: state=%s err=%v", state, err)
	}
}

func TestFloorBypassHookSessionStartAfterEndGetsNextGeneration(t *testing.T) {
	s, _, _ := testStore(t)
	registration := Request{Client: "claude", Host: "host", SessionID: "S", Cwd: "/repo/w", Worktree: "/repo/w", Source: "hook"}
	first := call(t, s, "sessions", registration)
	id := first["execution_id"].(string)
	call(t, s, "end", Request{ExecutionID: id, Generation: 1})
	out := call(t, s, "sessions", registration)
	if out["generation"] != int64(2) || out["execution_id"] == id {
		t.Fatalf("hook SessionStart after end did not get G+1: %+v", out)
	}
	var state string
	if err := s.db.QueryRow("SELECT state FROM Execution WHERE id=?", id).Scan(&state); err != nil || state != "ended" {
		t.Fatalf("hook SessionStart changed the ended row: state=%s err=%v", state, err)
	}
}

func TestFloorBypassHubRejectsNonHookRegistrationVariantsAfterEnd(t *testing.T) {
	for _, tc := range []struct {
		name, source string
		retry        int64
		code         string
	}{
		{"spoofed shim", "shim", 0, "lost"},
		{"legacy empty source", "", 0, "lost"},
		{"uppercase source", "MCP", 0, "bad_request"},
		{"whitespace source", "mcp ", 0, "bad_request"},
		{"unknown source", "foreign", 0, "bad_request"},
		{"negative retry", "mcp", -1, "bad_request"},
		{"mcp caller retry floor", "mcp", 1, "lost"},
		{"watch caller retry floor", "watch", 1, "lost"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, _ := testStore(t)
			registration := Request{Client: "claude", Host: "host", SessionID: "S", Cwd: "/repo/w", Worktree: "/repo/w", Source: "hook"}
			first := call(t, s, "sessions", registration)
			call(t, s, "end", Request{ExecutionID: first["execution_id"].(string), Generation: 1})
			registration.Source, registration.RetryGeneration = tc.source, tc.retry
			before := databaseState(t, s)
			out, err := s.Call(t.Context(), "sessions", registration)
			if out != nil {
				t.Fatalf("refused registration returned authority: %+v", out)
			}
			if tc.code == "lost" {
				assertRegistrationLost(t, err, 1)
			} else {
				var domain *Error
				if !errors.As(err, &domain) || domain.Code != tc.code {
					t.Fatalf("got %v, want %s", err, tc.code)
				}
			}
			if databaseState(t, s) != before {
				t.Fatalf("refused registration changed retained history")
			}
		})
	}
}

func TestFloorBypassConcurrentSessionEndAndRegistration(t *testing.T) {
	s, _, _ := testStore(t)
	registration := Request{Client: "claude", Host: "host", SessionID: "S", Cwd: "/repo/w", Worktree: "/repo/w", Source: "hook"}
	first := call(t, s, "sessions", registration)
	id := first["execution_id"].(string)
	registration.Source = "shim"
	start := make(chan struct{})
	registrationAtLock := make(chan struct{})
	releaseRegistration := make(chan struct{})
	type registrationResult struct {
		out map[string]any
		err error
	}
	registrations := make(chan registrationResult, 1)
	ends := make(chan error, 1)
	go func() {
		<-start
		ctx := withLockObserver(t.Context(), lockObserver{
			beforeLock: func() {
				close(registrationAtLock)
				<-releaseRegistration
			},
			afterUnlock: func(time.Duration, time.Duration) {},
		})
		out, err := s.Call(ctx, "sessions", registration)
		registrations <- registrationResult{out, err}
	}()
	go func() {
		<-start
		_, err := s.Call(t.Context(), "end", Request{ExecutionID: id, Generation: 1})
		ends <- err
	}()
	close(start)
	<-registrationAtLock
	endErr := <-ends
	close(releaseRegistration)
	result := <-registrations
	if endErr != nil {
		t.Fatalf("SessionEnd failed: %v", endErr)
	}
	if result.out != nil {
		t.Fatalf("registration after SessionEnd returned authority: %+v", result.out)
	}
	assertRegistrationLost(t, result.err, 1)
	var count, generation int
	var state string
	if err := s.db.QueryRow("SELECT COUNT(*),MIN(state),MAX(generation) FROM Execution WHERE host_id='host' AND client='claude' AND session_id='S'").Scan(&count, &state, &generation); err != nil {
		t.Fatal(err)
	}
	if count != 1 || state != "ended" || generation != 1 {
		t.Fatalf("concurrent end created a successor: count=%d state=%s generation=%d", count, state, generation)
	}
}

func TestFloorBypassHubRefusesExactGenerationAfterEnd(t *testing.T) {
	s, _, _ := testStore(t)
	registration := Request{Client: "claude", Host: "host", SessionID: "S", Cwd: "/repo/w", Worktree: "/repo/w", Source: "mcp"}
	first := call(t, s, "sessions", Request{Client: "claude", Host: "host", SessionID: "S", Cwd: "/repo/w", Worktree: "/repo/w", Source: "hook"})
	call(t, s, "end", Request{ExecutionID: first["execution_id"].(string), Generation: 1})
	before := databaseState(t, s)
	registration.Generation = 1
	out, err := s.Call(t.Context(), "sessions", registration)
	if err == nil {
		t.Fatalf("exact generation revived %+v", out)
	}
	assertRegistrationLost(t, err, 1)
	if out != nil || databaseState(t, s) != before {
		t.Fatalf("exact-generation refusal changed history: %v", out)
	}
}
