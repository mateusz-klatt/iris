package store

import (
	"database/sql"
	"errors"
)

func normalizeCreationSource(source string) string {
	switch source {
	case "hook", "shim", "watch", "mcp":
		return source
	default:
		return "legacy"
	}
}

func nullableString(s sql.NullString) any {
	if !s.Valid {
		return nil
	}
	return s.String
}

func isHooklessSuccessionChild(e execution) bool {
	return e.CreationSource.String == "shim" && e.CreationProof.String == "shim_succession" && e.HookSeenMS == 0
}

func allowHooklessHeartbeat(e execution, r Request) error {
	if isHooklessSuccessionChild(e) && (r.Source != "shim" || !r.SuccessionProof) {
		return fail("lost", "hookless succession child requires shim succession_proof opt-in")
	}
	return nil
}

func creationProvenance(previous execution, r Request) (priorID any, source, proof string) {
	source = normalizeCreationSource(r.Source)
	if previous.ID == "" {
		return nil, source, "initial"
	}
	proof = "absent"
	if r.RetryGeneration > 0 {
		proof = "stale_retry"
	}
	return previous.ID, source, proof
}

func (t *transaction) checkSuccessionProcess(target execution, r Request) error {
	if r.ClientPID != 0 || r.ClientPIDStartMS != 0 || target.Single {
		if r.ClientPID <= 0 || r.ClientPIDStartMS <= 0 || target.PID != r.ClientPID || target.PIDStart != r.ClientPIDStartMS {
			return fail("lost", "succession requires the exact client process")
		}
	}
	return nil
}

func (t *transaction) checkProcessSuperseded(prev execution) error {
	if (prev.Single || (!prev.Parent.Valid && prev.Client == "claude")) && prev.PID > 0 && prev.PIDStart > 0 {
		var superseded int
		if err := t.row(`SELECT COUNT(*) FROM Execution WHERE host_id=? AND client=? AND client_pid=? AND client_pid_start_ms=?
			AND hook_seen_ms>0 AND session_id<>? AND rowid>(SELECT rowid FROM Execution WHERE id=?)`,
			prev.Host, prev.Client, prev.PID, prev.PIDStart, prev.Session, prev.ID).Scan(&superseded); err != nil {
			return err
		}
		if superseded > 0 {
			return fail("lost", "a newer hook session superseded this client process session")
		}
	}
	return nil
}

// succeed creates one new generation from exact local proof of a lost hook-registered Execution,
// or replays an already-committed live G+1 child Execution derived from that exact predecessor
// without renewing its lease, granting claims, marking hook reachability, or emitting events.
func (t *transaction) succeed(r Request) (map[string]any, error) {
	if r.Client == "" || r.SessionID == "" || r.Host == "" || r.PriorGeneration <= 0 || r.PriorExecutionID == "" {
		return nil, fail("bad_request", "client, session_id, host, prior_generation and prior_execution_id are required")
	}
	if r.EndedThrough < 0 {
		return nil, fail("bad_request", "ended_through cannot be negative")
	}
	if r.Source != "shim" {
		return nil, fail("bad_request", "succession source must be shim")
	}
	latest, err := scanExecution(t.row(executionSelect+"WHERE e.host_id=? AND e.client=? AND e.session_id=? ORDER BY e.generation DESC LIMIT 1", r.Host, r.Client, r.SessionID))
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if err == nil && latest.Generation == r.PriorGeneration+1 {
		return t.replaySuccession(latest, r)
	}
	// Run the shared expiry sweep and stall credit only when not replaying an existing child.
	if err := t.expire(); err != nil {
		return nil, err
	}
	latest, err = scanExecution(t.row(executionSelect+"WHERE e.host_id=? AND e.client=? AND e.session_id=? ORDER BY e.generation DESC LIMIT 1", r.Host, r.Client, r.SessionID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fail("lost", "succession requires retained session history")
	}
	if err != nil {
		return nil, err
	}
	if latest.Generation != r.PriorGeneration || latest.ID != r.PriorExecutionID || latest.HookSeenMS == 0 || latest.State != "lost" {
		return nil, fail("lost", "succession requires the newest lost hook-registered Execution and exact proof")
	}
	if r.EndedThrough > 0 && latest.Generation <= r.EndedThrough {
		return nil, endedThroughRefusal("succession requires proof above the ended generation")
	}
	if err := t.checkSuccessionProcess(latest, r); err != nil {
		return nil, err
	}
	if err := t.checkProcessSuperseded(latest); err != nil {
		return nil, err
	}
	var busy int
	if err := t.row("SELECT COUNT(*) FROM Execution WHERE agent_id=? AND state='active'", latest.AgentID).Scan(&busy); err != nil {
		return nil, err
	}
	if busy > 0 {
		return nil, fail("lost", "the session mailbox already has an active Execution")
	}
	ex := id("ex_")
	tick := t.s.clock.Tick()
	_, err = t.exec(`INSERT INTO Execution(id,agent_id,repository_id,host_id,client,session_id,generation,worktree,branch,boot_epoch,deadline_tick_ms,lease_renewed_tick_ms,state,parent_id,client_pid,client_pid_start_ms,single_session,heartbeat_mode,prior_execution_id,creation_source,creation_proof) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,'active',?,?,?,?,?,?,?,?)`,
		ex, latest.AgentID, latest.Repo, latest.Host, latest.Client, latest.Session, latest.Generation+1, latest.Worktree, latest.Branch, t.s.epoch, tick+60000, tick, latest.Parent, latest.PID, latest.PIDStart, latest.Single, true, latest.ID, "shim", "shim_succession")
	if err != nil {
		return nil, err
	}
	t.insertedExecution = ex
	current, err := scanExecution(t.row(executionSelect+"WHERE e.id=?", ex))
	if err != nil {
		return nil, err
	}
	t.generationChanged(latest, current, r)
	t.generationChange.Proof = "shim_succession"
	return t.sessionResponse(current, 60000, true)
}

func (t *transaction) replaySuccession(latest execution, r Request) (map[string]any, error) {
	prev, err := scanExecution(t.row(executionSelect+"WHERE e.host_id=? AND e.client=? AND e.session_id=? AND e.id=? AND e.generation=?", r.Host, r.Client, r.SessionID, r.PriorExecutionID, r.PriorGeneration))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fail("lost", "succession requires the newest lost hook-registered Execution and exact proof")
	}
	if err != nil {
		return nil, err
	}
	if prev.HookSeenMS == 0 || prev.State != "lost" {
		return nil, fail("lost", "succession requires the newest lost hook-registered Execution and exact proof")
	}
	if r.EndedThrough > 0 && prev.Generation <= r.EndedThrough {
		return nil, endedThroughRefusal("succession requires proof above the ended generation")
	}
	if !latest.PriorExecutionID.Valid || latest.PriorExecutionID.String != prev.ID {
		return nil, fail("lost", "child Execution does not descend from this predecessor")
	}
	shimChild := latest.CreationSource.String == "shim" && latest.CreationProof.String == "shim_succession"
	hookChild := latest.CreationSource.String == "hook" && latest.HookSeenMS > 0
	if !shimChild && !hookChild {
		return nil, fail("lost", "child Execution was not created by eligible succession or hook registration")
	}
	if latest.AgentID != prev.AgentID || latest.Repo != prev.Repo {
		return nil, fail("lost", "child Execution changed mailbox or repository")
	}
	if err := t.checkSuccessionProcess(prev, r); err != nil {
		return nil, err
	}
	if err := t.checkSuccessionProcess(latest, r); err != nil {
		return nil, err
	}
	if err := t.checkProcessSuperseded(prev); err != nil {
		return nil, err
	}
	remaining := latest.Deadline - t.s.clock.Tick()
	if latest.State != "active" || latest.Epoch != t.s.epoch || latest.Recovery.Valid || remaining <= 0 {
		return nil, fail("lost", "child Execution is no longer live; reattach the session")
	}
	t.replayed = true
	return t.sessionResponse(latest, remaining, true)
}
