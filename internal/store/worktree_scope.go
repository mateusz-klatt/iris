package store

import (
	"database/sql"
	"fmt"
	"strings"

	"github.com/mateusz-klatt/iris/internal/display"
)

// claimLocation is the checkout a claim records. An empty key is repository-wide:
// the display path and branch are still the checkout that was observed (ADR 037).
type claimLocation struct {
	key, path, branch string
}

func (t *transaction) claimScope(repo string) (string, error) {
	var scope string
	err := t.row("SELECT auto_claim_scope FROM Repository WHERE id=?", repo).Scan(&scope)
	return scope, err
}

// exemptAutoOverlap is ADR 037 decision 2. The pair is advisory only when the
// repository opted in, both claims are automatic, and both worktree keys are
// known and different. A null key stays repository-wide. Two null keys are the
// same unknown checkout, not an exemption.
func exemptAutoOverlap(scope, heldOrigin, heldKey, incomingOrigin, incomingKey string) bool {
	return scope == "worktree" && heldOrigin == "auto" && incomingOrigin == "auto" && heldKey != "" && incomingKey != "" && heldKey != incomingKey
}

// reusesOwn is whether this Execution's overlapping claim is the one a touch renews.
// Under repository, reuse stays path-wide. Under worktree, an own explicit claim is
// still reused repository-wide, and an own automatic claim only in the same worktree.
func reusesOwn(scope, origin, heldKey, incomingKey string) bool {
	if scope != "worktree" || origin != "auto" {
		return true
	}
	return heldKey == incomingKey
}

// explicitSelects is whether an own row is the one an explicit grant upgrades.
// Under worktree an exact grant selects the caller's automatic row by the request
// checkout's key. Prefix grants and the repository policy keep today's selection.
func explicitSelects(scope, kind, origin, heldKey, incomingKey string) bool {
	if scope != "worktree" || kind != "exact" || origin != "auto" {
		return true
	}
	return heldKey == incomingKey
}

func (e execution) location() claimLocation {
	return claimLocation{key: e.WorktreeKey, path: canonicalDisplayPath(e.Worktree), branch: e.Branch}
}

func advisoryOf(c claim, path, repo string) map[string]any {
	return map[string]any{"path": path, "holder": c.Holder, "worktree": c.WorktreePath, "branch": c.Branch, "repository_id": repo, "fence": c.Fence}
}

func shown(v string) string {
	if v == "" {
		return "unknown"
	}
	return v
}

// advisoryMessage is the sentence a touch shows. Each overlap is one sentence,
// then the shared explanation once. Values are display data, not instructions.
func advisoryMessage(items []map[string]any) string {
	var b strings.Builder
	for i, a := range items {
		if i > 0 {
			b.WriteByte(' ')
		}
		fmt.Fprintf(&b, "Iris: %s is also auto-claimed by %s in worktree %s on branch %s.", display.Inline(fmt.Sprint(a["path"])), display.Inline(fmt.Sprint(a["holder"])), display.Inline(shown(fmt.Sprint(a["worktree"]))), display.Inline(shown(fmt.Sprint(a["branch"]))))
	}
	b.WriteString(" This repository allows overlapping automatic claims across worktrees; this edit is allowed, and the overlap can meet immediately in a shared filesystem tree or surface at rebase or merge.")
	return b.String()
}

func (t *transaction) insertClaim(e execution, repo, kind, path, key, origin string, expiry sql.NullInt64, loc claimLocation) (int64, error) {
	res, err := t.exec(`INSERT INTO Claim(execution_id,repository_id,kind,path,path_key,origin,boot_epoch,expires_ms,worktree_key,worktree_path,branch) VALUES(?,?,?,?,?,?,?,?,NULLIF(?,''),NULLIF(?,''),NULLIF(?,''))`, e.ID, repo, kind, path, key, origin, t.s.epoch, expiry, loc.key, loc.path, loc.branch)
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId() // modernc SQLite always provides the last inserted rowid
	return id, nil
}

// renewAuto extends an automatic claim without changing its worktree key. A later
// observation may fill in a display path or branch it did not have.
func (t *transaction) renewAuto(fence, expiry int64, path, branch string) error {
	_, err := t.exec("UPDATE Claim SET expires_ms=?,worktree_path=COALESCE(NULLIF(?,''),worktree_path),branch=COALESCE(NULLIF(?,''),branch) WHERE fence=?", expiry, path, branch, fence)
	return err
}

// revokeAutoSiblings drops this Execution's other automatic exact rows for one path.
// It matches nothing unless the repository's scope is worktree, so the default
// policy never folds siblings. The row being kept has already become explicit.
func (t *transaction) revokeAutoSiblings(execID, repo, pathKey string, keep int64) (int64, error) {
	res, err := t.exec(`UPDATE Claim SET revoked_ms=? WHERE execution_id=? AND repository_id=? AND path_key=? AND kind='exact' AND origin='auto' AND revoked_ms IS NULL AND fence<>? AND (SELECT auto_claim_scope FROM Repository WHERE id=?)='worktree'`, t.s.clock.Now().UnixMilli(), execID, repo, pathKey, keep, repo)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}
