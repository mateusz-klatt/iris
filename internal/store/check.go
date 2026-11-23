package store

import (
	"fmt"

	"github.com/mateusz-klatt/iris/internal/display"
)

func (t *transaction) check(r Request) (map[string]any, error) {
	if r.Host == "" || r.Worktree == "" {
		return nil, fail("bad_request", "host and worktree are required")
	}
	// A commit from another path to the same directory is from the same worktree (D6).
	if err := t.canonicalWorktree(&r); err != nil {
		return nil, err
	}
	// The guard never allocates a repository (ADR 032). One it allocated would hold no claims,
	// so its answer would read as a check that found no conflicts, and it would be one more
	// candidate for the next checkout there. A checkout it cannot place is said to be so.
	repo, ambiguous, err := t.resolveRepository(r)
	if err != nil {
		return nil, err
	}
	if ambiguous && repo == "" {
		return nil, fail("ambiguous", "the hub cannot tell which repository this checkout belongs to, so its claims were not checked; ask the owner to repair the split with iris admin merge")
	}
	if repo == "" {
		return nil, fail("not_found", "no repository on the hub is known for this checkout yet, so there are no claims to check it against")
	}
	if err = t.adoptRepository(r, repo); err != nil {
		return nil, err
	}
	// Record the committing checkout's key. The comparison below is the holder's
	// host and worktree, as v0.1.13 did: the worktree opt-in is not enabled.
	if _, err = t.ensureWorktreeKey(r.Host, r.WorktreeID); err != nil {
		return nil, err
	}
	out, err := t.checkPaths(repo, r.Host, r.Worktree, r.Paths)
	if err != nil {
		return nil, err
	}
	if conflicts := out["conflicts"].([]map[string]any); len(conflicts) > 0 {
		c := conflicts[0]
		// Checkout identity cannot establish who started git commit. Keep the refusal even
		// when the holder might be the same session working from another checkout.
		out["message"] = fmt.Sprintf("Iris: %s %s until %v (%s claim). The guard cannot identify the committing session. If this is your session in another worktree, call the release tool with {\"fence\":%v} from that session, then retry the commit. Otherwise message the holder with the send tool, or ask the human to help resolve the claim.", heldBy(c), where(c), c["expires_at"], c["origin"], c["fence"])
	}
	return out, nil
}

// checkPaths classifies staged or shell paths. The guard and the shell share it.
// Same checkout is the holder Execution's host and worktree, so a parent or child
// in another checkout still warns, as it did in v0.1.13. The worktree opt-in is
// not enabled, so a stored worktree value does not change that.
func (t *transaction) checkPaths(repo, host, worktree string, paths []string) (map[string]any, error) {
	policy, _, err := t.policy(repo)
	if err != nil {
		return nil, err
	}
	stored, err := t.storedClaimScope(repo)
	if err != nil {
		return nil, err
	}
	claims, err := t.claimRows(repo)
	if err != nil {
		return nil, err
	}
	conflicts := []map[string]any{}
	for _, raw := range paths {
		p, err := cleanPath(raw, "exact", false)
		if err != nil {
			return nil, err
		}
		key := pathKey(p, policy)
		for _, c := range claims {
			if !overlaps(key, "exact", c.Key, c.Kind) {
				continue
			}
			var ch, cw, cb string
			if err = t.row("SELECT host_id,worktree,branch FROM Execution WHERE id=?", c.Execution).Scan(&ch, &cw, &cb); err != nil {
				return nil, err
			}
			if ch == host && cw == worktree {
				continue
			}
			conflict, err := t.conflict(c)
			if err != nil {
				return nil, err
			}
			conflict["path"] = p
			conflict["branch"], conflict["worktree"], conflict["host"] = cb, cw, ch
			conflicts = append(conflicts, conflict)
		}
	}
	message := ""
	if len(conflicts) > 0 {
		c := conflicts[0]
		message = fmt.Sprintf("Iris: %s %s until %v (%s claim). Message them with the send tool, or work on something else. Ask the human if you need help resolving this conflict.", heldBy(c), where(c), c["expires_at"], c["origin"])
	}
	// case_policy lets iris doctor warn when this repository is case-sensitive while a host
	// that folds case works in it (ADR 003). claim_auto_scope is the effective scope.
	// It is always present, so a missing field means a hub from before ADR 037, which
	// doctor calls unknown. A stored worktree value is repository, and the note says so.
	out := map[string]any{"message": message, "conflicts": conflicts, "case_policy": policy, "claim_auto_scope": effectiveClaimScope(stored)}
	if stored == "worktree" {
		out["claim_scope_note"] = worktreeScopeDisabled
	}
	return out, nil
}

// where says which checkout holds a claim, so the person refused knows where to ask: the
// holder's branch, when it is on one, its worktree and its host. Each value is the holder's
// own and is escaped onto one line, as heldBy's are.
func where(c map[string]any) string {
	place := fmt.Sprintf("worktree %s on %s", display.Inline(fmt.Sprint(c["worktree"])), display.Inline(fmt.Sprint(c["host"])))
	if b := fmt.Sprint(c["branch"]); b != "" {
		place = "branch " + display.Inline(b) + ", " + place
	}
	return "(" + place + ")"
}
