package store

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
)

// The operator has a mailbox per repository, but never a live session or lease.
func (t *transaction) human(repo string) (string, error) {
	// A merge preserves the source's mailbox id and may leave it as the only
	// human mailbox here. The first slot remains the destination's address when
	// both repositories already had human mail.
	var agent string
	err := t.row("SELECT id FROM Agent WHERE repository_id=? AND host='operator' AND client='human' AND worktree='' ORDER BY slot,id LIMIT 1", repo).Scan(&agent)
	if err == nil {
		return agent, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	agent = "human_" + repo
	_, err = t.exec("INSERT OR IGNORE INTO Agent VALUES(?,?,?,?,?,?,1)", agent, agent, repo, "operator", "human", "")
	return agent, err
}
func (t *transaction) operatorRepository(r Request) (string, error) {
	// The operator writes from a repository's view, and that is the message's own
	// repository, even when it replies to mail from another one (ADR 004).
	repo := r.RepositoryID
	if repo == "" && r.ReplyTo != "" {
		if err := t.row("SELECT repository_id FROM Message WHERE id=?", r.ReplyTo).Scan(&repo); err != nil {
			return "", missing(err, "unknown reply")
		}
	}
	if repo == "" && len(r.To) > 0 {
		if err := t.row("SELECT repository_id FROM Agent WHERE name=? OR id=?", r.To[0], r.To[0]).Scan(&repo); err != nil {
			return "", missing(err, "unknown recipient")
		}
	}
	if repo == "" {
		return "", fail("bad_request", "repository or recipient required")
	}
	return repo, nil
}

func (t *transaction) operatorSender(repo string) (execution, error) {
	// The operator may write from any repository on the hub, but only from one that exists.
	var known int
	if err := t.row("SELECT COUNT(*) FROM Repository WHERE id=?", repo).Scan(&known); err != nil {
		return execution{}, err
	}
	if known == 0 {
		return execution{}, fail("not_found", "unknown repository")
	}
	agent, err := t.human(repo)
	if err != nil {
		return execution{}, err
	}
	ex := "operator_" + repo
	_, err = t.exec(`INSERT OR IGNORE INTO Execution(id,agent_id,repository_id,host_id,client,session_id,generation,worktree,branch,boot_epoch,deadline_tick_ms,state,client_pid,client_pid_start_ms,single_session) VALUES(?,?,?,'operator','human',?,1,'','',?,0,'ended',0,0,0)`, ex, agent, repo, ex, t.s.epoch)
	return execution{ID: ex, AgentID: agent, Repo: repo, Generation: 1}, err
}

// operatorCached searches all preserved operator principals in the request's
// repository. A merge changes where a recipient or reply resolves, but must not
// change the namespace of a send whose response was lost. Retired explicit ids
// can locate their old principal only for this lookup, never for a new send.
func (t *transaction) operatorCached(repo string, r Request) (map[string]any, bool, error) {
	rows, err := t.rows(`SELECT o.request_hash,o.result_json FROM Operation o JOIN Agent a ON a.id=o.principal_agent_id
 WHERE o.key=? AND a.host='operator' AND a.client='human' AND a.worktree=''
 AND (a.repository_id=?
 OR a.repository_id=(SELECT repository_id FROM Agent WHERE id=? AND host='operator' AND client='human' AND worktree='')
 OR a.repository_id=(SELECT e.repository_id FROM Execution e JOIN Agent origin ON origin.id=e.agent_id
 WHERE e.id=? AND origin.host='operator' AND origin.client='human' AND origin.worktree='')) ORDER BY a.id`, r.Key, repo, "human_"+repo, "operator_"+repo)
	if err != nil {
		return nil, false, err
	}
	type savedOperation struct {
		hash   []byte
		result string
	}
	operations := []savedOperation{}
	for rows.Next() {
		var operation savedOperation
		rows.Scan(&operation.hash, &operation.result)
		operations = append(operations, operation)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return nil, false, err
	}
	var result map[string]any
	matches := 0
	raw, _ := json.Marshal(struct {
		Route   string
		Request any
	}{"message", r})
	hash := sha256.Sum256(raw)
	for _, operation := range operations {
		if !bytes.Equal(operation.hash, hash[:]) {
			continue
		}
		decoder := json.NewDecoder(strings.NewReader(operation.result))
		decoder.UseNumber()
		if err = decoder.Decode(&result); err != nil {
			return nil, false, err
		}
		matches++
	}
	if matches > 1 {
		return nil, false, fail("key_reused", "retry key matches multiple preserved operator origins; inspect history before sending with a new key")
	}
	if matches == 1 {
		return result, true, nil
	}
	if len(operations) > 0 {
		return nil, false, fail("key_reused", "retry key was used for a different request in a preserved operator origin")
	}
	return nil, false, nil
}
func (t *transaction) operator(op string, r Request) (map[string]any, error) {
	switch op {
	case "ui_messages":
		r.Source = "operator"
		return t.send(r)
	case "ui_overview":
		return t.overview()
	case "ui_deliveries":
		return t.deliveries(r)
	case "ui_threads":
		return t.threads(r)
	case "ui_read":
		return t.readThread(r)
	case "ui_thread":
		return t.thread(r.Key, r.RepositoryID)
	case "ui_revoke":
		var repo string
		if err := t.row("SELECT repository_id FROM Claim WHERE fence=?", r.Fence).Scan(&repo); err != nil {
			return nil, missing(err, "unknown claim")
		}
		if _, err := t.exec("UPDATE Claim SET revoked_ms=? WHERE fence=? AND revoked_ms IS NULL", t.s.clock.Now().UnixMilli(), r.Fence); err != nil {
			return nil, err
		}
		return map[string]any{}, t.released(repo, r.Key)
	case "ui_doctor_get":
		// Every host's last report (ADR 007). A host that never reported is absent, rather
		// than shown as healthy.
		rows, err := t.rows("SELECT host,report FROM HostReport ORDER BY host")
		if err != nil {
			return nil, err
		}
		defer func() { _ = rows.Close() }()
		hosts := []map[string]any{}
		for rows.Next() {
			var host, raw string
			rows.Scan(&host, &raw)
			report := map[string]any{}
			if err := json.Unmarshal([]byte(raw), &report); err != nil {
				return nil, err
			}
			report["host"] = host
			hosts = append(hosts, report)
		}
		return map[string]any{"hosts": hosts}, rows.Err()
	case "ui_doctor_put":
		for _, c := range r.Clients {
			switch c["status"] {
			case "ok", "not_configured", "needs_human", "silent", "slow":
			default:
				return nil, fail("bad_request", "invalid doctor status")
			}
		}
		raw, err := json.Marshal(map[string]any{"checked_at": r.CheckedAt, "clients": r.Clients})
		if err != nil {
			return nil, err
		}
		// A doctor from before ADR 007 names no host. Its report goes to "unknown", so it
		// can never replace a real host's.
		host := r.Host
		if host == "" {
			host = "unknown"
		}
		if _, err = t.exec("INSERT INTO HostReport VALUES(?,?) ON CONFLICT(host) DO UPDATE SET report=excluded.report", host, string(raw)); err != nil {
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
	repositoryScope, repositoryArgs := t.repositoryVisibleSQL("id")
	rows, err := t.rows("SELECT id FROM Repository WHERE "+repositoryScope+" ORDER BY id", repositoryArgs...)
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
		var name, access, accountID string
		if t.console != nil {
			accountID = t.console.ID
		}
		if err = t.row("SELECT name,COALESCE((SELECT mode FROM ConsoleRepositoryAccess WHERE account_id=? AND repository_id=Repository.id),'') FROM Repository WHERE id=?", accountID, id).Scan(&name, &access); err != nil {
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
		repository := map[string]any{"id": id, "name": name, "aliases": aliases}
		// This describes the current console controls; each mutation still checks
		// authority in its own transaction. Legacy link sessions have no account.
		if t.console != nil {
			if t.console.Role == "operator" {
				access = "rw"
			}
			repository["console_access"] = access
		}
		repos = append(repos, repository)
		cs, err := t.claims(id)
		if err != nil {
			return nil, err
		}
		for _, c := range cs {
			c["repository_id"] = id
			claims = append(claims, c)
		}
	}
	agentScope, agentArgs := t.repositoryVisibleSQL("a.repository_id")
	rows, err = t.rows(`SELECT a.id,a.name,a.client,a.host,a.repository_id,a.worktree,COALESCE(e.branch,''),COALESCE(e.state,'offline'),COALESCE(e.last_seen_ms,0),(SELECT COUNT(*) FROM Delivery d WHERE d.recipient_agent_id=a.id AND d.id NOT IN (SELECT delivery_id FROM Ack)) FROM Agent a LEFT JOIN Execution e ON e.id=(SELECT id FROM Execution WHERE agent_id=a.id ORDER BY rowid DESC LIMIT 1) WHERE a.client<>'human' AND `+agentScope+` ORDER BY a.name`, agentArgs...)
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
	err = t.row(`SELECT COUNT(*) FROM Delivery d JOIN Agent a ON a.id=d.recipient_agent_id WHERE a.client='human' AND d.id NOT IN (SELECT delivery_id FROM Ack) AND `+agentScope, agentArgs...).Scan(&unread)
	return map[string]any{"repositories": repos, "agents": agents, "claims": claims, "human_unread": unread}, err
}

// visibleIn is the condition for a message, under alias, to show in repository repo
// (ADR 004): its sender's repository is repo, or one of its recipients is in repo.
// After A→B and then B→C, C's view shows B→C only. With no repository, every
// message shows. It returns the SQL and its arguments.
func visibleIn(alias, repo string) (string, []any) {
	if repo == "" {
		return "1=1", nil
	}
	return "(" + alias + ".repository_id=? OR EXISTS (SELECT 1 FROM Delivery dv JOIN Agent av ON av.id=dv.recipient_agent_id WHERE dv.message_id=" + alias + ".id AND av.repository_id=?))", []any{repo, repo}
}

func (t *transaction) threads(r Request) (map[string]any, error) {
	if err := t.validateConsoleThreadFilters(r); err != nil {
		return nil, err
	}
	// A thread is titled by its earliest message visible here, because its first message
	// may belong to another repository's view, or have been removed by forget.
	title, titleArgs := t.visibleIn("m2", r.RepositoryID)
	visible, visibleArgs := t.visibleIn("m", r.RepositoryID)
	humanScope, humanArgs := t.repositoryVisibleSQL("a.repository_id")
	q := `SELECT m.thread_id,(SELECT m2.subject FROM Message m2 WHERE m2.thread_id=m.thread_id AND ` + title + ` ORDER BY m2.created_ms,m2.rowid LIMIT 1),COUNT(*),MAX(m.created_ms),SUM((SELECT COUNT(*) FROM Delivery d JOIN Agent a ON a.id=d.recipient_agent_id WHERE d.message_id=m.id AND a.client='human' AND (?='' OR a.repository_id=?) AND d.id NOT IN (SELECT delivery_id FROM Ack) AND ` + humanScope + `)) FROM Message m WHERE ` + visible
	// Unread counts this view's human inbox only: in a thread that crosses repositories,
	// another repository's unread mail is not this view's to clear.
	args := append(append(append(append([]any{}, titleArgs...), r.RepositoryID, r.RepositoryID), humanArgs...), visibleArgs...)
	if match := literalMatch(r.Q); match != "" {
		search, searchArgs := t.consoleMatchIn("ms", r.RepositoryID)
		q += " AND m.thread_id IN (SELECT ms.thread_id FROM Message ms WHERE ms.rowid IN (SELECT rowid FROM MessageSearch WHERE MessageSearch MATCH ?) AND " + search + ")"
		args = append(append(args, match), searchArgs...)
	}
	if r.Agent != "" {
		participant := "a.name=? OR a.id=?"
		participantArgs := []any{r.Agent, r.Agent}
		if r.Agent == "human" {
			// Each repository has its own operator mailbox, but the console presents
			// them as one human participant across sent and received conversations.
			participant, participantArgs = "a.client='human'", nil
		}
		participantVisible, participantVisibleArgs := t.consoleMatchIn("mm", r.RepositoryID)
		q += ` AND m.thread_id IN (SELECT mm.thread_id FROM Message mm JOIN Execution e ON e.id=mm.sender_execution_id JOIN Agent a ON a.id=e.agent_id WHERE (` + participant + `) AND ` + participantVisible + ` UNION SELECT mm.thread_id FROM Message mm JOIN Delivery d ON d.message_id=mm.id JOIN Agent a ON a.id=d.recipient_agent_id WHERE (` + participant + `) AND ` + participantVisible + ` AND ` + humanScope + `)`
		args = append(append(append(append(append(args, participantArgs...), participantVisibleArgs...), participantArgs...), participantVisibleArgs...), humanArgs...)
	}
	q += " GROUP BY m.thread_id"
	if r.Before != "" {
		// A thread cursor continues after that thread in the list's own order: by its last
		// message visible in this view, then by thread id, so threads that share the
		// cursor's millisecond are neither skipped nor repeated (gap review A15). A cursor
		// that names no thread is a millisecond timestamp, as before.
		cursor, cursorArgs := t.visibleIn("mc", r.RepositoryID)
		at := "COALESCE((SELECT MAX(mc.created_ms) FROM Message mc WHERE mc.thread_id=? AND " + cursor + "),CAST(? AS INTEGER))"
		q += " HAVING MAX(m.created_ms)<" + at + " OR (MAX(m.created_ms)=" + at + " AND m.thread_id<? AND EXISTS(SELECT 1 FROM Message WHERE thread_id=?))"
		for range 2 {
			args = append(append(append(args, r.Before), cursorArgs...), r.Before)
		}
		args = append(args, r.Before, r.Before)
	}
	// One more than the page is read, to tell whether another page follows.
	limit := boundedLimit(r.Limit, 50, 100)
	q += " ORDER BY MAX(m.created_ms) DESC,m.thread_id DESC LIMIT ?"
	args = append(args, limit+1)
	rows, err := t.rows(q, args...)
	if err != nil {
		return nil, matchError(err)
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
		return nil, matchError(err)
	}
	page := map[string]any{}
	if len(out) > limit {
		out = out[:limit]
		page["next"] = out[limit-1]["id"]
	}
	page["threads"] = out
	recipientScope, recipientArgs := t.repositoryVisibleSQL("ar.repository_id")
	for _, th := range out {
		names := []string{}
		// Participants of the messages visible here only, not of the whole thread.
		rows, err = t.rows(`SELECT DISTINCT CASE WHEN a.client='human' THEN 'human' ELSE a.name END FROM Agent a WHERE a.id IN (SELECT e.agent_id FROM Message m JOIN Execution e ON e.id=m.sender_execution_id WHERE m.thread_id=? AND `+visible+` UNION SELECT d.recipient_agent_id FROM Delivery d JOIN Message m ON m.id=d.message_id JOIN Agent ar ON ar.id=d.recipient_agent_id WHERE m.thread_id=? AND `+visible+` AND `+recipientScope+`) ORDER BY 1`, append(append(append(append([]any{th["id"]}, visibleArgs...), th["id"]), visibleArgs...), recipientArgs...)...)
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
		if err = t.threadAuthors(th, visible, visibleArgs); err != nil {
			return nil, err
		}
	}
	return page, nil
}
func (t *transaction) thread(id, repo string) (map[string]any, error) {
	visible, args := t.visibleIn("m", repo)
	originScope, originArgs := t.repositoryVisibleSQL("m.repository_id")
	rows, err := t.rows(`SELECT m.id,CASE WHEN a.client='human' THEN 'human' ELSE a.name END,m.subject,m.body,m.created_ms,m.reply_to,CASE WHEN `+originScope+` THEN m.repository_id ELSE '' END,COALESCE(m.sender_account_id,''),COALESCE(m.sender_username,''),COALESCE(m.sender_display_name,'') FROM Message m JOIN Execution e ON e.id=m.sender_execution_id JOIN Agent a ON a.id=e.agent_id WHERE m.thread_id=? AND `+visible+` ORDER BY m.created_ms,m.rowid`, append(append(originArgs, id), args...)...)
	if err != nil {
		return nil, err
	}
	messages := []map[string]any{}
	for rows.Next() {
		var mid, sender, subject, body, repo, accountID, username, displayName string
		var at int64
		var reply sql.NullString
		rows.Scan(&mid, &sender, &subject, &body, &at, &reply, &repo, &accountID, &username, &displayName)
		// The sender's repository, so a thread that crosses repositories shows where each message came from.
		message := map[string]any{"id": mid, "thread_id": id, "sender": sender, "subject": subject, "body": body, "created_at": timestamp(sql.NullInt64{Int64: at, Valid: true}), "reply_to": reply.String}
		if repo != "" {
			message["repository_id"] = repo
			if sender == "human" {
				message["reply_address"] = "human:" + repo
			}
		}
		messageAttribution(accountID, username, displayName, message)
		messages = append(messages, message)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return nil, err
	}
	if len(messages) == 0 {
		return nil, fail("not_found", "unknown thread")
	}
	recipientScope, recipientArgs := t.repositoryVisibleSQL("a.repository_id")
	for _, m := range messages {
		recipients := []map[string]any{}
		rows, err = t.rows(`SELECT CASE WHEN a.client='human' THEN 'human' ELSE a.name END,d.state FROM DeliveryState d JOIN Agent a ON a.id=d.recipient_agent_id WHERE d.message_id=? AND `+recipientScope+` ORDER BY a.name`, append([]any{m["id"]}, recipientArgs...)...)
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

// readThread acknowledges the operator's unread mail in a thread, which is how the human
// inbox's unread count goes down (gap review A13): opening a thread, or replying in it,
// reads it. With a repository, only that repository's human deliveries are read; from the
// whole-hub view, every repository's. There is no way back to unread.
func (t *transaction) readThread(r Request) (map[string]any, error) {
	var n int
	if err := t.row("SELECT COUNT(*) FROM Message WHERE thread_id=?", r.Key).Scan(&n); err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, fail("not_found", "unknown thread")
	}
	rows, err := t.rows(`SELECT d.id,d.attempt,a.repository_id FROM Delivery d JOIN Message m ON m.id=d.message_id JOIN Agent a ON a.id=d.recipient_agent_id
 WHERE m.thread_id=? AND a.client='human' AND (?='' OR a.repository_id=?) AND d.id NOT IN (SELECT delivery_id FROM Ack) ORDER BY d.id`, r.Key, r.RepositoryID, r.RepositoryID)
	if err != nil {
		return nil, err
	}
	type unread struct {
		id, repo string
		attempt  int64
	}
	var all []unread
	for rows.Next() {
		var u unread
		rows.Scan(&u.id, &u.attempt, &u.repo)
		all = append(all, u)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return nil, err
	}
	now := t.s.clock.Now().UnixMilli()
	for _, u := range all {
		operator, err := t.operatorSender(u.repo)
		if err != nil {
			return nil, err
		}
		// An acknowledgement names an attempt above zero, and the operator never fetched
		// this mail through an inbox, so reading it counts as its first attempt.
		attempt := max(u.attempt, 1)
		if _, err = t.exec("UPDATE Delivery SET execution_id=?,attempt=?,attention=0 WHERE id=?", operator.ID, attempt, u.id); err != nil {
			return nil, err
		}
		if _, err = t.exec("INSERT INTO Ack(delivery_id,execution_id,attempt,acked_ms) VALUES(?,?,?,?)", u.id, operator.ID, attempt, now); err != nil {
			return nil, err
		}
	}
	if len(all) > 0 {
		if err = t.event("", "", "changed", ""); err != nil {
			return nil, err
		}
	}
	return map[string]any{"read": len(all)}, nil
}
