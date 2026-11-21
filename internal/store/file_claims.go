package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mateusz-klatt/iris/internal/display"
)

// ClaimGroup is the paths of one tool call that lie in one checkout, relative to that
// checkout's worktree, with what the hook read of the checkout (ADR 033).
type ClaimGroup struct {
	Worktree   string   `json:"worktree"`
	WorktreeID string   `json:"worktree_id,omitempty"`
	CommonDir  string   `json:"common_dir,omitempty"`
	RemoteURL  string   `json:"remote_url,omitempty"`
	RootCommit string   `json:"root_commit,omitempty"`
	Paths      []string `json:"paths"`
}

// filePlace is one repository's share of a grouped touch.
type filePlace struct {
	repo, policy, action string
	paths, ignored       []string
	reuse                map[string]claim
}

// touchFiles is touch for a tool call whose paths are grouped by the checkout each file lies in
// (ADR 033). Every group is placed in its own repository, and the whole call is still one
// transaction: every missing claim is granted, or, on any conflict, none is.
func (t *transaction) touchFiles(r Request) (map[string]any, error) {
	e, err := t.live(r.ExecutionID, r.Generation)
	if err != nil {
		return nil, err
	}
	if r.ToolCallID == "" {
		return nil, fail("bad_request", "tool_call_id is required")
	}
	if len(r.Groups) == 0 {
		return nil, fail("bad_request", "at least one group is required")
	}
	key := "touch_files:" + r.ToolCallID
	cached, found, err := t.cached(e, key, "touch_files", r)
	if err != nil {
		return nil, err
	}
	if found {
		return cached, t.stillHeld(e, cached)
	}
	var places []*filePlace
	byRepo := map[string]*filePlace{}
	unclaimed := []string{}
	seen := map[string]bool{}
	for _, g := range r.Groups {
		repo, err := t.fileRepository(e, g)
		if err != nil {
			return nil, err
		}
		if repo == "" {
			unclaimed = append(unclaimed, g.Worktree)
			continue
		}
		p := byRepo[repo]
		if p == nil {
			policy, action, err := t.policy(repo)
			if err != nil {
				return nil, err
			}
			p = &filePlace{repo: repo, policy: policy, action: action, reuse: map[string]claim{}}
			byRepo[repo] = p
			places = append(places, p)
		}
		for _, raw := range g.Paths {
			if err := refuseControl(raw); err != nil {
				return nil, err
			}
			clean, err := cleanPath(raw, "exact", false)
			if err != nil {
				return nil, err
			}
			k := pathKey(clean, p.policy)
			if seen[repo+"\x00"+k] {
				continue
			}
			seen[repo+"\x00"+k] = true
			if defaultIgnored(k) {
				p.ignored = append(p.ignored, clean)
			} else {
				p.paths = append(p.paths, clean)
			}
		}
	}
	if len(seen) == 0 && len(unclaimed) == 0 {
		return nil, fail("bad_request", "at least one path is required")
	}
	conflicts := []map[string]any{}
	action := "allow"
	for _, p := range places {
		found, err := t.fileConflicts(e, p, &conflicts)
		if err != nil {
			return nil, err
		}
		if found {
			action = stricter(action, p.action)
		}
	}
	ignored := []map[string]any{}
	for _, p := range places {
		for _, path := range p.ignored {
			ignored = append(ignored, map[string]any{"repository_id": p.repo, "path": path})
		}
	}
	out := map[string]any{"action": "allow", "granted": []map[string]any{}, "ignored": ignored, "conflicts": conflicts, "unclaimed": unclaimed, "message": ""}
	var notes []string
	if len(ignored) > 0 {
		notes = append(notes, "Iris: ignored paths are advisory only and did not take automatic claims.")
	}
	if len(conflicts) > 0 {
		if action == "ask" && e.Client != "claude" {
			action = "deny"
		}
		out["action"] = action
		c := conflicts[0]
		place, err := t.elsewhere(e, c)
		if err != nil {
			return nil, err
		}
		text := fmt.Sprintf("Iris: %s%s until %v (%s claim). Message them with the send tool, or work on something else. Do not bypass this with a shell command.", heldBy(c), place, c["expires_at"], c["origin"])
		if action == "deny" {
			text += " Ask the human if you need help resolving this conflict."
		}
		notes = append([]string{text}, notes...)
		// No paths are granted on any conflict, even when policy permits proceeding.
	} else if out["granted"], err = t.grantFiles(e, places); err != nil {
		return nil, err
	}
	for _, w := range unclaimed {
		notes = append(notes, fmt.Sprintf("Iris: the hub cannot tell which repository %s belongs to, so its paths were not claimed; ask the owner to repair the split with iris admin merge.", display.Inline(w)))
	}
	out["message"] = strings.Join(notes, " ")
	if out["attention"], err = t.attention(e); err != nil {
		return nil, err
	}
	if err = t.remember(e, key, "touch_files", r, out); err != nil {
		return nil, err
	}
	return out, nil
}

// fileRepository places one group's checkout. The Execution's own worktree is its repository.
// Another checkout is resolved by the registration rules (D11, ADR 012), never from the session.
// One that matches no repository allocates one, as a session started there would, so a later
// session in that checkout meets the claim. An ambiguous one answers "": allocating there could
// only add one more candidate (ADR 032).
func (t *transaction) fileRepository(e execution, g ClaimGroup) (string, error) {
	if g.Worktree == "" {
		return "", fail("bad_request", "every group needs its worktree")
	}
	r := Request{Host: e.Host, Cwd: g.Worktree, Worktree: g.Worktree, WorktreeID: g.WorktreeID, CommonDir: g.CommonDir, RemoteURL: g.RemoteURL, RootCommit: g.RootCommit}
	if err := t.canonicalWorktree(&r); err != nil {
		return "", err
	}
	if sameWorktree(r.Worktree, e.Worktree) {
		// The Execution's checkout still has to be the history its repository records, as a
		// registration from there checks: an event from elsewhere did not check it.
		var root string
		if err := t.row("SELECT root_commit FROM Repository WHERE id=?", e.Repo).Scan(&root); err != nil {
			return "", err
		}
		if root != "" && r.RootCommit != "" && root != r.RootCommit {
			return "", fail("conflict", "this active Execution belongs to another repository history; end the session or ask the owner to repair the split with iris admin merge")
		}
		return e.Repo, nil
	}
	repo, ambiguous, err := t.resolveRepository(r)
	if err != nil || (repo == "" && ambiguous) {
		return "", err
	}
	if repo == "" {
		return t.repository(r)
	}
	return repo, t.adoptRepository(r, repo)
}

// fileConflicts adds every conflict of p's paths to conflicts, and records that e waits for
// each conflicting path in p's repository. It reports whether p had one.
func (t *transaction) fileConflicts(e execution, p *filePlace, conflicts *[]map[string]any) (bool, error) {
	cs, err := t.claimRows(p.repo)
	if err != nil {
		return false, err
	}
	found := false
	// Ignored paths take no automatic claim, but another session's explicit claim on one
	// still applies (D10), so every requested path is checked for conflicts.
	for i, path := range append(append([]string{}, p.paths...), p.ignored...) {
		k := pathKey(path, p.policy)
		for _, c := range cs {
			if !overlaps(k, "exact", c.Key, c.Kind) {
				continue
			}
			if c.Execution == e.ID {
				if i < len(p.paths) {
					p.reuse[path] = c
				}
				continue
			}
			family, err := t.related(e.ID, c.Execution)
			if err != nil {
				return false, err
			}
			if family {
				continue
			}
			v, err := t.conflict(c)
			if err != nil {
				return false, err
			}
			v["path"] = path
			v["repository_id"] = p.repo
			*conflicts = append(*conflicts, v)
			found = true
			// The holder's release of this path is what this session waits for.
			if err = t.wait(e, p.repo, k, "exact"); err != nil {
				return false, err
			}
		}
	}
	return found, nil
}

// grantFiles takes or renews every path of places for e, within the claim limit, and returns
// them with their repositories.
func (t *transaction) grantFiles(e execution, places []*filePlace) ([]map[string]any, error) {
	fresh := 0
	for _, p := range places {
		fresh += len(p.paths) - len(p.reuse)
	}
	if err := t.claimLimit(e, fresh); err != nil {
		return nil, err
	}
	granted := []map[string]any{}
	expiry := t.s.clock.Now().Add(10 * time.Minute).UnixMilli()
	for _, p := range places {
		for _, path := range p.paths {
			k := pathKey(path, p.policy)
			c, ok := p.reuse[path]
			if !ok {
				res, err := t.exec("INSERT INTO Claim(execution_id,repository_id,kind,path,path_key,origin,boot_epoch,expires_ms) VALUES(?,?,'exact',?,?,'auto',?,?)", e.ID, p.repo, path, k, t.s.epoch, expiry)
				if err != nil {
					return nil, err
				}
				c.Fence, _ = res.LastInsertId() // SQLite always reports the rowid it assigned
			} else if c.Origin == "auto" {
				if _, err := t.exec("UPDATE Claim SET expires_ms=? WHERE fence=?", expiry, c.Fence); err != nil {
					return nil, err
				}
			}
			if err := t.took(e, p.repo, k); err != nil {
				return nil, err
			}
			granted = append(granted, map[string]any{"repository_id": p.repo, "path": path, "fence": c.Fence})
		}
	}
	return granted, nil
}

// stillHeld refuses the replay of an allowed grouped touch whose claims have since expired or
// been released: a replay cannot grant permission again. A grant still holds when e has a live
// claim of the current epoch that is either the very claim, by its fence, which an owner's merge
// of the repository keeps, or one on the same path in the same repository, which is what a
// recovery after a restart or a restore keeps while it renumbers fences (D10). A claim of the same
// name in another repository is another file and never counts.
func (t *transaction) stillHeld(e execution, cached map[string]any) error {
	if cached["action"] != "allow" {
		return nil
	}
	for _, g := range cached["granted"].([]any) {
		grant := g.(map[string]any)
		var policy string
		if err := t.row("SELECT case_policy FROM Repository WHERE id=?", grant["repository_id"]).Scan(&policy); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		k := pathKey(grant["path"].(string), policy)
		var n int
		if err := t.row("SELECT COUNT(*) FROM Claim WHERE execution_id=? AND revoked_ms IS NULL AND boot_epoch=? AND (fence=? OR (repository_id=? AND (path_key=? OR (kind='prefix' AND (path_key='' OR substr(?,1,length(path_key)+1)=path_key||'/')))))", e.ID, t.s.epoch, grant["fence"], grant["repository_id"], k, k).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			return fail("lost", "cached touch no longer owns the path; use a new tool call")
		}
	}
	return nil
}

// elsewhere names the repository of a conflict outside e's own, whose path is relative to a
// checkout the session may know by another name, such as a submodule's directory.
func (t *transaction) elsewhere(e execution, c map[string]any) (string, error) {
	repo := c["repository_id"].(string)
	if repo == e.Repo {
		return "", nil
	}
	var name string
	err := t.row("SELECT name FROM Repository WHERE id=?", repo).Scan(&name)
	return " in repository " + display.Inline(name), err
}

// stricter is the more restrictive of two conflict actions: deny, then ask, then warn.
func stricter(a, b string) string {
	rank := map[string]int{"allow": 0, "warn": 1, "ask": 2, "deny": 3}
	if rank[b] > rank[a] {
		return b
	}
	return a
}

// sameWorktree is whether two worktree paths spell one directory: separators and a trailing slash
// aside and, with a drive letter, without regard to case, as Windows reads them. A POSIX path stays
// case-sensitive.
func sameWorktree(a, b string) bool {
	a = strings.TrimRight(strings.ReplaceAll(a, "\\", "/"), "/")
	b = strings.TrimRight(strings.ReplaceAll(b, "\\", "/"), "/")
	if len(a) >= 2 && a[1] == ':' {
		return strings.EqualFold(a, b)
	}
	return a == b
}
