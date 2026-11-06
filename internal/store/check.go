package store

import "fmt"

func (t *transaction) check(r Request) (map[string]any, error) {
	if r.Host == "" || r.Worktree == "" {
		return nil, fail("bad_request", "host and worktree are required")
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
		p, err := cleanPath(raw, "exact", policy, false)
		if err != nil {
			return nil, err
		}
		for _, c := range claims {
			if !overlaps(p, "exact", c.Path, c.Kind) {
				continue
			}
			var ch, cw string
			if err = t.row("SELECT host_id,worktree FROM Execution WHERE id=?", c.Execution).Scan(&ch, &cw); err != nil {
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
			conflicts = append(conflicts, conflict)
		}
	}
	message := ""
	if len(conflicts) > 0 {
		c := conflicts[0]
		message = fmt.Sprintf("Iris: %s is held by %s until %v (%s claim). Message them with the send tool, or work on something else. Ask the human if you need help resolving this conflict.", c["path"], c["holder"], c["expires_at"], c["origin"])
	}
	return map[string]any{"message": message, "conflicts": conflicts}, nil
}
