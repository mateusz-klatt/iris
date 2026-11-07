package store

import (
	"database/sql"
	"encoding/json"
	"sort"
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
	if text, _ := out["message"].(string); text != "" {
		out["message"] = "Shell edit detected. " + text + " Iris detects these changes and cannot undo them."
	}
	return out, nil
}
