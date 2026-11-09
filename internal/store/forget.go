package store

import "fmt"

// forget removes one repository, or one agent whose sessions have all ended, with
// every record that refers to it. It exists for records nobody should keep, such as
// those a measurement run leaves behind; the hub has no other way to delete them.
//
// It refuses while anything it would remove is in use: an active Execution, or a
// claim not yet revoked. Expiry revokes a lost session's claims, so in practice that
// means a live one. A dry run reports the same counts and changes nothing.
//
// Mail goes with its sender or recipient, whoever the other party is. For any other
// agent, an event about a delivery that is gone now reads as settled, so a connected
// watcher is not woken for it. A changed event tells the web console to refresh.
func (t *transaction) forget(r Request) (map[string]any, error) {
	if (r.RepositoryID == "") == (r.Agent == "") {
		return nil, fail("bad_request", "name exactly one of repository_id or agent")
	}
	repo := r.RepositoryID
	// Each set is a subquery over the live tables. The deletes below run in an order
	// that keeps every table a later set reads from until that set is no longer needed.
	agents := set{"SELECT id FROM Agent WHERE (?<>'' AND repository_id=?) OR (?<>'' AND name=?)", []any{repo, repo, r.Agent, r.Agent}}
	executions := agents.wrap("SELECT id FROM Execution WHERE agent_id IN (%s) OR (?<>'' AND repository_id=?)", repo, repo)
	messages := executions.wrap("SELECT id FROM Message WHERE sender_execution_id IN (%s) OR (?<>'' AND repository_id=?)", repo, repo)
	byAgent := agents.wrap("recipient_agent_id IN (%s)")
	byMessage := messages.wrap("message_id IN (%s)")
	byExecution := executions.wrap("execution_id IN (%s)")
	deliveries := set{"SELECT id FROM Delivery WHERE " + byAgent.q + " OR " + byMessage.q + " OR " + byExecution.q,
		join(byAgent.args, byMessage.args, byExecution.args)}

	var known, active, claims int
	q, args := "SELECT COUNT(*) FROM ("+agents.q+")", agents.args
	if repo != "" {
		q, args = "SELECT COUNT(*) FROM Repository WHERE id=?", []any{repo}
	}
	if err := t.row(q, args...).Scan(&known); err != nil {
		return nil, err
	}
	if known == 0 {
		return nil, fail("not_found", "no such repository or agent")
	}
	inUse := executions.twice("SELECT (SELECT COUNT(*) FROM Execution WHERE state='active' AND id IN (%[1]s))," +
		"(SELECT COUNT(*) FROM Claim WHERE revoked_ms IS NULL AND execution_id IN (%[1]s))")
	if err := t.row(inUse.q, inUse.args...).Scan(&active, &claims); err != nil {
		return nil, err
	}
	if active > 0 || claims > 0 {
		return nil, &Error{Code: "conflict", Message: "still in use: end its sessions and revoke its claims first",
			Details: map[string]any{"active_executions": active, "live_claims": claims}}
	}
	// In foreign-key order: each table goes before anything it refers to.
	targets := []struct {
		table string
		where set
	}{
		{"Ack", join2(deliveries.wrap("delivery_id IN (%s)"), executions.wrap("execution_id IN (%s)"))},
		{"Delivery", deliveries.wrap("id IN (%s)")},
		{"Message", messages.wrap("id IN (%s)")},
		{"Claim", join2(executions.wrap("execution_id IN (%s)"), set{"?<>'' AND repository_id=?", []any{repo, repo}})},
		{"Waiter", join2(executions.wrap("execution_id IN (%s)"), set{"?<>'' AND repository_id=?", []any{repo, repo}})},
		{"Cursor", executions.wrap("execution_id IN (%s)")},
		{"ShellSnapshot", executions.wrap("execution_id IN (%s)")},
		{"Operation", agents.wrap("principal_agent_id IN (%s)")},
		{"Event", join2(agents.wrap("agent_id IN (%s)"), executions.wrap("execution_id IN (%s)"))},
		{"Execution", executions.wrap("id IN (%s)")},
		{"Agent", agents.wrap("id IN (%s)")},
		{"RepositoryAlias", set{"?<>'' AND repository_id=?", []any{repo, repo}}},
		{"Repository", set{"?<>'' AND id=?", []any{repo, repo}}},
	}
	counts := map[string]int{}
	for _, x := range targets {
		var n int
		if err := t.row("SELECT COUNT(*) FROM "+x.table+" WHERE "+x.where.q, x.where.args...).Scan(&n); err != nil {
			return nil, err
		}
		counts[x.table] = n
	}
	// A subagent of another agent outlives its forgotten parent, without the link.
	children := executions.twice("parent_id IN (%[1]s) AND id NOT IN (%[1]s)")
	var detached int
	if err := t.row("SELECT COUNT(*) FROM Execution WHERE "+children.q, children.args...).Scan(&detached); err != nil {
		return nil, err
	}
	out := map[string]any{"scope": scopeOf(r), "dry_run": r.DryRun, "counts": counts, "detached_children": detached}
	if r.DryRun {
		return out, nil
	}
	// Removing a repository invalidates every account that could read it. Session
	// revocation, version changes and grant deletion share the removal transaction.
	if _, err := t.exec(`DELETE FROM ConsoleSession WHERE account_id IN
 (SELECT account_id FROM ConsoleRepositoryAccess WHERE repository_id=?)`, repo); err != nil {
		return nil, err
	}
	if _, err := t.exec(`UPDATE ConsoleAccount SET auth_version=auth_version+1,edit_revision=edit_revision+1
 WHERE id IN (SELECT account_id FROM ConsoleRepositoryAccess WHERE repository_id=?)`, repo); err != nil {
		return nil, err
	}
	if _, err := t.exec("DELETE FROM ConsoleRepositoryAccess WHERE repository_id=?", repo); err != nil {
		return nil, err
	}
	// The search index holds its own copy of each message, which only an explicit
	// delete command removes.
	search := messages.wrap("INSERT INTO MessageSearch(MessageSearch,rowid,subject,body) SELECT 'delete',rowid,subject,body FROM Message WHERE id IN (%s)")
	if _, err := t.exec(search.q, search.args...); err != nil {
		return nil, err
	}
	if _, err := t.exec("UPDATE Execution SET parent_id=NULL WHERE "+children.q, children.args...); err != nil {
		return nil, err
	}
	// A repository whose contact is forgotten falls back to its human inbox. The
	// foreign key would do this too; it is explicit so it does not rest on the pragma.
	contacts := agents.wrap("UPDATE Repository SET contact_agent_id=NULL WHERE contact_agent_id IN (%s)")
	if _, err := t.exec(contacts.q, contacts.args...); err != nil {
		return nil, err
	}
	for _, x := range targets {
		if _, err := t.exec("DELETE FROM "+x.table+" WHERE "+x.where.q, x.where.args...); err != nil {
			return nil, err
		}
	}
	return out, t.event("", "", "changed", "")
}

// set is a SQL fragment and the arguments its placeholders take, in order.
type set struct {
	q    string
	args []any
}

// wrap puts s into format at its one %s. Extra arguments follow s's own.
func (s set) wrap(format string, extra ...any) set {
	return set{fmt.Sprintf(format, s.q), join(s.args, extra)}
}

// twice puts s into format at both of its %[1]s, so s's arguments come twice.
func (s set) twice(format string) set {
	return set{fmt.Sprintf(format, s.q), join(s.args, s.args)}
}

func join2(a, b set) set { return set{a.q + " OR " + b.q, join(a.args, b.args)} }

func join(parts ...[]any) []any {
	var out []any
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func scopeOf(r Request) map[string]any {
	if r.RepositoryID != "" {
		return map[string]any{"repository_id": r.RepositoryID}
	}
	return map[string]any{"agent": r.Agent}
}
