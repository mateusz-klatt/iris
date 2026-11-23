package store

import (
	"context"
	"database/sql"
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

func TestSucceedPreservesWorktreeScopedAutomaticClaims(t *testing.T) {
	s, clock, _ := testStore(t)
	registration := Request{
		Client: "codex", Host: "host", SessionID: "successor", Source: "hook",
		Cwd: "/repo/wt1", Worktree: "/repo/wt1", WorktreeID: "dev:1", RemoteURL: scopeRemote, RootCommit: "root", Branch: "main",
	}
	req, previous := successionFixture(t, s, clock, registration)
	repo := previous["repository_id"].(string)
	setScope(t, s, repo, "worktree")
	holder, _ := scopeSession(t, s, "host", "holder", "/repo/wt2", "dev:2", "other")
	call(t, s, "touch", touchRequest(holder, "held", "edit.go", "shell.go"))

	succeeded := call(t, s, "succeed", req)
	exec := Request{ExecutionID: succeeded["execution_id"].(string), Generation: succeeded["generation"].(int64)}
	edited := call(t, s, "touch", touchRequest(exec, "edit", "edit.go"))
	granted := edited["granted"].([]string)
	advisories, _ := edited["advisories"].([]map[string]any)
	if edited["action"] != "allow" || len(granted) != 1 || granted[0] != "edit.go" || len(advisories) != 1 {
		t.Errorf("successor edit did not claim a cross-worktree overlap with an advisory: %v", edited)
	}

	registration.Generation = exec.Generation
	registration.Kind, registration.Status = "before", map[string]string{}
	before := call(t, s, "reconcile", registration)
	if session := before["session"].(map[string]any); session["execution_id"] != exec.ExecutionID {
		t.Fatalf("shell hook bound to a different Execution: %v", session)
	}
	registration.Kind, registration.Status = "after", map[string]string{"shell.go": " M"}
	reconciled := call(t, s, "reconcile", registration)
	claimed := reconciled["claimed"].([]string)
	advisories, _ = reconciled["advisories"].([]map[string]any)
	if len(claimed) != 1 || claimed[0] != "shell.go" || len(advisories) != 1 || len(reconciled["conflicts"].([]map[string]any)) != 0 {
		t.Errorf("successor shell hook did not claim a cross-worktree overlap with an advisory: %v", reconciled)
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

func TestSucceedRefusesMissingStaleTerminalAndHookUnseenHistory(t *testing.T) {
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
	t.Run("rung 3 exact process succeeds", func(t *testing.T) {
		s, clock, _ := testStore(t)
		req, _ := successionFixture(t, s, clock, Request{Client: "cursor", Host: "host", SessionID: "S", Cwd: "/repo/w", Worktree: "/repo/w", ClientPID: 42, ClientPIDStartMS: 100, SingleSession: true})
		out := call(t, s, "succeed", req)
		if out["generation"] != int64(2) {
			t.Fatalf("rung 3 succession: %+v", out)
		}
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

func TestSucceedRefusesClearSupersessionInEverySuccessorState(t *testing.T) {
	for _, state := range []string{"active", "lost", "ended"} {
		t.Run(state, func(t *testing.T) {
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
}

func TestBindSuccessionWinnerRequiresExactRetainedPredecessor(t *testing.T) {
	s, clock, _ := testStore(t)
	registration := Request{Client: "codex", Host: "host", SessionID: "S", Cwd: "/repo/w", Worktree: "/repo/w", Source: "hook"}
	proof, _ := successionFixture(t, s, clock, registration)
	winner := call(t, s, "sessions", registration)
	out := call(t, s, "bind", proof)
	if out["execution_id"] != winner["execution_id"] || out["match"] != "succession" {
		t.Fatalf("exact predecessor did not confirm the hook winner: %+v", out)
	}
	proof.PriorExecutionID = "ex_other_history"
	if out, err := s.Call(t.Context(), "bind", proof); out != nil || !isCode(err, "unbound") {
		t.Fatalf("another history confirmed the hook winner: %+v %v", out, err)
	}
	proof.PriorGeneration, proof.PriorExecutionID = 2, winner["execution_id"].(string)
	out = call(t, s, "bind", proof)
	if out["execution_id"] != winner["execution_id"] || out["match"] != "succession" {
		t.Fatalf("exact proof advanced during reattach did not confirm the live winner: %+v", out)
	}
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

func TestSucceedAndHookOrShimRacesHaveOneGenerationWinner(t *testing.T) {
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
				if a.err != nil {
					assertRegistrationLost(t, a.err, 2)
				}
				if opponent == "shim" && b.err != nil {
					assertRegistrationLost(t, b.err, 2)
				}
				if opponent == "hook" && b.err != nil {
					t.Fatalf("registering hook lost the race: %v", b.err)
				}
				if (a.err == nil) == (b.err == nil) && opponent == "shim" {
					t.Fatalf("two shim results were both success or both refusal: %+v %+v", a, b)
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
					if err := s.db.QueryRow("SELECT hook_seen_ms FROM Execution WHERE generation=2").Scan(&hookSeen); err != nil || !hookSeen.Valid {
						t.Fatalf("winner did not receive the hook: %+v, %v", hookSeen, err)
					}
				}
			}
		})
	}
}
