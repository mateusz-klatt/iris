package store

import (
	"context"
	"database/sql"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/mateusz-klatt/iris/internal/display"
)

type claim struct {
	Fence                            int64
	Execution, Repo, Kind, Path, Key string
	Origin, Holder                   string
	Epoch                            int64
	Expiry                           sql.NullInt64
}

func (c claim) public() map[string]any {
	return map[string]any{"fence": c.Fence, "path": c.Path, "kind": c.Kind, "holder": c.Holder, "origin": c.Origin, "expires_at": timestamp(c.Expiry)}
}
func (t *transaction) conflict(c claim) (map[string]any, error) {
	var deadline int64
	var recovery sql.NullInt64
	if err := t.row("SELECT deadline_tick_ms,recovery_deadline FROM Execution WHERE id=?", c.Execution).Scan(&deadline, &recovery); err != nil {
		return nil, err
	}
	expiry := t.s.clock.Now().UnixMilli() + deadline - t.s.clock.Tick()
	if recovery.Valid {
		expiry = recovery.Int64
	}
	if c.Expiry.Valid && c.Expiry.Int64 < expiry {
		expiry = c.Expiry.Int64
	}
	out := c.public()
	out["claimed_path"] = c.Path
	out["expires_at"] = timestamp(sql.NullInt64{Int64: expiry, Valid: true})
	return out, nil
}

func (t *transaction) claimRows(repo string) ([]claim, error) {
	rows, err := t.rows(`SELECT c.fence,c.execution_id,c.repository_id,c.kind,c.path,c.path_key,c.origin,a.name,c.boot_epoch,c.expires_ms FROM Claim c JOIN Execution e ON e.id=c.execution_id JOIN Agent a ON a.id=e.agent_id WHERE c.repository_id=? AND c.revoked_ms IS NULL AND e.state='active' ORDER BY c.fence`, repo)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []claim{}
	for rows.Next() {
		var c claim
		rows.Scan(&c.Fence, &c.Execution, &c.Repo, &c.Kind, &c.Path, &c.Key, &c.Origin, &c.Holder, &c.Epoch, &c.Expiry)
		out = append(out, c)
	}
	return out, rows.Err()
}
func (t *transaction) claims(repo string) ([]map[string]any, error) {
	cs, err := t.claimRows(repo)
	if err != nil {
		return nil, err
	}
	out := []map[string]any{}
	for _, c := range cs {
		out = append(out, c.public())
	}
	return out, nil
}

// refuseControl keeps control characters out of claims, the paths an agent names.
// A claim path reaches other models in context, and a newline in it could start a
// line that reads as Iris's own. Paths that git reports are not refused here, so a
// repository with such a file still reconciles; they are escaped where shown. Every
// C0 and C1 control character is refused, tab included, since no claim needs one.
func refuseControl(raw string) error {
	if display.HasControl(raw) {
		return fail("bad_request", "claim path must not contain control characters such as a newline")
	}
	return nil
}

func cleanPath(raw, kind string, wide bool) (string, error) {
	if strings.ContainsAny(raw, "\\\x00*?[]") || strings.HasPrefix(raw, "/") || strings.Contains(raw, ":") {
		return "", fail("bad_request", "claim path must be a relative POSIX path without globs")
	}
	for _, part := range strings.Split(raw, "/") {
		if part == ".." {
			return "", fail("bad_request", "parent traversal is not a claim path")
		}
	}
	p := path.Clean(raw)
	if p == "." {
		p = ""
	}
	if p == "" && (kind != "prefix" || !wide) {
		return "", fail("bad_request", "empty prefix needs repo_wide=true")
	}
	return p, nil
}
func overlaps(a, ak, b, bk string) bool {
	return a == b || (ak == "prefix" && (a == "" || strings.HasPrefix(b, a+"/"))) || (bk == "prefix" && (b == "" || strings.HasPrefix(a, b+"/")))
}
func defaultIgnored(p string) bool {
	base := strings.ToLower(path.Base(p))
	switch base {
	case "go.sum", "package-lock.json", "pnpm-lock.yaml", "cargo.lock", "yarn.lock", "npm-shrinkwrap.json":
		return true
	}
	if strings.HasPrefix(base, "changelog") {
		return true
	}
	for _, part := range strings.Split(p, "/") {
		switch part {
		case "generated", "dist", "build", "node_modules":
			return true
		}
	}
	return false
}
func (t *transaction) related(a, b string) (bool, error) {
	var count int
	err := t.row(`WITH RECURSIVE lineage(id,parent_id) AS (SELECT id,parent_id FROM Execution WHERE id=? UNION ALL SELECT e.id,e.parent_id FROM Execution e JOIN lineage l ON e.id=l.parent_id) SELECT COUNT(*) FROM lineage WHERE id=?`, a, b).Scan(&count)
	if err != nil || count > 0 {
		return count > 0, err
	}
	err = t.row(`WITH RECURSIVE lineage(id,parent_id) AS (SELECT id,parent_id FROM Execution WHERE id=? UNION ALL SELECT e.id,e.parent_id FROM Execution e JOIN lineage l ON e.id=l.parent_id) SELECT COUNT(*) FROM lineage WHERE id=?`, b, a).Scan(&count)
	return count > 0, err
}
func (t *transaction) policy(repo string) (casePolicy, action string, err error) {
	err = t.row("SELECT case_policy,conflict_action FROM Repository WHERE id=?", repo).Scan(&casePolicy, &action)
	return
}

// SetRepositoryPolicy configures a repository. Changing the case policy rekeys its claims.
// Two live claims that now share a key both stay: only a new claim can conflict.
func (s *Store) SetRepositoryPolicy(ctx context.Context, repo, casePolicy, action string) error {
	if casePolicy != "sensitive" && casePolicy != "folded" {
		return fail("bad_request", "invalid case policy")
	}
	if action != "deny" && action != "ask" && action != "warn" {
		return fail("bad_request", "invalid conflict action")
	}
	return s.transact(ctx, func(t *transaction) error {
		old, _, err := t.policy(repo)
		if err != nil {
			return err
		}
		if _, err = t.exec("UPDATE Repository SET case_policy=?,conflict_action=? WHERE id=?", casePolicy, action, repo); err != nil || old == casePolicy {
			return err
		}
		return t.rekeyRepository(repo)
	})
}
func (t *transaction) touch(r Request) (map[string]any, error) {
	e, err := t.live(r.ExecutionID, r.Generation)
	if err != nil {
		return nil, err
	}
	key := "touch:" + r.ToolCallID
	if r.ToolCallID == "" {
		return nil, fail("bad_request", "tool_call_id is required")
	}
	cached, found, err := t.cached(e, key, "touch", r)
	if err != nil {
		return nil, err
	}
	if found {
		// A replay cannot grant permission after the cached claims have expired or been released.
		if cached["action"] == "allow" {
			if grants, ok := cached["granted"].([]any); ok {
				policy, _, err := t.policy(e.Repo)
				if err != nil {
					return nil, err
				}
				for _, p := range grants {
					var n int
					key := pathKey(p.(string), policy)
					err = t.row("SELECT COUNT(*) FROM Claim WHERE execution_id=? AND (path_key=? OR (kind='prefix' AND (path_key='' OR substr(?,1,length(path_key)+1)=path_key||'/'))) AND revoked_ms IS NULL AND boot_epoch=?", e.ID, key, key, t.s.epoch).Scan(&n)
					if err != nil {
						return nil, err
					}
					if n == 0 {
						return nil, fail("lost", "cached touch no longer owns the path; use a new tool call")
					}
				}
			}
		}
		return cached, nil
	}
	policy, action, err := t.policy(e.Repo)
	if err != nil {
		return nil, err
	}
	paths := append([]string{}, r.Paths...)
	if r.RenameFrom != "" {
		paths = append(paths, r.RenameFrom)
	}
	if r.RenameTo != "" {
		paths = append(paths, r.RenameTo)
	}
	if len(paths) == 0 {
		return nil, fail("bad_request", "at least one path is required")
	}
	seen := map[string]bool{}
	normalized := []string{}
	ignored := []string{}
	for _, raw := range paths {
		if err := refuseControl(raw); err != nil {
			return nil, err
		}
		p, err := cleanPath(raw, "exact", false)
		if err != nil {
			return nil, err
		}
		key := pathKey(p, policy)
		if seen[key] {
			continue
		}
		seen[key] = true
		if defaultIgnored(key) {
			ignored = append(ignored, p)
		} else {
			normalized = append(normalized, p)
		}
	}
	cs, err := t.claimRows(e.Repo)
	if err != nil {
		return nil, err
	}
	conflicts := []map[string]any{}
	reuse := map[string]claim{}
	// Ignored paths take no automatic claim, but another session's explicit claim on one
	// still applies (D10), so every requested path is checked for conflicts.
	for _, p := range append(append([]string{}, normalized...), ignored...) {
		for _, c := range cs {
			if !overlaps(pathKey(p, policy), "exact", c.Key, c.Kind) {
				continue
			}
			if c.Execution == e.ID {
				reuse[p] = c
				continue
			}
			family, err := t.related(e.ID, c.Execution)
			if err != nil {
				return nil, err
			}
			if family {
				continue
			}
			v, err := t.conflict(c)
			if err != nil {
				return nil, err
			}
			v["path"] = p
			conflicts = append(conflicts, v)
			// The holder's release of this path is what this session waits for.
			if err = t.wait(e, pathKey(p, policy), "exact"); err != nil {
				return nil, err
			}
		}
	}
	out := map[string]any{"action": "allow", "granted": []string{}, "ignored": ignored, "conflicts": conflicts, "message": ""}
	if len(ignored) > 0 {
		out["message"] = "Iris: ignored paths are advisory only and did not take automatic claims."
	}
	if len(conflicts) > 0 {
		if action == "ask" && e.Client != "claude" {
			action = "deny"
		}
		out["action"] = action
		c := conflicts[0]
		expiry := c["expires_at"]
		out["message"] = fmt.Sprintf("Iris: %s until %v (%s claim). Message them with the send tool, or work on something else. Do not bypass this with a shell command.", heldBy(c), expiry, c["origin"])
		if action == "deny" {
			out["message"] = out["message"].(string) + " Ask the human if you need help resolving this conflict."
		}
		// No paths are granted on any conflict, even when policy permits proceeding.
	} else {
		if err = t.claimLimit(e, len(normalized)-len(reuse)); err != nil {
			return nil, err
		}
		for _, p := range normalized {
			if c, ok := reuse[p]; ok {
				if c.Origin == "auto" {
					if _, err = t.exec("UPDATE Claim SET expires_ms=? WHERE fence=?", t.s.clock.Now().Add(10*time.Minute).UnixMilli(), c.Fence); err != nil {
						return nil, err
					}
				}
				continue
			}
			if _, err = t.exec("INSERT INTO Claim(execution_id,repository_id,kind,path,path_key,origin,boot_epoch,expires_ms) VALUES(?,?,'exact',?,?,'auto',?,?)", e.ID, e.Repo, p, pathKey(p, policy), t.s.epoch, t.s.clock.Now().Add(10*time.Minute).UnixMilli()); err != nil {
				return nil, err
			}
		}
		for _, p := range normalized {
			if err = t.took(e, pathKey(p, policy)); err != nil {
				return nil, err
			}
		}
		out["granted"] = normalized
	}
	if out["attention"], err = t.attention(e); err != nil {
		return nil, err
	}
	if err = t.remember(e, key, "touch", r, out); err != nil {
		return nil, err
	}
	return out, nil
}

// claimLimit only stops growth. An upgrade can leave an Execution above the limit,
// and it must still be able to touch paths it already holds.
func (t *transaction) claimLimit(e execution, extra int) error {
	if extra == 0 {
		return nil
	}
	var count int
	if err := t.row("SELECT COUNT(*) FROM Claim WHERE execution_id=? AND revoked_ms IS NULL", e.ID).Scan(&count); err != nil {
		return err
	}
	if count+extra > 500 {
		return fail("too_large", "maximum 500 live claims per Execution")
	}
	return nil
}
func (t *transaction) fenceToken(c claim) string {
	return fmt.Sprintf("%s:%d:%d:%s", t.s.stream, c.Epoch, c.Fence, c.Execution)
}
func (t *transaction) explicit(r Request) (map[string]any, error) {
	e, err := t.live(r.ExecutionID, r.Generation)
	if err != nil {
		return nil, err
	}
	out, found, err := t.cached(e, r.Key, "claim", r)
	if err != nil {
		return nil, err
	}
	if found {
		var n int
		// Recovery rotates the numeric fence; merge preserves it and its epoch.
		// The stream is fixed for this database, so a live current-epoch fence
		// also proves that its saved token belongs to this boot.
		if err = t.row("SELECT COUNT(*) FROM Claim WHERE fence=? AND execution_id=? AND boot_epoch=? AND revoked_ms IS NULL", out["fence"], e.ID, t.s.epoch).Scan(&n); err != nil {
			return nil, err
		}
		if n == 0 {
			return nil, fail("lost", "cached claim is no longer live")
		}
		return out, nil
	}
	if r.Kind != "exact" && r.Kind != "prefix" {
		return nil, fail("bad_request", "kind must be exact or prefix")
	}
	if r.TTLS < 0 || r.TTLS > 86400*365 {
		return nil, fail("bad_request", "ttl_s is outside the supported range")
	}
	policy, _, err := t.policy(e.Repo)
	if err != nil {
		return nil, err
	}
	if err = refuseControl(r.Path); err != nil {
		return nil, err
	}
	p, err := cleanPath(r.Path, r.Kind, r.RepoWide)
	if err != nil {
		return nil, err
	}
	key := pathKey(p, policy)
	cs, err := t.claimRows(e.Repo)
	if err != nil {
		return nil, err
	}
	conflicts := []map[string]any{}
	var reuse *claim
	for _, c := range cs {
		if !overlaps(key, r.Kind, c.Key, c.Kind) {
			continue
		}
		if c.Execution == e.ID {
			if c.Kind == r.Kind && c.Key == key {
				copy := c
				reuse = &copy
			}
			continue
		}
		family, err := t.related(e.ID, c.Execution)
		if err != nil {
			return nil, err
		}
		if !family {
			v, err := t.conflict(c)
			if err != nil {
				return nil, err
			}
			v["path"] = p
			conflicts = append(conflicts, v)
			if err = t.wait(e, key, r.Kind); err != nil {
				return nil, err
			}
		}
	}
	if len(conflicts) > 0 {
		// The refusal is returned after the commit, so the waiter it leaves is kept.
		t.refused = &Error{"conflict", "claim overlaps another Execution", map[string]any{"conflicts": conflicts}}
		return nil, nil
	}
	if err = t.took(e, key); err != nil {
		return nil, err
	}
	expiry := sql.NullInt64{}
	if r.TTLS > 0 {
		expiry = sql.NullInt64{Int64: t.s.clock.Now().Add(time.Duration(r.TTLS) * time.Second).UnixMilli(), Valid: true}
	}
	var c claim
	if reuse != nil {
		c = *reuse
		c.Origin = "explicit"
		c.Expiry = expiry
		if _, err = t.exec("UPDATE Claim SET origin='explicit',expires_ms=? WHERE fence=?", expiry, c.Fence); err != nil {
			return nil, err
		}
	} else {
		if err = t.claimLimit(e, 1); err != nil {
			return nil, err
		}
		res, err := t.exec("INSERT INTO Claim(execution_id,repository_id,kind,path,path_key,origin,boot_epoch,expires_ms) VALUES(?,?,?,?,?,'explicit',?,?)", e.ID, e.Repo, r.Kind, p, key, t.s.epoch, expiry)
		if err != nil {
			return nil, err
		}
		f, _ := res.LastInsertId() // modernc SQLite always provides the last inserted rowid
		c = claim{Fence: f, Execution: e.ID, Repo: e.Repo, Kind: r.Kind, Path: p, Origin: "explicit", Epoch: t.s.epoch, Expiry: expiry}
	}
	out = map[string]any{"fence": c.Fence, "token": t.fenceToken(c), "expires_at": timestamp(c.Expiry)}
	if err = t.remember(e, r.Key, "claim", r, out); err != nil {
		return nil, err
	}
	return out, nil
}
func (t *transaction) release(r Request) (map[string]any, error) {
	e, err := t.live(r.ExecutionID, r.Generation)
	if err != nil {
		return nil, err
	}
	if !r.All {
		var owner string
		var epoch int64
		if err = t.row("SELECT execution_id,boot_epoch FROM Claim WHERE fence=?", r.Fence).Scan(&owner, &epoch); err != nil {
			return nil, absent(err, "conflict", "unknown fence")
		}
		if owner != e.ID || epoch != t.s.epoch {
			return nil, fail("conflict", "fence owner or epoch does not match")
		}
		if r.Token != "" && r.Token != fmt.Sprintf("%s:%d:%d:%s", t.s.stream, epoch, r.Fence, e.ID) {
			return nil, fail("conflict", "invalid fence token")
		}
	}
	q := "UPDATE Claim SET revoked_ms=? WHERE execution_id=? AND revoked_ms IS NULL"
	args := []any{t.s.clock.Now().UnixMilli(), e.ID}
	if !r.All {
		q += " AND fence=?"
		args = append(args, r.Fence)
	}
	res, err := t.exec(q, args...)
	if err != nil {
		return nil, err
	}
	n, _ := res.RowsAffected() // SQLite always reports affected rows
	if n > 0 {
		err = t.released(e.Repo, e.ID)
	}
	return map[string]any{}, err
}

// released records a release for every agent in repo, which the console and
// the claims tool show, and wakes only the sessions that were waiting for it.
func (t *transaction) released(repo, object string) error {
	if err := t.repoEvent(repo, "claim_released", object); err != nil {
		return err
	}
	return t.wakeWaiters(repo)
}
