package store

import (
	"fmt"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// pathKey is what claims compare. path stays as written, for display. A folded key is the
// Unicode canonical caseless match, because APFS treats far more names as equal than
// strings.EqualFold does: ß and SS, ligatures, and NFC and NFD forms of one name.
func pathKey(p, policy string) string {
	if policy != "folded" {
		return p
	}
	return norm.NFD.String(cases.Fold().String(norm.NFD.String(p)))
}

// rekeyRepository follows a change of case policy.
func (t *transaction) rekeyRepository(repo string) error {
	return t.rekey("c.repository_id=?", repo)
}

// fillMissingKeys runs at every start, not only at the migration: a binary from before the
// key writes claims without one if the hub is rolled back, and an empty key on a prefix claim
// would read as repository-wide. Only a repository-wide claim really has an empty path.
func (t *transaction) fillMissingKeys() error {
	return t.rekey("c.path_key='' AND c.path<>''")
}

// rekey recomputes keys. Revoked claims are included because recovery reads the same rows.
func (t *transaction) rekey(where string, args ...any) error {
	rows, err := t.rows("SELECT c.fence,c.path,r.case_policy FROM Claim c JOIN Repository r ON r.id=c.repository_id WHERE "+where, args...)
	if err != nil {
		return err
	}
	type entry struct {
		fence int64
		key   string
	}
	var entries []entry
	for rows.Next() {
		var e entry
		var p, policy string
		rows.Scan(&e.fence, &p, &policy)
		e.key = pathKey(p, policy)
		entries = append(entries, e)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, e := range entries {
		if _, err = t.exec("UPDATE Claim SET path_key=? WHERE fence=?", e.key, e.fence); err != nil {
			return err
		}
	}
	return nil
}

// heldBy names both spellings when they differ, so that a refused agent can see why a path
// it did not write the same way is taken.
func heldBy(c map[string]any) string {
	if c["path"] == c["claimed_path"] {
		return fmt.Sprintf("%s is held by %s", c["path"], c["holder"])
	}
	return fmt.Sprintf("%s is held by %s as %s", c["path"], c["holder"], c["claimed_path"])
}
