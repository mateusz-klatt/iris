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
	Worktree   string `json:"worktree"`
	WorktreeID string `json:"worktree_id,omitempty"`
	CommonDir  string `json:"common_dir,omitempty"`
	RemoteURL  string `json:"remote_url,omitempty"`
	RootCommit string `json:"root_commit,omitempty"`
	// Branch is the checkout's branch when the client observed one. An older client
	// omits it; the claim then says the branch is unknown rather than borrowing the
	// session's branch (ADR 037).
	Branch string   `json:"branch,omitempty"`
	Paths  []string `json:"paths"`
	// Prefix is where an explicit claim's checkout lies in the session's worktree; the claim's path
	// is relative to the session's repository and starts with it.
	Prefix string `json:"prefix,omitempty"`
}

// filePlace is one repository's share of a grouped touch.
type filePlace struct {
	repo, policy, action string
	scope                string
	loc                  claimLocation
	paths, ignored       []string
	reuse                map[string]claim
	advisories           []map[string]any
}

// touchFiles is touch for a tool call whose paths are grouped by the checkout each file lies in
// (ADR 033). Every group is placed in its own repository, and the whole call is still one
// transaction: every missing claim is granted, or, on any conflict, none is.
func (t *transaction) touchFiles(r Request) (map[string]any, error) {
	e, err := t.live(r.ExecutionID, r.Generation)
	if err != nil {
		return nil, err
	}
	if e.Outside {
		return nil, noClaims()
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
	byPlace := map[string]*filePlace{}
	scopeOf := map[string]string{}
	unclaimed := []string{}
	seen := map[string]bool{}
	for _, g := range r.Groups {
		repo, worktree, err := t.fileRepository(e, g)
		if err != nil {
			return nil, err
		}
		if repo == "" {
			unclaimed = append(unclaimed, g.Worktree)
			continue
		}
		scope, ok := scopeOf[repo]
		if !ok {
			scope, err = t.claimScope(repo)
			if err != nil {
				return nil, err
			}
			scopeOf[repo] = scope
		}
		// The group's own checkout, not the session's. fileRepository folds an alias
		// to the path this host first named and returns that path in display form.
		// An omitted WorktreeID has no key, and that claim stays repository-wide.
		// The branch is the group's, or unknown when an older client left it out.
		loc := claimLocation{path: worktree, branch: g.Branch}
		if g.WorktreeID != "" {
			loc.key, err = t.ensureWorktreeKey(e.Host, g.WorktreeID)
			if err != nil {
				return nil, err
			}
		}
		// Under repository, equal paths across groups stay one claim. Under worktree,
		// /wt1, /wt2 and a submodule of the same path are different claims.
		id := repo
		seenKey := repo + "\x00"
		if scope == "worktree" {
			id += "\x00" + loc.key
			seenKey = id + "\x00"
		}
		p := byPlace[id]
		if p == nil {
			policy, action, err := t.policy(repo)
			if err != nil {
				return nil, err
			}
			p = &filePlace{repo: repo, policy: policy, action: action, scope: scope, loc: loc, reuse: map[string]claim{}}
			byPlace[id] = p
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
			if seen[seenKey+k] {
				continue
			}
			seen[seenKey+k] = true
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
		// An advisory on another path does not split or weaken that refusal.
	} else {
		if out["granted"], err = t.grantFiles(e, places); err != nil {
			return nil, err
		}
		var advisories []map[string]any
		for _, p := range places {
			advisories = append(advisories, p.advisories...)
		}
		if len(advisories) > 0 {
			out["advisories"] = advisories
			notes = append([]string{advisoryMessage(advisories)}, notes...)
		}
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

// fileRepository places one group's checkout and returns the display path to store.
// The Execution's own worktree is its repository. Another checkout is resolved by the
// registration rules (D11, ADR 012), never from the session. One that matches no
// repository allocates one, as a session started there would, so a later session in
// that checkout meets the claim. An ambiguous one answers "": allocating there could
// only add one more candidate (ADR 032).
//
// The path is the one this host first named for the directory, then the display form:
// slashes, no trailing separator, and an upper-case Windows drive letter. A later
// spelling of the same directory, including a different case, does not replace it.
// The comparison that places the checkout still uses the alias path, not the display form.
func (t *transaction) fileRepository(e execution, g ClaimGroup) (string, string, error) {
	if g.Worktree == "" {
		return "", "", fail("bad_request", "every group needs its worktree")
	}
	r := Request{Host: e.Host, Cwd: g.Worktree, Worktree: g.Worktree, WorktreeID: g.WorktreeID, CommonDir: g.CommonDir, RemoteURL: g.RemoteURL, RootCommit: g.RootCommit}
	if err := t.canonicalWorktree(&r); err != nil {
		return "", "", err
	}
	shown := canonicalDisplayPath(r.Worktree)
	if sameWorktree(r.Worktree, e.Worktree) {
		// The Execution's checkout still has to be the history its repository records, as a
		// registration from there checks: an event from elsewhere did not check it.
		var root string
		if err := t.row("SELECT root_commit FROM Repository WHERE id=?", e.Repo).Scan(&root); err != nil {
			return "", "", err
		}
		if root != "" && r.RootCommit != "" && root != r.RootCommit {
			return "", "", fail("conflict", "this active Execution belongs to another repository history; end the session or ask the owner to repair the split with iris admin merge")
		}
		// It is the Execution's own worktree, so its repository learns from it as a registration
		// from there would (ADR 032), without redirecting any checkout continuity.
		return e.Repo, shown, t.rememberRepository(r, e.Repo)
	}
	repo, ambiguous, err := t.resolveRepository(r)
	if err != nil || (repo == "" && ambiguous) {
		return "", "", err
	}
	if repo == "" {
		repo, err = t.repository(r)
		return repo, shown, err
	}
	return repo, shown, t.adoptRepository(r, repo)
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
				if reusesOwn(p.scope, c.Origin, c.WorktreeKey, p.loc.key) {
					if i < len(p.paths) {
						p.reuse[path] = c
					}
					continue
				}
				if exemptAutoOverlap(p.scope, c.Origin, c.WorktreeKey, "auto", p.loc.key) {
					p.advisories = append(p.advisories, advisoryOf(c, path, p.repo))
					continue
				}
			} else {
				family, err := t.related(e.ID, c.Execution)
				if err != nil {
					return false, err
				}
				if family {
					continue
				}
				if exemptAutoOverlap(p.scope, c.Origin, c.WorktreeKey, "auto", p.loc.key) {
					p.advisories = append(p.advisories, advisoryOf(c, path, p.repo))
					continue
				}
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
			// An overlap with this Execution's own other checkout does not wait on itself.
			if c.Execution != e.ID {
				if err = t.wait(e, p.repo, k, "exact", p.loc.key); err != nil {
					return false, err
				}
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
				fence, err := t.insertClaim(e, p.repo, "exact", path, k, "auto", sql.NullInt64{Int64: expiry, Valid: true}, p.loc)
				if err != nil {
					return nil, err
				}
				c.Fence = fence
			} else if c.Origin == "auto" {
				if err := t.renewAuto(c.Fence, expiry, p.loc.path, p.loc.branch); err != nil {
					return nil, err
				}
			}
			if err := t.took(e, p.repo, k, p.loc.key); err != nil {
				return nil, err
			}
			granted = append(granted, map[string]any{"repository_id": p.repo, "path": path, "fence": c.Fence})
		}
	}
	return granted, nil
}

// stillHeld refuses the replay of an allowed grouped touch whose claims have since expired or
// been released: a replay cannot grant permission again. A grant is looked up by its fence first:
// that claim row stays, revoked or not, and an owner's merge moves it to the repository the grant
// now lies in. A grant still holds when e has a live claim of the current epoch on the same path in
// that repository, which a recovery keeps while it renumbers fences (D10). A claim of the same
// name in another repository is another file and never counts.
func (t *transaction) stillHeld(e execution, cached map[string]any) error {
	if cached["action"] != "allow" {
		return nil
	}
	for _, g := range cached["granted"].([]any) {
		grant := g.(map[string]any)
		repo := grant["repository_id"].(string)
		// A missing fence is an empty key: only an explicit claim or a null-key claim
		// of this Execution still counts. The fence's own key is what a grouped grant
		// promised, not the Execution's current checkout (ADR 037).
		var origin, grantKey string
		if err := t.row("SELECT repository_id,origin,COALESCE(worktree_key,'') FROM Claim WHERE fence=? AND execution_id=?", grant["fence"], e.ID).Scan(&repo, &origin, &grantKey); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var policy string
		if err := t.row("SELECT case_policy FROM Repository WHERE id=?", repo).Scan(&policy); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		scope, err := t.claimScope(repo)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		k := pathKey(grant["path"].(string), policy)
		var n int
		if err := t.row("SELECT COUNT(*) FROM Claim WHERE execution_id=? AND repository_id=? AND revoked_ms IS NULL AND boot_epoch=? AND (path_key=? OR (kind='prefix' AND (path_key='' OR substr(?,1,length(path_key)+1)=path_key||'/'))) AND (?<>'worktree' OR origin='explicit' OR COALESCE(worktree_key,'')=?)", e.ID, repo, t.s.epoch, k, k, scope, grantKey).Scan(&n); err != nil {
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

// sameWorktree is whether two worktree paths spell one directory. A path with a drive letter is a
// Windows one: its separators, a trailing separator and its case are folded, as Windows reads it.
// A POSIX path loses only a trailing slash, and keeps its case and any backslash, which there is a
// character of a name.
func sameWorktree(a, b string) bool {
	windows := func(p string) bool { return len(p) >= 2 && p[1] == ':' }
	if windows(a) || windows(b) {
		fold := func(p string) string { return strings.TrimRight(strings.ReplaceAll(p, "\\", "/"), "/") }
		return strings.EqualFold(fold(a), fold(b))
	}
	return strings.TrimRight(a, "/") == strings.TrimRight(b, "/")
}

// win32Path drops the trailing dots and spaces that Win32 removes from each name of a path. A name
// made only of them is left alone, as Win32 does not reduce it to nothing.
func win32Path(p string) string {
	parts := strings.Split(p, "/")
	for i, part := range parts {
		if t := strings.TrimRight(part, ". "); t != "" {
			parts[i] = t
		}
	}
	return strings.Join(parts, "/")
}
