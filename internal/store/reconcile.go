package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// shellSession is whether a shell reconcile names its session rather than an Execution. The
// scoped route takes both forms, as the plain one does.
func shellSession(operation string, r Request) bool {
	return (operation == "reconcile" || operation == "reconcile_scoped") && r.ExecutionID == "" && r.SessionID != ""
}

// shellSession registers the session as /v1/sessions does, recovery proof included (D9, #127),
// then reconciles for the Execution it gives, all in one transaction: a shell hook pays one
// round trip to the hub instead of two. r then names that Execution, so the caller records
// the hook as seen there. The answer carries the Execution and generation for the hook to
// remember as its next proof.
func (t *transaction) shellSession(r *Request) (map[string]any, error) {
	sess, err := t.session(*r)
	if err != nil {
		return nil, err
	}
	r.ExecutionID, r.Generation = sess["execution_id"].(string), sess["generation"].(int64)
	if _, err = t.exec("UPDATE Execution SET last_seen_ms=? WHERE id=?", t.s.clock.Now().UnixMilli(), r.ExecutionID); err != nil {
		return nil, err
	}
	out, err := t.reconcile(*r)
	if err != nil {
		return nil, err
	}
	out["session"] = map[string]any{"execution_id": r.ExecutionID, "generation": r.Generation, "capabilities": shellCapabilities()}
	return out, t.event("", "", "changed", "")
}

func (t *transaction) reconcile(r Request) (map[string]any, error) {
	e, err := t.live(r.ExecutionID, r.Generation)
	if err != nil {
		return nil, err
	}
	if r.Kind != "before" && r.Kind != "after" {
		return nil, fail("bad_request", "reconciliation kind must be before or after")
	}
	empty := map[string]any{"message": "", "conflicts": []map[string]any{}}
	var count int
	if err = t.row("SELECT COUNT(*) FROM Execution WHERE repository_id=? AND host_id=? AND worktree=? AND state='active'", e.Repo, e.Host, e.Worktree).Scan(&count); err != nil {
		return nil, err
	}
	if count > 1 {
		// Another live Execution shares the filesystem: attributing changes is impossible.
		for _, table := range []string{"ShellSnapshot", "ReducedShellSnapshot"} {
			if _, err = t.exec("DELETE FROM "+table+" WHERE execution_id IN (SELECT id FROM Execution WHERE repository_id=? AND host_id=? AND worktree=?)", e.Repo, e.Host, e.Worktree); err != nil {
				return nil, err
			}
		}
		return empty, nil
	}
	for p := range r.Status {
		if _, err = cleanPath(p, "exact", false); err != nil {
			return nil, err
		}
	}
	previous, scope, found, err := t.shellBaseline(e.ID)
	if err != nil {
		return nil, err
	}
	if err = t.keepShellSnapshot(e.ID, r.Status, r.Scope); err != nil {
		return nil, err
	}
	// A snapshot is compared only with a baseline of the same scope, its submodule paths
	// included. Any other baseline is replaced and nothing is attributed (ADR 018 decision 2).
	if r.Kind == "before" || !found || scope != scopeKey(r.Scope) {
		return empty, nil
	}
	// Only paths still changed after the command count. One that left the status was committed,
	// stashed or restored by git in this worktree, which changes nothing in a holder's worktree,
	// so it is neither warned about nor claimed.
	changed, left := shellChanges(previous, r.Status, r.Scope)
	out, err := t.checkPaths(e.Repo, e.Host, e.Worktree, changed)
	if err != nil {
		return nil, err
	}
	// A write a shell or Codex's js tool made is claimed for this session afterwards, as an edit
	// tool's would have been before (gap review C7). A path someone else holds stays theirs, and
	// the message above reports it.
	claimed, skipped, err := t.autoClaim(e, changed)
	if err != nil {
		return nil, err
	}
	// An auto claim means this session has uncommitted changes at its path (D10). One whose path
	// left the status was committed, stashed or restored, so it is released, and its waiters are
	// woken as by a release. Explicit claims, and every other session's, stay.
	if err = t.releaseLeft(e, left); err != nil {
		return nil, err
	}
	out["claimed"] = claimed
	if skipped > 0 {
		// One command can change more paths than the claim limit allows. The warning about other
		// sessions' paths still matters most, so the rest are left unclaimed and that is said.
		note := fmt.Sprintf("Iris claimed %d changed paths for this session and left %d unclaimed, at the limit of %d live claims.", len(claimed), skipped, maxClaims)
		out["message"] = strings.TrimSpace(out["message"].(string) + " " + note)
	}
	if text, _ := out["message"].(string); text != "" {
		out["message"] = "Shell edit detected. " + text + " Iris detects these changes and cannot undo them."
	}
	return out, nil
}

// shellBaseline is what an Execution's next snapshot is compared with: a full one from
// ShellSnapshot, whose scope key is empty, or a reduced one with its scope key. Writing one kind
// deletes the other, and a start clears the reduced kind, so an Execution has at most one.
func (t *transaction) shellBaseline(id string) (map[string]string, string, bool, error) {
	var raw, scope string
	err := t.row("SELECT status_json,'' FROM ShellSnapshot WHERE execution_id=? UNION ALL SELECT status_json,scope_json FROM ReducedShellSnapshot WHERE execution_id=?", id, id).Scan(&raw, &scope)
	if err == sql.ErrNoRows {
		return nil, "", false, nil
	}
	if err != nil {
		return nil, "", false, err
	}
	previous := map[string]string{}
	if err = json.Unmarshal([]byte(raw), &previous); err != nil {
		return nil, "", false, err
	}
	return previous, scope, true, nil
}

// keepShellSnapshot makes a snapshot the Execution's baseline. A full one goes to ShellSnapshot,
// which an older hub reads too; a reduced one goes to ReducedShellSnapshot, which an older hub
// never reads, so it never takes a reduced map for a full one. Each write deletes the other
// kind's row in the same transaction.
func (t *transaction) keepShellSnapshot(id string, status map[string]string, scope *ShellScope) error {
	encoded, _ := json.Marshal(status) // a map of strings is always JSON encodable
	drop, keep, args := "ReducedShellSnapshot", "INSERT INTO ShellSnapshot VALUES(?,?) ON CONFLICT(execution_id) DO UPDATE SET status_json=excluded.status_json", []any{id, string(encoded)}
	if scope != nil {
		drop, keep, args = "ShellSnapshot", "INSERT INTO ReducedShellSnapshot VALUES(?,?,?) ON CONFLICT(execution_id) DO UPDATE SET status_json=excluded.status_json,scope_json=excluded.scope_json", []any{id, string(encoded), scopeKey(scope)}
	}
	if _, err := t.exec("DELETE FROM "+drop+" WHERE execution_id=?", id); err != nil {
		return err
	}
	_, err := t.exec(keep, args...)
	return err
}

// maxClaims is claimLimit's bound on the live claims of one Execution.
const maxClaims = 500

// releaseLeft releases e's own auto claims on paths that left its status. Both ends of a staged
// rename stay in the status, so neither is released before the rename is committed.
func (t *transaction) releaseLeft(e execution, paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	policy, _, err := t.policy(e.Repo)
	if err != nil {
		return err
	}
	cs, err := t.claimRows(e.Repo)
	if err != nil {
		return err
	}
	now := t.s.clock.Now().UnixMilli()
	released := false
	for _, p := range paths {
		key := pathKey(p, policy)
		for _, c := range cs {
			if c.Execution != e.ID || c.Origin != "auto" || c.Kind != "exact" || c.Key != key {
				continue
			}
			if _, err = t.exec("UPDATE Claim SET revoked_ms=? WHERE fence=? AND revoked_ms IS NULL", now, c.Fence); err != nil {
				return err
			}
			released = true
		}
	}
	if !released {
		return nil
	}
	return t.released(e.Repo, e.ID)
}

// autoClaim gives e an automatic claim on each path no other Execution holds, as touch does,
// and renews e's own automatic claims, up to the claim limit. It returns the paths e now holds
// and how many it could not claim.
func (t *transaction) autoClaim(e execution, paths []string) ([]string, int, error) {
	policy, _, err := t.policy(e.Repo)
	if err != nil {
		return nil, 0, err
	}
	cs, err := t.claimRows(e.Repo)
	if err != nil {
		return nil, 0, err
	}
	expiry := t.s.clock.Now().Add(10 * time.Minute).UnixMilli()
	claimed := []string{}
	var fresh []string
	for _, p := range paths {
		key := pathKey(p, policy)
		if defaultIgnored(key) {
			continue
		}
		held, mine := false, false
		for _, c := range cs {
			if !overlaps(key, "exact", c.Key, c.Kind) {
				continue
			}
			if c.Execution == e.ID {
				mine = true
				if c.Origin == "auto" {
					if _, err = t.exec("UPDATE Claim SET expires_ms=? WHERE fence=?", expiry, c.Fence); err != nil {
						return nil, 0, err
					}
				}
				continue
			}
			family, err := t.related(e.ID, c.Execution)
			if err != nil {
				return nil, 0, err
			}
			held = held || !family
		}
		switch {
		case held:
		case mine:
			claimed = append(claimed, p)
		default:
			fresh = append(fresh, p)
		}
	}
	var live int
	if err = t.row("SELECT COUNT(*) FROM Claim WHERE execution_id=? AND revoked_ms IS NULL", e.ID).Scan(&live); err != nil {
		return nil, 0, err
	}
	skipped := 0
	if room := max(maxClaims-live, 0); len(fresh) > room {
		skipped = len(fresh) - room
		fresh = fresh[:room]
	}
	for _, p := range fresh {
		if _, err = t.exec("INSERT INTO Claim(execution_id,repository_id,kind,path,path_key,origin,boot_epoch,expires_ms) VALUES(?,?,'exact',?,?,'auto',?,?)", e.ID, e.Repo, p, pathKey(p, policy), t.s.epoch, expiry); err != nil {
			return nil, 0, err
		}
		claimed = append(claimed, p)
	}
	for _, p := range claimed {
		if err = t.took(e, pathKey(p, policy)); err != nil {
			return nil, 0, err
		}
	}
	sort.Strings(claimed)
	return claimed, skipped, nil
}
