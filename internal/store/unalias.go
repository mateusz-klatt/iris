package store

import "strings"

// unalias removes exact alias mappings from one repository: the owner's repair for aliases a
// repository learned from another checkout, a submodule or another project (ADR 032). It
// removes only that repository's mapping of each alias, never another repository's mapping of
// the same spelling. A dry run reports the same answer and changes nothing.
func (t *transaction) unalias(r Request) (map[string]any, error) {
	if r.RepositoryID == "" || len(r.Aliases) == 0 {
		return nil, fail("bad_request", "repository_id and aliases are required")
	}
	var known int
	if err := t.row("SELECT COUNT(*) FROM Repository WHERE id=?", r.RepositoryID).Scan(&known); err != nil {
		return nil, err
	}
	if known == 0 {
		return nil, fail("not_found", "unknown repository")
	}
	for _, alias := range r.Aliases {
		if strings.HasPrefix(alias, outsidePrefix) {
			return nil, fail("conflict", "the marker of a host's context of sessions outside git cannot be removed (ADR 034)")
		}
	}
	removed, absent := []string{}, []string{}
	seen := map[string]bool{}
	for _, alias := range r.Aliases {
		if seen[alias] {
			continue
		}
		seen[alias] = true
		var held int
		if err := t.row("SELECT COUNT(*) FROM RepositoryAlias WHERE alias=? AND repository_id=?", alias, r.RepositoryID).Scan(&held); err != nil {
			return nil, err
		}
		if held == 0 {
			absent = append(absent, alias)
			continue
		}
		removed = append(removed, alias)
		if !r.DryRun {
			if _, err := t.exec("DELETE FROM RepositoryAlias WHERE alias=? AND repository_id=?", alias, r.RepositoryID); err != nil {
				return nil, err
			}
		}
	}
	return map[string]any{"repository_id": r.RepositoryID, "dry_run": r.DryRun, "removed": removed, "absent": absent}, nil
}
