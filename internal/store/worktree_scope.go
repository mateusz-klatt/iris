package store

import "database/sql"

// worktreeScopeDisabled is the refusal for claim.auto_scope=worktree. ADR 037
// is recorded and the column exists, and the opt-in is not enabled in v0.1.14.
// A stored worktree value is treated as repository.
const worktreeScopeDisabled = "worktree claim scope is not enabled in this release"

// ambiguousWorktree is a display path no checkout has. It records that the
// observations of one repository-wide claim disagreed, so the row is not one
// checkout. listed maps it back to unknown.
const ambiguousWorktree = "\x00"

// claimLocation is the checkout a claim records. An empty key is repository-wide:
// the display path and branch are still the checkout that was observed (ADR 037).
// ambiguousWorktree means the observations disagreed and the claim stays
// repository-wide with no checkout.
type claimLocation struct {
	key, path, branch string
}

func ambiguousLocation() claimLocation {
	return claimLocation{path: ambiguousWorktree}
}

func (t *transaction) storedClaimScope(repo string) (string, error) {
	var scope string
	err := t.row("SELECT auto_claim_scope FROM Repository WHERE id=?", repo).Scan(&scope)
	return scope, err
}

// effectiveClaimScope is the scope decisions use. worktree is disabled in this
// release, so a stored worktree row is repository everywhere.
func effectiveClaimScope(stored string) string {
	if stored == "worktree" {
		return "repository"
	}
	return stored
}

func (e execution) location() claimLocation {
	return claimLocation{key: e.WorktreeKey, path: canonicalDisplayPath(e.Worktree), branch: e.Branch}
}

func (t *transaction) insertClaim(e execution, repo, kind, path, key, origin string, expiry sql.NullInt64, loc claimLocation) (int64, error) {
	res, err := t.exec(`INSERT INTO Claim(execution_id,repository_id,kind,path,path_key,origin,boot_epoch,expires_ms,worktree_key,worktree_path,branch) VALUES(?,?,?,?,?,?,?,?,NULLIF(?,''),NULLIF(?,''),NULLIF(?,''))`, e.ID, repo, kind, path, key, origin, t.s.epoch, expiry, loc.key, loc.path, loc.branch)
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId() // modernc SQLite always provides the last inserted rowid
	return id, nil
}

// renewAuto extends an automatic claim. A keyed row whose observation is the
// same checkout keeps that key and may fill in a display path or branch. An
// observation that disagrees clears the key and records ambiguity, so the
// claim stays repository-wide instead of wearing the first checkout.
func (t *transaction) renewAuto(fence, expiry int64, observed claimLocation) error {
	var storedKey, storedPath sql.NullString
	err := t.row("SELECT worktree_key,worktree_path FROM Claim WHERE fence=?", fence).Scan(&storedKey, &storedPath)
	if err != nil {
		return err
	}
	if storedPath.String == ambiguousWorktree {
		_, err = t.exec("UPDATE Claim SET expires_ms=? WHERE fence=?", expiry, fence)
		return err
	}
	if !provenanceAgrees(storedKey.String, storedPath.String, observed) {
		_, err = t.exec("UPDATE Claim SET expires_ms=?,worktree_key=NULL,worktree_path=?,branch=NULL WHERE fence=?", expiry, ambiguousWorktree, fence)
		return err
	}
	path, branch := "", ""
	if storedKey.String != "" && observed.path != "" {
		path, branch = observed.path, observed.branch
	}
	_, err = t.exec("UPDATE Claim SET expires_ms=?,worktree_path=COALESCE(NULLIF(?,''),worktree_path),branch=COALESCE(NULLIF(?,''),branch) WHERE fence=?", expiry, path, branch, fence)
	return err
}

// provenanceAgrees is whether observed is the checkout a claim already records.
// A legacy row records none, so the first observation does not disagree with it.
func provenanceAgrees(storedKey, storedPath string, observed claimLocation) bool {
	if storedKey != "" {
		return storedKey == observed.key
	}
	if storedPath == "" {
		return true
	}
	return worktreePathsEqual(storedPath, observed.path)
}

// sameProvenance is whether two observations of one path are the same checkout.
// A known key matches only that key. Two observations with no key match only
// when the display paths do.
func sameProvenance(a, b claimLocation) bool {
	if a.key != b.key {
		return false
	}
	if a.key != "" {
		return true
	}
	return a.path != ambiguousWorktree && b.path != ambiguousWorktree && worktreePathsEqual(a.path, b.path)
}
