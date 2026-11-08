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
	repo, err := t.repository(r)
	if err != nil {
		return nil, err
	}
	return t.checkPaths(repo, r.Host, r.Worktree, r.Paths)
}

func (t *transaction) checkPaths(repo, host, worktree string, paths []string) (map[string]any, error) {
	policy, _, err := t.policy(repo)
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
	// that folds case works in it (ADR 003).
	return map[string]any{"message": message, "conflicts": conflicts, "case_policy": policy}, nil
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
