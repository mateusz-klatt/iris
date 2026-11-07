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

// wait records that e was refused key, of kind exact or prefix, because of
// the blocking claim. It lasts until that claim's expiry or waitLimit,
// whichever comes first.
func (t *transaction) wait(e execution, key, kind string, blocking claim) error {
	until := t.s.clock.Now().Add(waitLimit).UnixMilli()
	if blocking.Expiry.Valid && blocking.Expiry.Int64 < until {
		until = blocking.Expiry.Int64
	}
	_, err := t.exec(`INSERT INTO Waiter(execution_id,repository_id,path_key,kind,expires_ms) VALUES(?,?,?,?,?)
 ON CONFLICT(execution_id,path_key,kind) DO UPDATE SET expires_ms=MAX(Waiter.expires_ms,excluded.expires_ms)`, e.ID, e.Repo, key, kind, until)
	return err
}

// took clears e's waits on key once it holds it.
func (t *transaction) took(e execution, key string) error {
	_, err := t.exec("DELETE FROM Waiter WHERE execution_id=? AND path_key=?", e.ID, key)
	return err
}

type waiter struct {
	execution, agent, key, kind string
}

// wakeWaiters runs after claims in repo are released. A live waiter whose
// path no longer overlaps a claim of another Execution gets one event,
// addressed to it alone, and is cleared. One still blocked keeps waiting.
func (t *transaction) wakeWaiters(repo string) error {
	now := t.s.clock.Now().UnixMilli()
	if _, err := t.exec("DELETE FROM Waiter WHERE repository_id=? AND expires_ms<=?", repo, now); err != nil {
		return err
	}
	rows, err := t.rows(`SELECT w.execution_id,e.agent_id,w.path_key,w.kind FROM Waiter w JOIN Execution e ON e.id=w.execution_id
 WHERE w.repository_id=? AND e.state='active' ORDER BY w.execution_id,w.path_key`, repo)
	if err != nil {
		return err
	}
	var ws []waiter
	for rows.Next() {
		var w waiter
		rows.Scan(&w.execution, &w.agent, &w.key, &w.kind)
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
	cs, err := t.claimRows(repo)
	if err != nil {
		return err
	}
	for _, w := range ws {
		blocked, err := t.blocked(w, cs)
		if err != nil {
			return err
		}
		if blocked {
			continue
		}
		if err = t.event(w.agent, w.execution, "claim_available", w.key); err != nil {
			return err
		}
		if _, err = t.exec("DELETE FROM Waiter WHERE execution_id=? AND path_key=? AND kind=?", w.execution, w.key, w.kind); err != nil {
			return err
		}
	}
	return nil
}

// blocked is whether a claim of an unrelated Execution still covers w's path.
func (t *transaction) blocked(w waiter, cs []claim) (bool, error) {
	for _, c := range cs {
		if c.Execution == w.execution || !overlaps(w.key, w.kind, c.Key, c.Kind) {
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
