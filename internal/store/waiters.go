package store

import (
	"time"
)

// A waiter is an Execution that was refused a path because another Execution
// held it. Only a waiter is woken when that path becomes free: a release that
// frees nobody's wait wakes no session, because each wake costs a model turn
// (D7). Every release is still an event for the console and the claims tool.

// waitLimit bounds a waiter. A session that has not taken the path within an
// hour has moved on, and a wake then would be noise.
const waitLimit = time.Hour

// wait records that e was refused key, of kind exact or prefix, in repo: the repository of the
// claim that refused it, which for a file in another checkout is not e's own (ADR 033). It lasts
// waitLimit whatever the blocking claim's expiry: that expiry is itself a release, and a wait
// that ended with it could not hear about it. One Execution keeps one wait per key and kind, as
// an older hub's table requires, so a later wait on the same key in another repository moves it.
// worktreeKey is the incoming claim's key. An explicit wait passes an empty key, which
// is stored as null and stays repository-wide. Under repository the stored key is
// ignored by took, blocked and wake (ADR 037).
func (t *transaction) wait(e execution, repo, key, kind, worktreeKey string) error {
	until := t.s.clock.Now().Add(waitLimit).UnixMilli()
	_, err := t.exec(`INSERT INTO Waiter(execution_id,repository_id,path_key,kind,expires_ms,worktree_key) VALUES(?,?,?,?,?,NULLIF(?,''))
 ON CONFLICT(execution_id,path_key,kind) DO UPDATE SET expires_ms=MAX(Waiter.expires_ms,excluded.expires_ms),repository_id=excluded.repository_id,worktree_key=excluded.worktree_key`, e.ID, repo, key, kind, until, worktreeKey)
	return err
}

// took clears e's waits on key in repo once it holds it there. Under worktree only the
// wait with the same key is cleared; under repository the key is ignored.
func (t *transaction) took(e execution, repo, key, worktreeKey string) error {
	_, err := t.exec("DELETE FROM Waiter WHERE execution_id=? AND repository_id=? AND path_key=? AND ((SELECT auto_claim_scope FROM Repository WHERE id=?)<>'worktree' OR COALESCE(worktree_key,'')=?)", e.ID, repo, key, repo, worktreeKey)
	return err
}

type waiter struct {
	execution, agent, key, kind, worktreeKey string
}

// wakeWaiters runs after claims in repo are released. A live waiter whose
// path no longer overlaps a claim of another Execution gets one event,
// addressed to it alone, and is cleared. One still blocked keeps waiting.
func (t *transaction) wakeWaiters(repo string) error {
	now := t.s.clock.Now().UnixMilli()
	if _, err := t.exec("DELETE FROM Waiter WHERE repository_id=? AND expires_ms<=?", repo, now); err != nil {
		return err
	}
	rows, err := t.rows(`SELECT w.execution_id,e.agent_id,w.path_key,w.kind,COALESCE(w.worktree_key,'') FROM Waiter w JOIN Execution e ON e.id=w.execution_id
 WHERE w.repository_id=? AND e.state='active' ORDER BY w.execution_id,w.path_key`, repo)
	if err != nil {
		return err
	}
	var ws []waiter
	for rows.Next() {
		var w waiter
		rows.Scan(&w.execution, &w.agent, &w.key, &w.kind, &w.worktreeKey)
		ws = append(ws, w)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return err
	}
	if len(ws) == 0 {
		return nil
	}
	scope, err := t.claimScope(repo)
	if err != nil {
		return err
	}
	cs, err := t.claimRows(repo)
	if err != nil {
		return err
	}
	for _, w := range ws {
		blocked, err := t.blocked(scope, w, cs)
		if err != nil {
			return err
		}
		if blocked {
			continue
		}
		// The event keeps the waiter's key and the repository that owned the wait.
		// Waking deletes the Waiter, and a later replay of the event is what
		// freedRewind reads (ADR 037). The Execution's repository is not that
		// repository when the file is in a submodule (ADR 033).
		if err = t.eventAt(w.agent, w.execution, "claim_available", w.key, w.worktreeKey, repo); err != nil {
			return err
		}
		if _, err = t.exec("DELETE FROM Waiter WHERE execution_id=? AND path_key=? AND kind=?", w.execution, w.key, w.kind); err != nil {
			return err
		}
	}
	return nil
}

// blocked is whether a claim of an unrelated Execution still covers w's path.
// Under worktree, decision 2's exempt automatic pair does not block. A null waiter
// key is not an automatic wait, so it is never exempt. Any other overlap blocks,
// including an explicit or null-key claim. Under repository the key is ignored.
func (t *transaction) blocked(scope string, w waiter, cs []claim) (bool, error) {
	incoming := "explicit"
	if w.worktreeKey != "" {
		incoming = "auto"
	}
	for _, c := range cs {
		if c.Execution == w.execution || !overlaps(w.key, w.kind, c.Key, c.Kind) {
			continue
		}
		if exemptAutoOverlap(scope, c.Origin, c.WorktreeKey, incoming, w.worktreeKey) {
			continue
		}
		family, err := t.related(w.execution, c.Execution)
		if err != nil {
			return false, err
		}
		if !family {
			return true, nil
		}
	}
	return false, nil
}
