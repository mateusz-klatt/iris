package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// shellSession is whether a shell reconcile names its session rather than an Execution.
func shellSession(operation string, r Request) bool {
	return operation == "reconcile" && r.ExecutionID == "" && r.SessionID != ""
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
	out["session"] = map[string]any{"execution_id": r.ExecutionID, "generation": r.Generation}
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
		_, err = t.exec("DELETE FROM ShellSnapshot WHERE execution_id IN (SELECT id FROM Execution WHERE repository_id=? AND host_id=? AND worktree=?)", e.Repo, e.Host, e.Worktree)
		return empty, err
	}
	for p := range r.Status {
		if _, err = cleanPath(p, "exact", false); err != nil {
			return nil, err
		}
	}
	previous := map[string]string{}
	var raw string
	err = t.row("SELECT status_json FROM ShellSnapshot WHERE execution_id=?", e.ID).Scan(&raw)
	found := err == nil
	if err != nil && err != sql.ErrNoRows {
		return nil, err
	}
	if found {
		if err = json.Unmarshal([]byte(raw), &previous); err != nil {
			return nil, err
		}
	}
	encoded, _ := json.Marshal(r.Status) // a map of strings is always JSON encodable
	if _, err = t.exec("INSERT INTO ShellSnapshot VALUES(?,?) ON CONFLICT(execution_id) DO UPDATE SET status_json=excluded.status_json", e.ID, string(encoded)); err != nil {
		return nil, err
	}
	if r.Kind == "before" || !found {
		return empty, nil
	}
	// Only paths still changed after the command count. One that left the status was committed,
	// stashed or restored by git in this worktree, which changes nothing in a holder's worktree,
	// so it is neither warned about nor claimed.
	changed := []string{}
	for p, status := range r.Status {
		if previous[p] != status {
			changed = append(changed, p)
		}
	}
	sort.Strings(changed)
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

// maxClaims is claimLimit's bound on the live claims of one Execution.
const maxClaims = 500

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
