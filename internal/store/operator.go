package store

import (
	"database/sql"
	"encoding/json"
)

// The operator has a mailbox per repository, but never a live session or lease.
func (t *transaction) human(repo string) (string, error) {
	agent := "human_" + repo
	_, err := t.exec("INSERT OR IGNORE INTO Agent VALUES(?,?,?,?,?,?,1)", agent, agent, repo, "operator", "human", "")
	return agent, err
}
func (t *transaction) humanSender(r Request) (execution, error) {
	repo := r.RepositoryID
	if r.ReplyTo != "" {
		if err := t.row("SELECT repository_id FROM Message WHERE id=?", r.ReplyTo).Scan(&repo); err != nil {
			return execution{}, fail("not_found", "unknown reply")
		}
	}
	if repo == "" && len(r.To) > 0 {
		if err := t.row("SELECT repository_id FROM Agent WHERE name=? OR id=?", r.To[0], r.To[0]).Scan(&repo); err != nil {
			return execution{}, fail("not_found", "unknown recipient")
		}
	}
	if repo == "" {
		return execution{}, fail("bad_request", "repository or recipient required")
	}
	agent, err := t.human(repo)
	if err != nil {
		return execution{}, err
	}
	ex := "operator_" + repo
	_, err = t.exec(`INSERT OR IGNORE INTO Execution(id,agent_id,repository_id,host_id,client,session_id,generation,worktree,branch,boot_epoch,deadline_tick_ms,state,client_pid,client_pid_start_ms,single_session) VALUES(?,?,?,'operator','human',?,1,'','',?,0,'ended',0,0,0)`, ex, agent, repo, ex, t.s.epoch)
	return execution{ID: ex, AgentID: agent, Repo: repo, Generation: 1}, err
}
func (t *transaction) operator(op string, r Request) (map[string]any, error) {
	switch op {
	case "ui_messages":
		r.Source = "operator"
		return t.send(r)
	case "ui_overview":
		return t.overview()
	case "ui_threads":
		return t.threads(r)
	case "ui_thread":
		return t.thread(r.Key)
	case "ui_revoke":
		var repo string
		if err := t.row("SELECT repository_id FROM Claim WHERE fence=?", r.Fence).Scan(&repo); err != nil {
			return nil, fail("not_found", "unknown claim")
		}
		if _, err := t.exec("UPDATE Claim SET revoked_ms=? WHERE fence=? AND revoked_ms IS NULL", t.s.clock.Now().UnixMilli(), r.Fence); err != nil {
			return nil, err
		}
		return map[string]any{}, t.repoEvent(repo, "claim_released", r.Key)
	case "ui_doctor_get":
		out := map[string]any{"checked_at": nil, "clients": []any{}}
		var raw string
		err := t.row("SELECT report FROM DoctorReport WHERE id=1").Scan(&raw)
		if err == sql.ErrNoRows {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		err = json.Unmarshal([]byte(raw), &out)
		return out, err
	case "ui_doctor_put":
		for _, c := range r.Clients {
			switch c["status"] {
			case "ok", "not_configured", "needs_human", "silent":
			default:
				return nil, fail("bad_request", "invalid doctor status")
			}
		}
		raw, err := json.Marshal(map[string]any{"checked_at": r.CheckedAt, "clients": r.Clients})
		if err != nil {
			return nil, err
		}
		if _, err = t.exec("INSERT INTO DoctorReport VALUES(1,?) ON CONFLICT(id) DO UPDATE SET report=excluded.report", string(raw)); err != nil {
			return nil, err
		}
		return map[string]any{}, t.event("", "", "changed", "")
	case "ui_events":
		var high int64
		err := t.row("SELECT COALESCE((SELECT seq FROM sqlite_sequence WHERE name='Event'),0)").Scan(&high)
		return map[string]any{"cursor": high, "kind": "changed"}, err
	}
	return nil, fail("not_found", "unknown operator operation")
}
func (t *transaction) overview() (map[string]any, error) {
	repos := []map[string]any{}
	agents := []map[string]any{}
	claims := []map[string]any{}
	rows, err := t.rows("SELECT id FROM Repository ORDER BY id")
	if err != nil {
		return nil, err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		rows.Scan(&id)
		ids = append(ids, id)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		aliases := []string{}
		var name string
		if err = t.row("SELECT name FROM Repository WHERE id=?", id).Scan(&name); err != nil {
			return nil, err
		}
		rows, err = t.rows("SELECT alias FROM RepositoryAlias WHERE repository_id=? ORDER BY alias", id)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var a string
			rows.Scan(&a)
			aliases = append(aliases, a)
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return nil, err
		}
		repos = append(repos, map[string]any{"id": id, "name": name, "aliases": aliases})
		cs, err := t.claims(id)
		if err != nil {
			return nil, err
		}
		for _, c := range cs {
			c["repository_id"] = id
			claims = append(claims, c)
		}
	}
	rows, err = t.rows(`SELECT a.id,a.name,a.client,a.host,a.repository_id,a.worktree,COALESCE(e.branch,''),COALESCE(e.state,'offline'),COALESCE(e.last_seen_ms,0),(SELECT COUNT(*) FROM Delivery d WHERE d.recipient_agent_id=a.id AND d.id NOT IN (SELECT delivery_id FROM Ack)) FROM Agent a LEFT JOIN Execution e ON e.id=(SELECT id FROM Execution WHERE agent_id=a.id ORDER BY rowid DESC LIMIT 1) WHERE a.client<>'human' ORDER BY a.name`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id, name, client, host, repo, worktree, branch, state string
		var seen int64
		var unread int
		rows.Scan(&id, &name, &client, &host, &repo, &worktree, &branch, &state, &seen, &unread)
		agents = append(agents, map[string]any{"id": id, "name": name, "client": client, "host": host, "repository_id": repo, "worktree": worktree, "branch": branch, "state": state, "last_seen": timestamp(sql.NullInt64{Int64: seen, Valid: seen > 0}), "unread": unread})
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return nil, err
	}
	var unread int
	err = t.row(`SELECT COUNT(*) FROM Delivery d JOIN Agent a ON a.id=d.recipient_agent_id WHERE a.client='human' AND d.id NOT IN (SELECT delivery_id FROM Ack)`).Scan(&unread)
	return map[string]any{"repositories": repos, "agents": agents, "claims": claims, "human_unread": unread}, err
}
func (t *transaction) threads(r Request) (map[string]any, error) {
	q := `SELECT m.thread_id,(SELECT subject FROM Message WHERE id=m.thread_id),COUNT(*),MAX(m.created_ms),SUM((SELECT COUNT(*) FROM Delivery d JOIN Agent a ON a.id=d.recipient_agent_id WHERE d.message_id=m.id AND a.client='human' AND d.id NOT IN (SELECT delivery_id FROM Ack))) FROM Message m WHERE 1=1`
	args := []any{}
	if r.RepositoryID != "" {
		q += " AND m.repository_id=?"
		args = append(args, r.RepositoryID)
	}
	if r.Q != "" {
		q += " AND m.thread_id IN (SELECT thread_id FROM Message WHERE rowid IN (SELECT rowid FROM MessageSearch WHERE MessageSearch MATCH ?))"
		args = append(args, r.Q)
	}
	if r.Agent != "" {
		q += ` AND m.thread_id IN (SELECT mm.thread_id FROM Message mm JOIN Execution e ON e.id=mm.sender_execution_id JOIN Agent a ON a.id=e.agent_id WHERE a.name=? OR a.id=? UNION SELECT mm.thread_id FROM Message mm JOIN Delivery d ON d.message_id=mm.id JOIN Agent a ON a.id=d.recipient_agent_id WHERE a.name=? OR a.id=?)`
		args = append(args, r.Agent, r.Agent, r.Agent, r.Agent)
	}
	q += " GROUP BY m.thread_id"
	if r.Before != "" {
		q += " HAVING MAX(m.created_ms)<COALESCE((SELECT MAX(created_ms) FROM Message WHERE thread_id=?),CAST(? AS INTEGER))"
		args = append(args, r.Before, r.Before)
	}
	q += " ORDER BY MAX(m.created_ms) DESC,m.thread_id DESC LIMIT ?"
	args = append(args, boundedLimit(r.Limit, 50, 100))
	rows, err := t.rows(q, args...)
	if err != nil {
		return nil, fail("bad_request", "invalid thread search")
	}
	out := []map[string]any{}
	for rows.Next() {
		var id, subject string
		var count, at, unread int64
		rows.Scan(&id, &subject, &count, &at, &unread)
		out = append(out, map[string]any{"id": id, "subject": subject, "count": count, "last_at": timestamp(sql.NullInt64{Int64: at, Valid: true}), "unread": unread})
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return nil, err
	}
	for _, th := range out {
		names := []string{}
		rows, err = t.rows(`SELECT DISTINCT CASE WHEN a.client='human' THEN 'human' ELSE a.name END FROM Agent a WHERE a.id IN (SELECT e.agent_id FROM Message m JOIN Execution e ON e.id=m.sender_execution_id WHERE m.thread_id=? UNION SELECT d.recipient_agent_id FROM Delivery d JOIN Message m ON m.id=d.message_id WHERE m.thread_id=?) ORDER BY 1`, th["id"], th["id"])
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var name string
			rows.Scan(&name)
			names = append(names, name)
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return nil, err
		}
		th["participants"] = names
	}
	return map[string]any{"threads": out}, nil
}
func (t *transaction) thread(id string) (map[string]any, error) {
	rows, err := t.rows(`SELECT m.id,CASE WHEN a.client='human' THEN 'human' ELSE a.name END,m.subject,m.body,m.created_ms,m.reply_to FROM Message m JOIN Execution e ON e.id=m.sender_execution_id JOIN Agent a ON a.id=e.agent_id WHERE m.thread_id=? ORDER BY m.created_ms,m.rowid`, id)
	if err != nil {
		return nil, err
	}
	messages := []map[string]any{}
	for rows.Next() {
		var mid, sender, subject, body string
		var at int64
		var reply sql.NullString
		rows.Scan(&mid, &sender, &subject, &body, &at, &reply)
		messages = append(messages, map[string]any{"id": mid, "thread_id": id, "sender": sender, "subject": subject, "body": body, "created_at": timestamp(sql.NullInt64{Int64: at, Valid: true}), "reply_to": reply.String})
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return nil, err
	}
	if len(messages) == 0 {
		return nil, fail("not_found", "unknown thread")
	}
	for _, m := range messages {
		recipients := []map[string]any{}
		rows, err = t.rows(`SELECT CASE WHEN a.client='human' THEN 'human' ELSE a.name END,d.state FROM DeliveryState d JOIN Agent a ON a.id=d.recipient_agent_id WHERE d.message_id=? ORDER BY a.name`, m["id"])
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var name, state string
			rows.Scan(&name, &state)
			recipients = append(recipients, map[string]any{"name": name, "state": state})
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return nil, err
		}
		m["recipients"] = recipients
	}
	return map[string]any{"id": id, "messages": messages}, nil
}
