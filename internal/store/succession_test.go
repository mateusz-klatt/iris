package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func successionFixture(t *testing.T, s *Store, clock *fakeClock, registration Request) (Request, map[string]any) {
	t.Helper()
	registration.Source = "hook"
	previous := call(t, s, "sessions", registration)
	clock.advance(16 * time.Minute)
	call(t, s, "expire", Request{})
	return Request{
		Client: registration.Client, Host: registration.Host, SessionID: registration.SessionID, Source: "shim",
		PriorGeneration: previous["generation"].(int64), PriorExecutionID: previous["execution_id"].(string),
		ClientPID: registration.ClientPID, ClientPIDStartMS: registration.ClientPIDStartMS,
	}, previous
}

func refuseSuccession(t *testing.T, s *Store, req Request, highest int64) {
	t.Helper()
	before := databaseState(t, s)
	out, err := s.Call(t.Context(), "succeed", req)
	if out != nil {
		t.Fatalf("refused succession returned authority: %+v", out)
	}
	assertRegistrationLost(t, err, highest)
	if databaseState(t, s) != before {
		t.Fatal("refused succession changed Agent, Execution, Delivery or Claim state")
	}
}

func TestMessageRetryAcceptsOperationWrittenBeforeSuccessionProofFields(t *testing.T) {
	s, _, _ := testStore(t)
	exec, registered := session(t, s, 1)
	request := exec
	request.Key = "committed-before-upgrade"
	request.To = []string{registered["agent"].(map[string]any)["name"].(string)}
	request.Body = "already sent"
	raw, err := json.Marshal(struct {
		Route   string
		Request Request
	}{"message", request})
	if err != nil {
		t.Fatal(err)
	}
	proofFields := []byte(`,"prior_generation":0,"prior_execution_id":""`)
	newRaw := raw
	if !bytes.Contains(raw, proofFields) {
		newRaw = bytes.TrimSuffix(raw, []byte("}}"))
		newRaw = append(newRaw, proofFields...)
		newRaw = append(newRaw, '}', '}')
	}
	oldRaw := bytes.Replace(newRaw, proofFields, nil, 1)
	if bytes.Equal(oldRaw, newRaw) {
		t.Fatal("test request did not contain the new zero-valued proof fields")
	}
	hash := sha256.Sum256(oldRaw)
	agentID := registered["agent"].(map[string]any)["id"].(string)
	if _, err := s.db.Exec("INSERT INTO Operation(principal_agent_id,key,request_hash,result_json,created_ms) VALUES(?,?,?,?,1)", agentID, request.Key, hash[:], `{"message_id":"ms_committed","deliveries":[]}`); err != nil {
		t.Fatal(err)
	}
	replayed := call(t, s, "messages", request)
	if replayed["message_id"] != "ms_committed" {
		t.Fatalf("old retry operation was not replayed: %+v", replayed)
	}
	if messages := rowCount(t, s, "SELECT COUNT(*) FROM Message"); messages != 0 {
		t.Fatalf("replayed old operation sent another message: %d", messages)
	}
}

func TestProvenanceRecordedOnRegisterAndSucceed(t *testing.T) {
	s, clock, _ := testStore(t)
	checkProv := func(execID string, wantPrior sql.NullString, wantSource, wantProof string) {
		t.Helper()
		var prior sql.NullString
		var source, proof string
		if err := s.db.QueryRow("SELECT prior_execution_id,creation_source,creation_proof FROM Execution WHERE id=?", execID).Scan(&prior, &source, &proof); err != nil {
			t.Fatal(err)
		}
		if prior != wantPrior || source != wantSource || proof != wantProof {
			t.Fatalf("%s provenance=%+v/%s/%s want %+v/%s/%s", execID, prior, source, proof, wantPrior, wantSource, wantProof)
		}
	}

	hookReq := Request{Client: "codex", Host: "host", SessionID: "H", Cwd: "/repo/w", Worktree: "/repo/w", Source: "hook"}
	g1 := call(t, s, "sessions", hookReq)
	g1ID := g1["execution_id"].(string)
	checkProv(g1ID, sql.NullString{}, "hook", "initial")

	clock.advance(16 * time.Minute)
	call(t, s, "expire", Request{})

	g2 := call(t, s, "succeed", Request{Client: "codex", Host: "host", SessionID: "H", Source: "shim", PriorGeneration: 1, PriorExecutionID: g1ID})
	g2ID := g2["execution_id"].(string)
	checkProv(g2ID, sql.NullString{String: g1ID, Valid: true}, "shim", "shim_succession")

	clock.advance(61 * time.Second)
	call(t, s, "expire", Request{})

	g3Req := hookReq
	g3Req.RetryGeneration = 2
	g3 := call(t, s, "sessions", g3Req)
	g3ID := g3["execution_id"].(string)
	checkProv(g3ID, sql.NullString{String: g2ID, Valid: true}, "hook", "stale_retry")

	clock.advance(16 * time.Minute)
	call(t, s, "expire", Request{})
	g4 := call(t, s, "sessions", hookReq)
	checkProv(g4["execution_id"].(string), sql.NullString{String: g3ID, Valid: true}, "hook", "absent")

	for _, source := range []string{"", "mcp", "watch", "shim"} {
		wantSource := source
		if wantSource == "" {
			wantSource = "legacy"
		}
		reg := call(t, s, "sessions", Request{Client: "codex", Host: "host", SessionID: "S-" + wantSource, Cwd: "/repo/w", Worktree: "/repo/w", Source: source})
		checkProv(reg["execution_id"].(string), sql.NullString{}, wantSource, "initial")
	}
}

func TestSucceedCreatesOneHookUnseenGenerationAndReusesMail(t *testing.T) {
	s, clock, _ := testStore(t)
	registration := Request{Client: "claude", Host: "host", SessionID: "S", Cwd: "/repo/w", Worktree: "/repo/w", Branch: "main", ClientPID: 42, ClientPIDStartMS: 100, Source: "hook"}
	first := call(t, s, "sessions", registration)
	exec := Request{ExecutionID: first["execution_id"].(string), Generation: 1}
	call(t, s, "claim", claimRequest(exec, "old-claim", "old.go", "exact"))
	sender := call(t, s, "sessions", Request{Client: "claude", Host: "host", SessionID: "sender", Cwd: "/repo/w", Worktree: "/repo/w", Source: "hook"})
	send := Request{ExecutionID: sender["execution_id"].(string), Generation: 1, Key: "mail", To: []string{first["agent"].(map[string]any)["name"].(string)}, Body: "pending"}
	call(t, s, "messages", send)
	fetched := call(t, s, "inbox", exec)["deliveries"].([]map[string]any)
	if len(fetched) != 1 {
		t.Fatalf("fixture mail: %+v", fetched)
	}
	clock.advance(16 * time.Minute)
	call(t, s, "expire", Request{})
	req := Request{Client: "claude", Host: "host", SessionID: "S", Source: "shim", PriorGeneration: 1, PriorExecutionID: exec.ExecutionID}
	var change GenerationChange
	ctx := WithGenerationObserver(t.Context(), func(g GenerationChange) { change = g })
	tick := clock.Tick()
	out, err := s.Call(ctx, "succeed", req)
	if err != nil {
		t.Fatal(err)
	}
	newID := out["execution_id"].(string)
	if newID == exec.ExecutionID || out["generation"] != int64(2) || out["agent"].(map[string]any)["id"] != first["agent"].(map[string]any)["id"] || out["repository_id"] != first["repository_id"] {
		t.Fatalf("succession result: %+v", out)
	}
	var state, worktree, branch string
	var generation, deadline, renewed, hookSeen, heartbeat int64
	if err := s.db.QueryRow("SELECT state,generation,deadline_tick_ms,lease_renewed_tick_ms,COALESCE(hook_seen_ms,0),heartbeat_mode,worktree,branch FROM Execution WHERE id=?", newID).
		Scan(&state, &generation, &deadline, &renewed, &hookSeen, &heartbeat, &worktree, &branch); err != nil {
		t.Fatal(err)
	}
	if state != "active" || generation != 2 || deadline != tick+60000 || renewed != tick || hookSeen != 0 || heartbeat != 1 || worktree != "/repo/w" || branch != "main" {
		t.Fatalf("new Execution: state=%s generation=%d deadline=%d renewed=%d hook=%d heartbeat=%d worktree=%s branch=%s", state, generation, deadline, renewed, hookSeen, heartbeat, worktree, branch)
	}
	var oldState string
	if err := s.db.QueryRow("SELECT state FROM Execution WHERE id=?", exec.ExecutionID).Scan(&oldState); err != nil || oldState != "lost" {
		t.Fatalf("old Execution was revived: state=%s err=%v", oldState, err)
	}
	var claims int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM Claim WHERE execution_id=? AND revoked_ms IS NULL", newID).Scan(&claims); err != nil || claims != 0 {
		t.Fatalf("succession inherited claims: %d, %v", claims, err)
	}
	newExec := Request{ExecutionID: newID, Generation: 2}
	deliveries := call(t, s, "inbox", newExec)["deliveries"].([]map[string]any)
	if len(deliveries) != 1 || deliveries[0]["id"] != fetched[0]["id"] {
		t.Fatalf("requeued mail was not fetchable on the successor: %+v", deliveries)
	}
	ack := newExec
	ack.Key, ack.Attempt = deliveries[0]["id"].(string), number(deliveries[0]["attempt"])
	call(t, s, "ack", ack)
	if change.ExecutionID != newID || change.PreviousExecutionID != exec.ExecutionID || change.Generation != 2 || change.PreviousGeneration != 1 || change.Reason != "lease_expired" || change.Source != "shim" || change.Proof != "shim_succession" || change.RetryGeneration != 0 {
		t.Fatalf("generation diagnostic: %+v", change)
	}

	clock.advance(61 * time.Second)
	call(t, s, "expire", Request{})
	second := req
	second.PriorGeneration, second.PriorExecutionID = 2, newID
	refuseSuccession(t, s, second, 2)
}

func TestSucceedReplaysShimAndHookWinnersWithoutSideEffects(t *testing.T) {
	t.Run("shim winner", func(t *testing.T) {
		s, clock, _ := testStore(t)
		req, _ := successionFixture(t, s, clock, Request{Client: "claude", Host: "host", SessionID: "S", Cwd: "/repo/w", Worktree: "/repo/w", Branch: "main", ClientPID: 42, ClientPIDStartMS: 100})
		winner := call(t, s, "succeed", req)
		childID := winner["execution_id"].(string)

		// Put a claim on winner to verify replay does not touch or grant claims.
		call(t, s, "claim", claimRequest(Request{ExecutionID: childID, Generation: 2}, "k1", "a.go", "exact"))

		var beforeDeadline, beforeRenewed, beforeLastSeen, beforeHookSeen, beforeEvents int64
		if err := s.db.QueryRow("SELECT deadline_tick_ms,lease_renewed_tick_ms,last_seen_ms,hook_seen_ms FROM Execution WHERE id=?", childID).Scan(&beforeDeadline, &beforeRenewed, &beforeLastSeen, &beforeHookSeen); err != nil {
			t.Fatal(err)
		}
		if err := s.db.QueryRow("SELECT COALESCE(MAX(seq),0) FROM Event").Scan(&beforeEvents); err != nil {
			t.Fatal(err)
		}

		clock.advance(5 * time.Second)
		var observed bool
		ctx := WithGenerationObserver(t.Context(), func(GenerationChange) { observed = true })
		replay, err := s.Call(ctx, "succeed", req)
		if err != nil {
			t.Fatal(err)
		}
		if observed {
			t.Fatal("replay emitted a second GenerationChange")
		}
		if replay["execution_id"] != childID || replay["generation"] != int64(2) || replay["lease_ms"] != int64(55000) {
			t.Fatalf("replay response: %+v", replay)
		}
		var afterDeadline, afterRenewed, afterLastSeen, afterHookSeen, afterEvents int64
		if err := s.db.QueryRow("SELECT deadline_tick_ms,lease_renewed_tick_ms,last_seen_ms,hook_seen_ms FROM Execution WHERE id=?", childID).Scan(&afterDeadline, &afterRenewed, &afterLastSeen, &afterHookSeen); err != nil {
			t.Fatal(err)
		}
		if err := s.db.QueryRow("SELECT COALESCE(MAX(seq),0) FROM Event").Scan(&afterEvents); err != nil {
			t.Fatal(err)
		}
		if afterDeadline != beforeDeadline || afterRenewed != beforeRenewed || afterLastSeen != beforeLastSeen || afterHookSeen != beforeHookSeen || afterHookSeen != 0 || afterEvents != beforeEvents {
			t.Fatalf("replay mutated state: deadline=%d/%d renewed=%d/%d last=%d/%d hook=%d/%d events=%d/%d",
				afterDeadline, beforeDeadline, afterRenewed, beforeRenewed, afterLastSeen, beforeLastSeen, afterHookSeen, beforeHookSeen, afterEvents, beforeEvents)
		}
	})

	t.Run("hook winner", func(t *testing.T) {
		s, clock, _ := testStore(t)
		reg := Request{Client: "claude", Host: "host", SessionID: "S", Cwd: "/repo/w", Worktree: "/repo/w", Branch: "main", ClientPID: 42, ClientPIDStartMS: 100}
		req, _ := successionFixture(t, s, clock, reg)
		reg.Source = "hook"
		hookChild := call(t, s, "sessions", reg)
		childID := hookChild["execution_id"].(string)

		var beforeDeadline, beforeRenewed, beforeLastSeen, beforeHookSeen, beforeHeartbeat, beforeEvents int64
		if err := s.db.QueryRow("SELECT deadline_tick_ms,lease_renewed_tick_ms,last_seen_ms,hook_seen_ms,heartbeat_mode FROM Execution WHERE id=?", childID).
			Scan(&beforeDeadline, &beforeRenewed, &beforeLastSeen, &beforeHookSeen, &beforeHeartbeat); err != nil {
			t.Fatal(err)
		}
		if err := s.db.QueryRow("SELECT COALESCE(MAX(seq),0) FROM Event").Scan(&beforeEvents); err != nil {
			t.Fatal(err)
		}

		clock.advance(10 * time.Second)
		var observed bool
		ctx := WithGenerationObserver(t.Context(), func(GenerationChange) { observed = true })
		replay, err := s.Call(ctx, "succeed", req)
		if err != nil {
			t.Fatal(err)
		}
		if observed || replay["execution_id"] != childID || replay["generation"] != int64(2) || replay["lease_ms"] != int64(890000) {
			t.Fatalf("hook-winner replay: observed=%t out=%+v", observed, replay)
		}
		var afterDeadline, afterRenewed, afterLastSeen, afterHookSeen, afterHeartbeat, afterEvents int64
		if err := s.db.QueryRow("SELECT deadline_tick_ms,lease_renewed_tick_ms,last_seen_ms,hook_seen_ms,heartbeat_mode FROM Execution WHERE id=?", childID).
			Scan(&afterDeadline, &afterRenewed, &afterLastSeen, &afterHookSeen, &afterHeartbeat); err != nil {
			t.Fatal(err)
		}
		if err := s.db.QueryRow("SELECT COALESCE(MAX(seq),0) FROM Event").Scan(&afterEvents); err != nil {
			t.Fatal(err)
		}
		if afterDeadline != beforeDeadline || afterRenewed != beforeRenewed || afterLastSeen != beforeLastSeen || afterHookSeen != beforeHookSeen || afterHeartbeat != beforeHeartbeat || afterEvents != beforeEvents {
			t.Fatalf("hook-winner replay mutated state")
		}
	})

	t.Run("combined shell hook winner", func(t *testing.T) {
		s, clock, _ := testStore(t)
		reg := Request{Client: "codex", Host: "host", SessionID: "S", Cwd: "/repo/w", Worktree: "/repo/w"}
		req, _ := successionFixture(t, s, clock, reg)
		shellHook := reg
		shellHook.Source, shellHook.Kind, shellHook.Status = "hook", "before", map[string]string{}
		out := call(t, s, "reconcile", shellHook)
		childID := out["session"].(map[string]any)["execution_id"].(string)

		clock.advance(5 * time.Second)
		replay, err := s.Call(t.Context(), "succeed", req)
		if err != nil {
			t.Fatalf("combined shell hook-winner replay: %v", err)
		}
		if replay["execution_id"] != childID || replay["generation"] != int64(2) {
			t.Fatalf("unexpected shell hook-winner replay: %+v", replay)
		}
	})
}

func TestSucceedRecoveryBoundaryAndSuccessorStallCredit(t *testing.T) {
	s, clock, file := testStore(t)
	registration := Request{Client: "codex", Host: "host", SessionID: "S", Cwd: "/repo/w", Worktree: "/repo/w", Source: "hook"}
	first := call(t, s, "sessions", registration)
	s = restart(t, s, file, clock)
	req := Request{Client: "codex", Host: "host", SessionID: "S", Source: "shim", PriorGeneration: 1, PriorExecutionID: first["execution_id"].(string)}
	clock.advance(119 * time.Second)
	refuseSuccession(t, s, req, 1)
	clock.advance(2 * time.Second)
	var change GenerationChange
	ctx := WithGenerationObserver(t.Context(), func(g GenerationChange) { change = g })
	tick := clock.Tick()
	out, err := s.Call(ctx, "succeed", req)
	if err != nil || change.Reason != "recovery_expired" {
		t.Fatalf("post-window succession: out=%+v change=%+v err=%v", out, change, err)
	}
	newID := out["execution_id"].(string)
	s.SetStreamSweep(true)
	clock.advance(70 * time.Second)
	call(t, s, "expire", Request{})
	row := readExecutionRow(t, s, newID)
	if row.state != "active" || row.renewed != tick || row.deadline != tick+90000 {
		t.Fatalf("successor did not receive bounded stall credit from its creation baseline: %+v, tick=%d", row, tick)
	}
}

func TestSucceedValidatesRequestsWithoutMutation(t *testing.T) {
	s, clock, _ := testStore(t)
	base, _ := successionFixture(t, s, clock, Request{Client: "codex", Host: "host", SessionID: "S", Cwd: "/repo/w", Worktree: "/repo/w"})
	for _, tc := range []struct {
		name   string
		change func(*Request)
	}{
		{"client", func(r *Request) { r.Client = "" }},
		{"host", func(r *Request) { r.Host = "" }},
		{"session", func(r *Request) { r.SessionID = "" }},
		{"zero generation", func(r *Request) { r.PriorGeneration = 0 }},
		{"negative generation", func(r *Request) { r.PriorGeneration = -1 }},
		{"negative ended floor", func(r *Request) { r.EndedThrough = -1 }},
		{"execution", func(r *Request) { r.PriorExecutionID = "" }},
		{"missing source", func(r *Request) { r.Source = "" }},
		{"mcp source", func(r *Request) { r.Source = "mcp" }},
		{"watch source", func(r *Request) { r.Source = "watch" }},
		{"hook source", func(r *Request) { r.Source = "hook" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := base
			tc.change(&req)
			before := databaseState(t, s)
			out, err := s.Call(t.Context(), "succeed", req)
			var domain *Error
			if out != nil || !errors.As(err, &domain) || domain.Code != "bad_request" || databaseState(t, s) != before {
				t.Fatalf("out=%+v err=%v", out, err)
			}
		})
	}
}

func TestSucceedEndedThroughFloorAndReplay(t *testing.T) {
	for _, tc := range []struct {
		name   string
		replay bool
		floor  int64
		wantHi int64
	}{
		{"create equal floor", false, 1, 1},
		{"create higher floor", false, 2, 1},
		{"replay equal prev floor", true, 1, 2},
		{"replay equal child floor", true, 2, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, clock, _ := testStore(t)
			req, _ := successionFixture(t, s, clock, Request{Client: "codex", Host: "host", SessionID: "S", Cwd: "/repo/w", Worktree: "/repo/w"})
			if tc.replay {
				call(t, s, "succeed", req)
			}
			req.EndedThrough = tc.floor
			before := databaseState(t, s)
			out, err := s.Call(t.Context(), "succeed", req)
			if out != nil || databaseState(t, s) != before {
				t.Fatalf("ended succession returned authority or changed state: %+v, %v", out, err)
			}
			assertRegistrationLost(t, err, tc.wantHi)
			var lost *Error
			if !errors.As(err, &lost) {
				t.Fatal("strict succession refusal lost its typed error")
			}
			details, _ := lost.Details.(map[string]any)
			if details["reason"] != "ended_through" {
				t.Fatalf("strict succession reason = %v", details)
			}
		})
	}
	t.Run("prior above the floor", func(t *testing.T) {
		s, clock, _ := testStore(t)
		registration := Request{Client: "codex", Host: "host", SessionID: "S", Cwd: "/repo/w", Worktree: "/repo/w", Source: "hook"}
		first := call(t, s, "sessions", registration)
		call(t, s, "end", Request{ExecutionID: first["execution_id"].(string), Generation: 1})
		req, _ := successionFixture(t, s, clock, registration)
		req.EndedThrough = 1
		out := call(t, s, "succeed", req)
		if out["generation"] != int64(3) {
			t.Fatalf("succession above the end floor: %+v", out)
		}
	})
}

func TestSucceedRefusesIneligibleCreateAndReplay(t *testing.T) {
	t.Run("no retained history", func(t *testing.T) {
		s, _, _ := testStore(t)
		refuseSuccession(t, s, Request{Client: "codex", Host: "host", SessionID: "none", Source: "shim", PriorGeneration: 1, PriorExecutionID: "ex_missing"}, 0)
	})
	for _, state := range []string{"active", "ended"} {
		t.Run(state, func(t *testing.T) {
			s, _, _ := testStore(t)
			registration := Request{Client: "codex", Host: "host", SessionID: "S", Cwd: "/repo/w", Worktree: "/repo/w", Source: "hook"}
			first := call(t, s, "sessions", registration)
			if state == "ended" {
				call(t, s, "end", Request{ExecutionID: first["execution_id"].(string), Generation: 1})
			}
			refuseSuccession(t, s, Request{Client: "codex", Host: "host", SessionID: "S", Source: "shim", PriorGeneration: 1, PriorExecutionID: first["execution_id"].(string)}, 1)
		})
	}
	for _, tc := range []struct {
		name       string
		generation int64
		execution  string
	}{{"ahead local proof", 3, "ex_live"}, {"equal-generation different history", 1, "ex_other"}} {
		t.Run(tc.name, func(t *testing.T) {
			s, clock, _ := testStore(t)
			req, _ := successionFixture(t, s, clock, Request{Client: "codex", Host: "host", SessionID: "S", Cwd: "/repo/w", Worktree: "/repo/w"})
			req.PriorGeneration, req.PriorExecutionID = tc.generation, tc.execution
			refuseSuccession(t, s, req, 1)
		})
	}
	t.Run("hook unseen", func(t *testing.T) {
		s, clock, _ := testStore(t)
		registration := Request{Client: "codex", Host: "host", SessionID: "S", Cwd: "/repo/w", Worktree: "/repo/w", Source: "mcp"}
		first := call(t, s, "sessions", registration)
		clock.advance(16 * time.Minute)
		call(t, s, "expire", Request{})
		refuseSuccession(t, s, Request{Client: "codex", Host: "host", SessionID: "S", Source: "shim", PriorGeneration: 1, PriorExecutionID: first["execution_id"].(string)}, 1)
	})
	t.Run("replay unknown prior execution at G", func(t *testing.T) {
		s, clock, _ := testStore(t)
		req, _ := successionFixture(t, s, clock, Request{Client: "codex", Host: "host", SessionID: "S", Cwd: "/repo/w", Worktree: "/repo/w"})
		call(t, s, "succeed", req)
		req.PriorExecutionID = "ex_wrong"
		refuseSuccession(t, s, req, 2)
	})
	t.Run("replay non-lost or hook-unseen prior at G", func(t *testing.T) {
		s, clock, _ := testStore(t)
		req, _ := successionFixture(t, s, clock, Request{Client: "codex", Host: "host", SessionID: "S", Cwd: "/repo/w", Worktree: "/repo/w"})
		call(t, s, "succeed", req)
		if _, err := s.db.Exec("UPDATE Execution SET hook_seen_ms=0 WHERE id=?", req.PriorExecutionID); err != nil {
			t.Fatal(err)
		}
		refuseSuccession(t, s, req, 2)
	})
	t.Run("replay missing provenance lineage", func(t *testing.T) {
		s, clock, _ := testStore(t)
		req, _ := successionFixture(t, s, clock, Request{Client: "codex", Host: "host", SessionID: "S", Cwd: "/repo/w", Worktree: "/repo/w"})
		child := call(t, s, "succeed", req)
		if _, err := s.db.Exec("UPDATE Execution SET prior_execution_id=NULL WHERE id=?", child["execution_id"]); err != nil {
			t.Fatal(err)
		}
		refuseSuccession(t, s, req, 2)
	})
	t.Run("replay child in different worktree/mailbox", func(t *testing.T) {
		s, clock, _ := testStore(t)
		req, _ := successionFixture(t, s, clock, Request{Client: "codex", Host: "host", SessionID: "S", Cwd: "/repo/w1", Worktree: "/repo/w1"})
		call(t, s, "sessions", Request{Client: "codex", Host: "host", SessionID: "S", Cwd: "/repo/w2", Worktree: "/repo/w2", Source: "hook"})
		refuseSuccession(t, s, req, 2)
	})
	t.Run("replay child ended", func(t *testing.T) {
		s, clock, _ := testStore(t)
		req, _ := successionFixture(t, s, clock, Request{Client: "codex", Host: "host", SessionID: "S", Cwd: "/repo/w", Worktree: "/repo/w"})
		child := call(t, s, "succeed", req)
		call(t, s, "end", Request{ExecutionID: child["execution_id"].(string), Generation: 2})
		refuseSuccession(t, s, req, 2)
	})
	t.Run("replay child expired", func(t *testing.T) {
		s, clock, _ := testStore(t)
		req, _ := successionFixture(t, s, clock, Request{Client: "codex", Host: "host", SessionID: "S", Cwd: "/repo/w", Worktree: "/repo/w"})
		call(t, s, "succeed", req)
		clock.advance(61 * time.Second)
		call(t, s, "expire", Request{})
		refuseSuccession(t, s, req, 2)
	})
	t.Run("replay child recovering after restart follows D9", func(t *testing.T) {
		s, clock, file := testStore(t)
		req, _ := successionFixture(t, s, clock, Request{Client: "codex", Host: "host", SessionID: "S", Cwd: "/repo/w", Worktree: "/repo/w"})
		call(t, s, "succeed", req)
		s = restart(t, s, file, clock)
		refuseSuccession(t, s, req, 2)
	})
	t.Run("already moved past G+1", func(t *testing.T) {
		s, clock, _ := testStore(t)
		req, _ := successionFixture(t, s, clock, Request{Client: "codex", Host: "host", SessionID: "S", Cwd: "/repo/w", Worktree: "/repo/w"})
		call(t, s, "succeed", req)
		clock.advance(61 * time.Second)
		call(t, s, "expire", Request{})
		call(t, s, "sessions", Request{Client: "codex", Host: "host", SessionID: "S", Cwd: "/repo/w", Worktree: "/repo/w", Source: "hook"})
		refuseSuccession(t, s, req, 3)
	})
}

func TestSucceedReplayRefusesNullCreationSourceAtGuard(t *testing.T) {
	testSucceedReplayRefusesNullProvenanceAtGuard(t, "creation_source=NULL")
}

func TestSucceedReplayRefusesNullCreationProofAtGuard(t *testing.T) {
	testSucceedReplayRefusesNullProvenanceAtGuard(t, "creation_proof=NULL")
}

func TestSucceedReplayRefusesNullCreationProvenanceAtGuard(t *testing.T) {
	testSucceedReplayRefusesNullProvenanceAtGuard(t, "creation_source=NULL,creation_proof=NULL")
}

func testSucceedReplayRefusesNullProvenanceAtGuard(t *testing.T, nullColumns string) {
	t.Helper()
	s, clock, _ := testStore(t)
	req, _ := successionFixture(t, s, clock, Request{Client: "codex", Host: "host", SessionID: "S", Cwd: "/repo/w", Worktree: "/repo/w"})
	child := call(t, s, "succeed", req)
	childID := child["execution_id"].(string)
	if _, err := s.db.Exec("UPDATE Execution SET "+nullColumns+" WHERE id=?", childID); err != nil {
		t.Fatal(err)
	}

	var prior, source, proof sql.NullString
	if err := s.db.QueryRow("SELECT prior_execution_id,creation_source,creation_proof FROM Execution WHERE id=?", childID).Scan(&prior, &source, &proof); err != nil {
		t.Fatal(err)
	}
	if !prior.Valid || prior.String != req.PriorExecutionID {
		t.Fatalf("child lineage changed: prior=%+v want=%s", prior, req.PriorExecutionID)
	}

	type executionState struct {
		id         string
		generation int64
		state      string
		hookSeen   sql.NullInt64
	}
	readState := func(id string) executionState {
		t.Helper()
		var row executionState
		if err := s.db.QueryRow("SELECT id,generation,state,hook_seen_ms FROM Execution WHERE id=?", id).
			Scan(&row.id, &row.generation, &row.state, &row.hookSeen); err != nil {
			t.Fatal(err)
		}
		return row
	}
	beforeParent, beforeChild := readState(req.PriorExecutionID), readState(childID)
	beforeDatabase := databaseState(t, s)

	out, err := s.Call(t.Context(), "succeed", req)
	if out != nil {
		t.Fatalf("NULL provenance replay returned authority: %+v", out)
	}
	var lost *Error
	if !errors.As(err, &lost) || lost.Code != "lost" {
		t.Fatalf("NULL provenance replay error = %v, want code lost", err)
	}
	assertRegistrationLost(t, err, 2)
	if lost.Message != "child Execution was not created by eligible succession or hook registration" {
		t.Fatalf("NULL provenance replay did not reach the provenance guard: %v", err)
	}
	t.Logf("lineage prior_execution_id=%q; guard refusal code=%s message=%q", prior.String, lost.Code, lost.Message)

	afterParent, afterChild := readState(req.PriorExecutionID), readState(childID)
	if afterParent != beforeParent || afterChild != beforeChild {
		t.Fatalf("refused replay changed Execution rows: parent before=%+v after=%+v; child before=%+v after=%+v", beforeParent, afterParent, beforeChild, afterChild)
	}
	if databaseState(t, s) != beforeDatabase {
		t.Fatal("refused NULL provenance replay mutated the database")
	}
}

func TestSucceedProcessIdentityAndBusyMailboxRefusals(t *testing.T) {
	for _, tc := range []struct {
		name   string
		single bool
		pid    int64
		start  int64
		change func(*Request)
	}{
		{"missing process", true, 42, 100, func(r *Request) { r.ClientPID, r.ClientPIDStartMS = 0, 0 }},
		{"wrong pid", true, 42, 100, func(r *Request) { r.ClientPID++ }},
		{"reused pid", true, 42, 100, func(r *Request) { r.ClientPIDStartMS++ }},
		{"process proof on rung 2", false, 42, 100, func(r *Request) { r.ClientPID++ }},
		{"single session without recorded process", true, 0, 0, func(r *Request) {}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, clock, _ := testStore(t)
			req, _ := successionFixture(t, s, clock, Request{Client: "cursor", Host: "host", SessionID: "S", Cwd: "/repo/w", Worktree: "/repo/w", ClientPID: tc.pid, ClientPIDStartMS: tc.start, SingleSession: tc.single})
			tc.change(&req)
			refuseSuccession(t, s, req, 1)
		})
	}
	t.Run("replay refuses wrong prev pid or hook resumed in new process", func(t *testing.T) {
		s, clock, _ := testStore(t)
		reg := Request{Client: "cursor", Host: "host", SessionID: "S", Cwd: "/repo/w", Worktree: "/repo/w", ClientPID: 42, ClientPIDStartMS: 100, SingleSession: true}
		req, _ := successionFixture(t, s, clock, reg)
		reg.Source, reg.ClientPID, reg.ClientPIDStartMS = "hook", 99, 200
		call(t, s, "sessions", reg)
		badPrev := req
		badPrev.ClientPID++
		refuseSuccession(t, s, badPrev, 2)
		refuseSuccession(t, s, req, 2)
	})
	t.Run("active mailbox holder", func(t *testing.T) {
		s, clock, _ := testStore(t)
		registration := Request{Client: "codex", Host: "host", SessionID: "S", Cwd: "/repo/w", Worktree: "/repo/w"}
		req, previous := successionFixture(t, s, clock, registration)
		other := call(t, s, "sessions", Request{Client: "codex", Host: "host", SessionID: "other", Cwd: "/repo/w", Worktree: "/repo/w", Source: "hook"})
		if _, err := s.db.Exec("UPDATE Execution SET agent_id=? WHERE id=?", previous["agent"].(map[string]any)["id"], other["execution_id"]); err != nil {
			t.Fatal(err)
		}
		refuseSuccession(t, s, req, 1)
	})
}

func TestSucceedRefusesClearSupersessionOnCreateAndReplay(t *testing.T) {
	for _, state := range []string{"active", "lost", "ended"} {
		t.Run("create/"+state, func(t *testing.T) {
			s, clock, _ := testStore(t)
			process := Request{Client: "cursor", Host: "host", SessionID: "S1", Cwd: "/repo/w", Worktree: "/repo/w", ClientPID: 42, ClientPIDStartMS: 100, SingleSession: true}
			req, _ := successionFixture(t, s, clock, process)
			process.SessionID, process.Source = "S2", "hook"
			second := call(t, s, "sessions", process)
			switch state {
			case "lost":
				clock.advance(16 * time.Minute)
				call(t, s, "expire", Request{})
			case "ended":
				call(t, s, "end", Request{ExecutionID: second["execution_id"].(string), Generation: 1})
			}
			refuseSuccession(t, s, req, 1)
		})
	}
	t.Run("replay/superseded after child creation", func(t *testing.T) {
		s, clock, _ := testStore(t)
		process := Request{Client: "cursor", Host: "host", SessionID: "S1", Cwd: "/repo/w", Worktree: "/repo/w", ClientPID: 42, ClientPIDStartMS: 100, SingleSession: true}
		req, _ := successionFixture(t, s, clock, process)
		call(t, s, "succeed", req)
		process.SessionID, process.Source = "S2", "hook"
		call(t, s, "sessions", process)
		refuseSuccession(t, s, req, 2)
	})
}

func TestSucceedRestoreLineageAndMarkRestoredRotation(t *testing.T) {
	t.Run("equal-generation diverged restore", func(t *testing.T) {
		s, clock, file := testStore(t)
		registration := Request{Client: "codex", Host: "host", SessionID: "S", Cwd: "/repo/w", Worktree: "/repo/w", Source: "hook"}
		call(t, s, "sessions", registration)
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		backup := filepath.Join(t.TempDir(), "backup.db")
		copyFile(t, file, backup)
		var err error
		s, err = Open(file, clock)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
		clock.advance(121 * time.Second)
		live := call(t, s, "sessions", registration)
		if live["generation"] != int64(2) {
			t.Fatalf("live generation: %+v", live)
		}
		restored, err := Open(backup, clock)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = restored.Close() })
		clock.advance(121 * time.Second)
		diverged := call(t, restored, "sessions", registration)
		if diverged["generation"] != int64(2) || diverged["execution_id"] == live["execution_id"] {
			t.Fatalf("restore did not diverge: live=%+v restored=%+v", live, diverged)
		}
		clock.advance(16 * time.Minute)
		call(t, restored, "expire", Request{})
		refuseSuccession(t, restored, Request{Client: "codex", Host: "host", SessionID: "S", Source: "shim", PriorGeneration: 2, PriorExecutionID: live["execution_id"].(string)}, 2)
	})
	t.Run("mark-restored rotation grants no authority", func(t *testing.T) {
		s, clock, file := testStore(t)
		registration := Request{Client: "codex", Host: "host", SessionID: "S", Cwd: "/repo/w", Worktree: "/repo/w", Source: "hook"}
		first := call(t, s, "sessions", registration)
		oldStream := s.stream
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		if err := MarkRestored(t.Context(), file, clock.Now()); err != nil {
			t.Fatal(err)
		}
		var err error
		s, err = Open(file, clock)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
		if s.stream == oldStream || !s.Restored() {
			t.Fatalf("restore marker did not rotate the stream: old=%s new=%s restored=%t", oldStream, s.stream, s.Restored())
		}
		clock.advance(121 * time.Second)
		call(t, s, "expire", Request{})
		req := Request{Client: "codex", Host: "host", SessionID: "S", Source: "shim", PriorGeneration: 3, PriorExecutionID: "ex_live"}
		refuseSuccession(t, s, req, 1)
		req.PriorGeneration, req.PriorExecutionID = 1, first["execution_id"].(string)
		out := call(t, s, "succeed", req)
		if out["generation"] != int64(2) {
			t.Fatalf("exact proof on rotated stream did not succeed: %+v", out)
		}
	})
}

func TestSucceedAndHookOrShimRacesHaveOneChildID(t *testing.T) {
	for _, opponent := range []string{"hook", "shim"} {
		t.Run(opponent, func(t *testing.T) {
			for range 4 {
				s, clock, _ := testStore(t)
				registration := Request{Client: "codex", Host: "host", SessionID: "S", Cwd: "/repo/w", Worktree: "/repo/w", Source: "hook"}
				first := call(t, s, "sessions", registration)
				clock.advance(16 * time.Minute)
				call(t, s, "expire", Request{})
				req := Request{Client: "codex", Host: "host", SessionID: "S", Source: "shim", PriorGeneration: 1, PriorExecutionID: first["execution_id"].(string)}
				start := make(chan struct{})
				type result struct {
					out map[string]any
					err error
				}
				left, right := make(chan result, 1), make(chan result, 1)
				go func() {
					<-start
					out, err := s.Call(context.Background(), "succeed", req)
					left <- result{out, err}
				}()
				go func() {
					<-start
					if opponent == "hook" {
						out, err := s.Call(context.Background(), "sessions", registration)
						right <- result{out, err}
						return
					}
					out, err := s.Call(context.Background(), "succeed", req)
					right <- result{out, err}
				}()
				close(start)
				a, b := <-left, <-right
				if a.err != nil || b.err != nil {
					t.Fatalf("concurrent race failed: a=%v b=%v", a.err, b.err)
				}
				if a.out["execution_id"] != b.out["execution_id"] || a.out["generation"] != int64(2) || b.out["generation"] != int64(2) {
					t.Fatalf("race returned divergent children: a=%+v b=%+v", a.out, b.out)
				}
				var executions, agents, active, generation int
				if err := s.db.QueryRow("SELECT COUNT(*),COUNT(DISTINCT agent_id),SUM(state='active'),MAX(generation) FROM Execution WHERE host_id='host' AND client='codex' AND session_id='S'").Scan(&executions, &agents, &active, &generation); err != nil {
					t.Fatal(err)
				}
				if executions != 2 || agents != 1 || active != 1 || generation != 2 {
					t.Fatalf("race split generations or mailboxes: executions=%d agents=%d active=%d generation=%d", executions, agents, active, generation)
				}
				if opponent == "hook" {
					var hookSeen sql.NullInt64
					if err := s.db.QueryRow("SELECT hook_seen_ms FROM Execution WHERE generation=2").Scan(&hookSeen); err != nil || !hookSeen.Valid || hookSeen.Int64 == 0 {
						t.Fatalf("winner did not receive the hook: %+v, %v", hookSeen, err)
					}
				}
			}
		})
	}
}

func TestSucceedReplayIsolatesSweepStallCreditAndRefusesExpiredChild(t *testing.T) {
	t.Run("live replay performs no expiry sweep stall credit or event writes", func(t *testing.T) {
		s, clock, _ := testStore(t)
		req, _ := successionFixture(t, s, clock, Request{Client: "codex", Host: "host", SessionID: "S", Cwd: "/repo/w", Worktree: "/repo/w"})
		_, otherReg := session(t, s, 99)
		otherReq := Request{ExecutionID: otherReg["execution_id"].(string), Generation: 1, Source: "shim"}
		call(t, s, "heartbeat", otherReq) // deadline = now + 60s
		otherID := otherReg["execution_id"].(string)

		clock.advance(10 * time.Second)
		winner := call(t, s, "succeed", req) // child deadline = now + 60s
		childID := winner["execution_id"].(string)
		s.SetStreamSweep(true)
		beforeExpireTick := s.lastExpireTick
		var beforeEvents int64
		if err := s.db.QueryRow("SELECT COALESCE(MAX(seq),0) FROM Event").Scan(&beforeEvents); err != nil {
			t.Fatal(err)
		}
		changes := s.Changes()

		// Advance 51s: otherReq's 60s deadline is now <= tick (and fell inside the >5s stall gap), while child has 9s remaining.
		clock.advance(51 * time.Second)
		replay, err := s.Call(t.Context(), "succeed", req)
		if err != nil {
			t.Fatalf("live replay failed: %v", err)
		}
		if replay["execution_id"] != childID || replay["lease_ms"] != int64(9000) {
			t.Fatalf("unexpected live replay: %+v", replay)
		}
		var afterEvents int64
		if err := s.db.QueryRow("SELECT COALESCE(MAX(seq),0) FROM Event").Scan(&afterEvents); err != nil {
			t.Fatal(err)
		}
		if afterEvents != beforeEvents {
			t.Fatalf("replay emitted events: before=%d after=%d", beforeEvents, afterEvents)
		}
		select {
		case <-changes:
			t.Fatal("replay notified change listeners")
		default:
		}
		if s.lastExpireTick != beforeExpireTick {
			t.Fatalf("replay updated lastExpireTick: %d -> %d", beforeExpireTick, s.lastExpireTick)
		}
		otherRow := readExecutionRow(t, s, otherID)
		childRow := readExecutionRow(t, s, childID)
		if otherRow.state != "active" {
			t.Fatalf("replay swept unrelated execution: %+v", otherRow)
		}
		if childRow.state != "active" {
			t.Fatalf("child unexpected state: %+v", childRow)
		}
	})

	t.Run("expired child cannot be rescued by stall credit or returned with zero lease", func(t *testing.T) {
		s, clock, _ := testStore(t)
		req, _ := successionFixture(t, s, clock, Request{Client: "codex", Host: "host", SessionID: "S", Cwd: "/repo/w", Worktree: "/repo/w"})
		winner := call(t, s, "succeed", req)
		childID := winner["execution_id"].(string)
		s.SetStreamSweep(true)
		clock.advance(50 * time.Second)
		call(t, s, "expire", Request{})

		// At exact deadline (60s) and past deadline inside stall gap (61s), replay refuses without stall credit.
		for _, step := range []time.Duration{10 * time.Second, time.Second} {
			clock.advance(step)
			beforeDeadline := readExecutionRow(t, s, childID).deadline
			out, err := s.Call(t.Context(), "succeed", req)
			if out != nil {
				t.Fatalf("expired child replayed with %+v", out)
			}
			assertRegistrationLost(t, err, 2)
			if after := readExecutionRow(t, s, childID); after.deadline != beforeDeadline {
				t.Fatalf("refused replay modified child deadline: %d -> %d", beforeDeadline, after.deadline)
			}
		}
	})
}

func TestSucceedRefusesOrdinaryRegistrationReplayProvenances(t *testing.T) {
	for _, tc := range []struct {
		name       string
		source     string
		retryGen   int64
		touchAfter bool
	}{
		{"mcp absent", "mcp", 0, false},
		{"mcp stale_retry", "mcp", 1, false},
		{"mcp touched later by hook", "mcp", 0, true},
		{"watch", "watch", 0, false},
		{"legacy empty source", "", 0, false},
		{"ordinary generation-zero shim", "shim", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, clock, _ := testStore(t)
			base := Request{Client: "codex", Host: "host", SessionID: "S", Cwd: "/repo/w", Worktree: "/repo/w", Branch: "main"}
			req, _ := successionFixture(t, s, clock, base)
			reg := base
			reg.Source = tc.source
			reg.RetryGeneration = tc.retryGen
			child := call(t, s, "sessions", reg)
			if tc.touchAfter {
				call(t, s, "touch", touchRequest(Request{ExecutionID: child["execution_id"].(string), Generation: 2}, "tc_1", "a.go"))
			}
			var observed bool
			ctx := WithGenerationObserver(t.Context(), func(GenerationChange) { observed = true })
			before := databaseState(t, s)
			out, err := s.Call(ctx, "succeed", req)
			if out != nil || observed || databaseState(t, s) != before {
				t.Fatalf("ordinary registration %q replayed or mutated state: out=%+v observed=%t err=%v", tc.name, out, observed, err)
			}
			assertRegistrationLost(t, err, 2)
		})
	}

	t.Run("hook source without hook_seen_ms", func(t *testing.T) {
		s, clock, _ := testStore(t)
		base := Request{Client: "codex", Host: "host", SessionID: "S", Cwd: "/repo/w", Worktree: "/repo/w", Branch: "main"}
		req, _ := successionFixture(t, s, clock, base)
		reg := base
		reg.Source = "hook"
		child := call(t, s, "sessions", reg)
		if _, err := s.db.Exec("UPDATE Execution SET hook_seen_ms=0 WHERE id=?", child["execution_id"]); err != nil {
			t.Fatal(err)
		}
		refuseSuccession(t, s, req, 2)
	})
}
