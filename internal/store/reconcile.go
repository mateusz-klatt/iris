package store

import (
	"database/sql"
	"encoding/json"
	"sort"
	"time"
)

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
	paths := []string{}
	for p, status := range r.Status {
		if previous[p] != status {
			paths = append(paths, p)
		}
	}
	for p := range previous {
		if _, exists := r.Status[p]; !exists {
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)
	out, err := t.checkPaths(e.Repo, e.Host, e.Worktree, paths)
	if err != nil {
		return nil, err
	}
	// A write a shell or Codex's js tool made is claimed for this session afterwards, as an edit
	// tool's would have been before (gap review C7). Only paths still changed are claimed: one
	// that left the status was committed or reverted. A path someone else holds stays theirs, and
	// the message above reports it.
	var changed []string
	for _, p := range paths {
		if _, still := r.Status[p]; still {
			changed = append(changed, p)
		}
	}
	claimed, err := t.autoClaim(e, changed)
	if err != nil {
		return nil, err
	}
	out["claimed"] = claimed
	if text, _ := out["message"].(string); text != "" {
		out["message"] = "Shell edit detected. " + text + " Iris detects these changes and cannot undo them."
	}
	return out, nil
}

// autoClaim gives e an automatic claim on each path no other Execution holds, as touch does,
// and renews e's own automatic claims. It returns the paths e now holds.
func (t *transaction) autoClaim(e execution, paths []string) ([]string, error) {
	policy, _, err := t.policy(e.Repo)
	if err != nil {
		return nil, err
	}
	cs, err := t.claimRows(e.Repo)
	if err != nil {
		return nil, err
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
						return nil, err
					}
				}
				continue
			}
			family, err := t.related(e.ID, c.Execution)
			if err != nil {
				return nil, err
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
	if err = t.claimLimit(e, len(fresh)); err != nil {
		return nil, err
	}
	for _, p := range fresh {
		if _, err = t.exec("INSERT INTO Claim(execution_id,repository_id,kind,path,path_key,origin,boot_epoch,expires_ms) VALUES(?,?,'exact',?,?,'auto',?,?)", e.ID, e.Repo, p, pathKey(p, policy), t.s.epoch, expiry); err != nil {
			return nil, err
		}
		claimed = append(claimed, p)
	}
	for _, p := range claimed {
		if err = t.took(e, pathKey(p, policy)); err != nil {
			return nil, err
		}
	}
	sort.Strings(claimed)
	return claimed, nil
}
