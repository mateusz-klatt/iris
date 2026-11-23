package store

import (
	"database/sql"
	"errors"
)

// refuseProoflessRegistration keeps non-hook generation-zero registration for sessions whose
// hooks never reached the hub, while closing it over retained hook history and process history.
func (t *transaction) refuseProoflessRegistration(r Request) error {
	if r.Generation != 0 || r.Source == "hook" {
		return nil
	}
	var history int
	if err := t.row("SELECT COUNT(*) FROM Execution WHERE host_id=? AND client=? AND session_id=? AND hook_seen_ms>0", r.Host, r.Client, r.SessionID).Scan(&history); err != nil {
		return err
	}
	if history > 0 {
		return fail("lost", "non-hook generation-zero registration cannot succeed hook-registered history")
	}
	if r.ClientPID > 0 && r.ClientPIDStartMS > 0 && (r.SingleSession || r.Client == "claude") {
		if err := t.row("SELECT COUNT(*) FROM Execution WHERE host_id=? AND client=? AND client_pid=? AND client_pid_start_ms=? AND hook_seen_ms>0", r.Host, r.Client, r.ClientPID, r.ClientPIDStartMS).Scan(&history); err != nil {
			return err
		}
		if history > 0 {
			return fail("lost", "non-hook generation-zero registration cannot succeed hook process history")
		}
	}
	return nil
}

// succeed creates one new generation from exact local proof of a lost hook-registered Execution.
// The old Execution stays lost; only the new, initially hook-unseen Execution becomes active.
func (t *transaction) succeed(r Request) (map[string]any, error) {
	if r.Client == "" || r.SessionID == "" || r.Host == "" || r.PriorGeneration <= 0 || r.PriorExecutionID == "" {
		return nil, fail("bad_request", "client, session_id, host, prior_generation and prior_execution_id are required")
	}
	if r.Source != "shim" {
		return nil, fail("bad_request", "succession source must be shim")
	}
	prev, err := scanExecution(t.row(executionSelect+"WHERE e.host_id=? AND e.client=? AND e.session_id=? ORDER BY e.generation DESC LIMIT 1", r.Host, r.Client, r.SessionID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fail("lost", "succession requires retained session history")
	}
	if err != nil {
		return nil, err
	}
	if prev.Generation != r.PriorGeneration || prev.ID != r.PriorExecutionID || prev.HookSeenMS == 0 || prev.State != "lost" {
		return nil, fail("lost", "succession requires the newest lost hook-registered Execution and exact proof")
	}
	if r.ClientPID != 0 || r.ClientPIDStartMS != 0 || prev.Single {
		if r.ClientPID <= 0 || r.ClientPIDStartMS <= 0 || prev.PID != r.ClientPID || prev.PIDStart != r.ClientPIDStartMS {
			return nil, fail("lost", "succession requires the exact client process")
		}
	}
	if (prev.Single || (!prev.Parent.Valid && prev.Client == "claude")) && prev.PID > 0 && prev.PIDStart > 0 {
		var superseded int
		if err := t.row(`SELECT COUNT(*) FROM Execution WHERE host_id=? AND client=? AND client_pid=? AND client_pid_start_ms=?
			AND hook_seen_ms>0 AND session_id<>? AND rowid>(SELECT rowid FROM Execution WHERE id=?)`, prev.Host, prev.Client, prev.PID, prev.PIDStart, prev.Session, prev.ID).Scan(&superseded); err != nil {
			return nil, err
		}
		if superseded > 0 {
			return nil, fail("lost", "a newer hook session superseded this client process session")
		}
	}
	var busy int
	if err := t.row("SELECT COUNT(*) FROM Execution WHERE agent_id=? AND state='active'", prev.AgentID).Scan(&busy); err != nil {
		return nil, err
	}
	if busy > 0 {
		return nil, fail("lost", "the session mailbox already has an active Execution")
	}
	ex := id("ex_")
	tick := t.s.clock.Tick()
	_, err = t.exec(`INSERT INTO Execution(id,agent_id,repository_id,host_id,client,session_id,generation,worktree,branch,boot_epoch,deadline_tick_ms,lease_renewed_tick_ms,state,parent_id,client_pid,client_pid_start_ms,single_session,heartbeat_mode,worktree_key)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,'active',?,?,?,?,1,NULLIF(?,''))`, ex, prev.AgentID, prev.Repo, prev.Host, prev.Client, prev.Session, prev.Generation+1, prev.Worktree, prev.Branch, t.s.epoch, tick+60000, tick, prev.Parent, prev.PID, prev.PIDStart, prev.Single, prev.WorktreeKey)
	if err != nil {
		return nil, err
	}
	t.insertedExecution = ex
	current, err := scanExecution(t.row(executionSelect+"WHERE e.id=?", ex))
	if err != nil {
		return nil, err
	}
	t.generationChanged(prev, current, r)
	t.generationChange.Proof = "shim_succession"
	return t.sessionResponse(current, 60000)
}
