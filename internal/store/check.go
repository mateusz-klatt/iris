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
	// The committing checkout's key. An empty WorktreeID has none, and a missing
	// key is never an exemption (ADR 037). The same-host path comparison below is
	// unchanged; step 4's guard compares keys for that exemption.
	incoming, err := t.ensureWorktreeKey(r.Host, r.WorktreeID)
	if err != nil {
		return nil, err
	}
	out, err := t.checkPaths(repo, r.Host, r.Worktree, incoming, r.Paths)
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

func (t *transaction) checkPaths(repo, host, worktree, incomingKey string, paths []string) (map[string]any, error) {
	policy, _, err := t.policy(repo)
	if err != nil {
		return nil, err
	}
	scope, err := t.claimScope(repo)
	if err != nil {
		return nil, err
	}
	claims, err := t.claimRows(repo)
	if err != nil {
		return nil, err
	}
	conflicts := []map[string]any{}
	advisories := []map[string]any{}
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
			// Same checkout is not a conflict and not an advisory. Under repository
			// that is the holder Execution's host and worktree. Under worktree it
			// is the claim key, then the explicit display path, then a legacy
			// row's Execution path (ADR 037). An empty incoming key never takes
			// the cross-worktree exemption.
			if sameGuardWorktree(scope, c, incomingKey, host, worktree, ch, cw) {
				continue
			}
			if exemptAutoOverlap(scope, c.Origin, c.WorktreeKey, "auto", incomingKey) {
				advisories = append(advisories, advisoryOf(c, p, repo))
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
	// that folds case works in it (ADR 003). advisories is omitted when empty so a check
	// with no exemption keeps today's keys. The shell renders them into message later;
	// the guard keeps message empty for an advisory-only result.
	out := map[string]any{"message": message, "conflicts": conflicts, "case_policy": policy}
	if len(advisories) > 0 {
		out["advisories"] = advisories
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
