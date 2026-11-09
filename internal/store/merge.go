package store

import "database/sql"

type mergeRepository struct {
	name, root, policy, action string
	contact                    sql.NullString
}

func (t *transaction) mergeRepository(repo string) (mergeRepository, error) {
	var r mergeRepository
	err := t.row("SELECT name,root_commit,case_policy,conflict_action,contact_agent_id FROM Repository WHERE id=?", repo).Scan(&r.name, &r.root, &r.policy, &r.action, &r.contact)
	return r, missing(err, "repository is unknown")
}

// mergeRepositories repairs only an operator-selected pair. Discovery never
// invokes it, and a dry run uses the same checks and snapshot without any writes.
func (t *transaction) mergeRepositories(r Request) (map[string]any, error) {
	from, into := r.FromRepository, r.IntoRepository
	if from == "" || into == "" || from == into {
		return nil, fail("bad_request", "from_repository_id and into_repository_id must name distinct repositories")
	}
	source, err := t.mergeRepository(from)
	if err != nil {
		return nil, err
	}
	destination, err := t.mergeRepository(into)
	if err != nil {
		return nil, err
	}
	// Repair cannot infer whether combining two histories is appropriate for a
	// member. Both grant modes must be reviewed and removed before any move.
	var grants int
	if err = t.row(`SELECT COUNT(*) FROM ConsoleRepositoryAccess g JOIN ConsoleAccount a ON a.id=g.account_id
 WHERE g.repository_id IN (?,?) AND a.role='member' AND g.mode IN ('ro','rw')`, from, into).Scan(&grants); err != nil {
		return nil, err
	}
	if grants > 0 {
		return nil, fail("conflict", "member repository grants must be reviewed and removed before merging")
	}
	if source.policy != destination.policy || source.action != destination.action {
		return nil, fail("conflict", "repository policies differ; choose matching policies before merging")
	}
	if source.contact.Valid && destination.contact.Valid && source.contact.String != destination.contact.String {
		return nil, fail("conflict", "repository contacts differ; clear or choose a contact before merging")
	}
	if err = t.mergeClaimCheck(from, into, destination.policy); err != nil {
		return nil, err
	}
	agents, changes, err := t.mergeAgentSlots(from, into)
	if err != nil {
		return nil, err
	}
	counts := map[string]int{}
	for _, table := range []string{"Agent", "Execution", "Claim", "Waiter", "Message", "RepositoryAlias"} {
		var countsValue int
		if err = t.row("SELECT COUNT(*) FROM "+table+" WHERE repository_id=?", from).Scan(&countsValue); err != nil {
			return nil, err
		}
		counts[table] = countsValue
	}
	aliases := []string{}
	rows, err := t.rows("SELECT alias FROM RepositoryAlias WHERE repository_id=? ORDER BY alias", from)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var alias string
		rows.Scan(&alias)
		aliases = append(aliases, alias)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return nil, err
	}
	var coalesced int
	if err = t.row("SELECT COUNT(*) FROM RepositoryAlias a JOIN RepositoryAlias b ON a.alias=b.alias WHERE a.repository_id=? AND b.repository_id=?", from, into).Scan(&coalesced); err != nil {
		return nil, err
	}
	contact := destination.contact
	if !contact.Valid {
		contact = source.contact
	}
	var contactID any
	if contact.Valid {
		contactID = contact.String
	}
	out := map[string]any{"from_repository_id": from, "into_repository_id": into, "dry_run": r.DryRun,
		"counts": counts, "aliases": aliases, "coalesced_aliases": coalesced, "slot_changes": changes,
		"contact_agent_id": contactID, "removed_repositories": 1, "created_events": 1,
		"from_repository": map[string]any{"id": from, "name": source.name, "root_commit": source.root, "case_policy": source.policy, "conflict_action": source.action},
		"into_repository": map[string]any{"id": into, "name": destination.name, "root_commit": destination.root, "case_policy": destination.policy, "conflict_action": destination.action}}
	if r.DryRun {
		return out, nil
	}
	for _, a := range agents {
		if _, err = t.exec("UPDATE Agent SET repository_id=?,slot=? WHERE id=?", into, a.slot, a.id); err != nil {
			return nil, err
		}
	}
	for _, table := range []string{"Execution", "Claim", "Waiter", "Message"} {
		if _, err = t.exec("UPDATE "+table+" SET repository_id=? WHERE repository_id=?", into, from); err != nil {
			return nil, err
		}
	}
	if _, err = t.exec("INSERT OR IGNORE INTO RepositoryAlias(alias,repository_id) SELECT alias,? FROM RepositoryAlias WHERE repository_id=?", into, from); err != nil {
		return nil, err
	}
	if _, err = t.exec("DELETE FROM RepositoryAlias WHERE repository_id=?", from); err != nil {
		return nil, err
	}
	if _, err = t.exec("UPDATE Repository SET contact_agent_id=? WHERE id=?", contact, into); err != nil {
		return nil, err
	}
	if _, err = t.exec("DELETE FROM Repository WHERE id=?", from); err != nil {
		return nil, err
	}
	return out, t.event("", "", "changed", "")
}

// Liveness matches expire without changing the stored sessions, claims or mail.
// Recovering claims still block until their persisted recovery deadline.
func (t *transaction) mergeClaimCheck(from, into, policy string) error {
	rows, err := t.rows(`SELECT c.fence,c.execution_id,c.repository_id,c.kind,c.path FROM Claim c JOIN Execution e ON e.id=c.execution_id
 WHERE c.repository_id IN (?,?) AND c.revoked_ms IS NULL AND e.state='active'
 AND (c.expires_ms IS NULL OR c.expires_ms>?)
 AND ((e.recovery_deadline IS NOT NULL AND e.recovery_deadline>?)
 OR (e.recovery_deadline IS NULL AND e.boot_epoch=? AND e.deadline_tick_ms>?)) ORDER BY c.fence`, from, into, t.s.clock.Now().UnixMilli(), t.s.clock.Now().UnixMilli(), t.s.epoch, t.s.clock.Tick())
	if err != nil {
		return err
	}
	claims := []claim{}
	for rows.Next() {
		var c claim
		rows.Scan(&c.Fence, &c.Execution, &c.Repo, &c.Kind, &c.Path)
		c.Key = pathKey(c.Path, policy)
		claims = append(claims, c)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return err
	}
	for i, a := range claims {
		for _, b := range claims[i+1:] {
			if a.Repo == b.Repo || !overlaps(a.Key, a.Kind, b.Key, b.Kind) {
				continue
			}
			related, err := t.related(a.Execution, b.Execution)
			if err != nil {
				return err
			}
			if !related {
				return &Error{"conflict", "live claims overlap; release or resolve them before merging", map[string]any{"fences": []int64{a.Fence, b.Fence}, "paths": []string{a.Path, b.Path}}}
			}
		}
	}
	return nil
}

type mergeAgent struct {
	id, name, host, client, worktree string
	slot                             int
}

func (t *transaction) mergeAgentSlots(from, into string) ([]mergeAgent, []map[string]any, error) {
	rows, err := t.rows("SELECT id,name,host,client,worktree,slot,repository_id FROM Agent WHERE repository_id IN (?,?) ORDER BY host,client,worktree,slot,id", from, into)
	if err != nil {
		return nil, nil, err
	}
	type slotKey struct {
		host, client, worktree string
		slot                   int
	}
	key := func(a mergeAgent) slotKey { return slotKey{a.host, a.client, a.worktree, a.slot} }
	occupied := map[slotKey]bool{}
	agents := []mergeAgent{}
	for rows.Next() {
		var a mergeAgent
		var repo string
		rows.Scan(&a.id, &a.name, &a.host, &a.client, &a.worktree, &a.slot, &repo)
		if repo == into {
			occupied[key(a)] = true
		} else {
			agents = append(agents, a)
		}
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return nil, nil, err
	}
	// Reserve source slots that already fit before relocating colliding slots.
	collisions := map[string]bool{}
	for _, a := range agents {
		if occupied[key(a)] {
			collisions[a.id] = true
		} else {
			occupied[key(a)] = true
		}
	}
	changes := []map[string]any{}
	for i := range agents {
		a := &agents[i]
		if !collisions[a.id] {
			continue
		}
		prior := a.slot
		a.slot = 1
		for occupied[key(*a)] {
			a.slot++
		}
		occupied[key(*a)] = true
		changes = append(changes, map[string]any{"agent_id": a.id, "name": a.name, "from_slot": prior, "into_slot": a.slot})
	}
	return agents, changes, nil
}
