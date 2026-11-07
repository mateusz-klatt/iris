package store

import (
	"database/sql"
	"errors"
	"time"
)

func (t *transaction) send(r Request) (map[string]any, error) {
	var e execution
	var err error
	if r.Source == "operator" {
		e, err = t.humanSender(r)
	} else {
		e, err = t.live(r.ExecutionID, r.Generation)
	}
	if err != nil {
		return nil, err
	}
	if len([]byte(r.Body)) > 16384 || len(r.Subject) > 1024 {
		return nil, fail("too_large", "body is limited to 16 KiB and subject to 1024 bytes")
	}
	out, found, err := t.cached(e, r.Key, "message", r)
	if err != nil || found {
		return out, err
	}
	if len(r.To) == 0 || len(r.To) > 100 {
		return nil, fail("bad_request", "provide 1 to 100 recipients")
	}
	// Addresses are resolved after the retry check, so a retry returns the deliveries
	// it made the first time even if a repository's contact has changed since (ADR 004).
	recipients := []recipient{}
	seen := map[string]bool{}
	for _, name := range r.To {
		a, err := t.resolve(e, name)
		if err != nil {
			return nil, err
		}
		if !seen[a.id] {
			recipients = append(recipients, a)
			seen[a.id] = true
		}
	}
	message := id("ms_")
	thread := message
	if r.ReplyTo != "" {
		// A reply follows its thread across repositories, but only someone who wrote or
		// received the message may reply to it. The operator oversees a repository, so it
		// may reply to any message visible there.
		eligible := `(m.sender_execution_id IN (SELECT id FROM Execution WHERE agent_id=?) OR EXISTS (SELECT 1 FROM Delivery d WHERE d.message_id=m.id AND d.recipient_agent_id=?))`
		args := []any{r.ReplyTo, e.AgentID, e.AgentID}
		if r.Source == "operator" {
			var more []any
			eligible, more = visibleIn("m", e.Repo)
			args = append([]any{r.ReplyTo}, more...)
		}
		if err = t.row(`SELECT m.thread_id FROM Message m WHERE m.id=? AND `+eligible, args...).Scan(&thread); err != nil {
			return nil, fail("not_found", "you can reply only to a message you sent or received")
		}
	}
	if _, err = t.exec("INSERT INTO Message(id,sender_execution_id,subject,body,created_ms,repository_id,thread_id,reply_to) VALUES(?,?,?,?,?,?,?,NULLIF(?,''))", message, e.ID, r.Subject, r.Body, t.s.clock.Now().UnixMilli(), e.Repo, thread, r.ReplyTo); err != nil {
		return nil, err
	}
	deliveries := []map[string]any{}
	for _, a := range recipients {
		delivery := id("dl_")
		if _, err = t.exec("INSERT INTO Delivery(id,message_id,recipient_agent_id,attention) VALUES(?,?,?,1)", delivery, message, a.id); err != nil {
			return nil, err
		}
		if err = t.event(a.id, "", "inbox_changed", delivery); err != nil {
			return nil, err
		}
		d := map[string]any{"id": delivery, "recipient": a.name, "repository_id": a.repo}
		if a.name != "human" {
			// The sender learns that nobody can take the mail now: it waits for the next session.
			live, err := t.reachable(a.id)
			if err != nil {
				return nil, err
			}
			d["queued"] = !live
		}
		deliveries = append(deliveries, d)
	}
	out = map[string]any{"message_id": message, "thread_id": thread, "deliveries": deliveries}
	return out, t.remember(e, r.Key, "message", r, out)
}
func boundedLimit(n, def, max int) int {
	if n <= 0 {
		return def
	}
	if n > max {
		return max
	}
	return n
}
func (t *transaction) inbox(r Request) (map[string]any, error) {
	e, err := t.live(r.ExecutionID, 0)
	if err != nil {
		return nil, err
	}
	// The sender's repository comes from its Execution, never from the message (ADR 004).
	rows, err := t.rows(`SELECT d.id,d.attempt,CASE WHEN a.client='human' THEN 'human' ELSE a.name END,m.subject,m.body,m.created_ms,m.id,m.thread_id,m.repository_id,COALESCE(r.name,'') FROM Delivery d JOIN Message m ON m.id=d.message_id JOIN Execution sender ON sender.id=m.sender_execution_id JOIN Agent a ON a.id=sender.agent_id LEFT JOIN Repository r ON r.id=m.repository_id WHERE d.recipient_agent_id=? AND d.id NOT IN (SELECT delivery_id FROM Ack) AND (d.execution_id IS NULL OR d.execution_id=?) ORDER BY m.created_ms,d.id LIMIT ?`, e.AgentID, e.ID, boundedLimit(r.Limit, 20, 100))
	if err != nil {
		return nil, err
	}
	out := []map[string]any{}
	for rows.Next() {
		var delivery, sender, subject, body, message, thread, fromRepo, fromName string
		var attempt, created int64
		rows.Scan(&delivery, &attempt, &sender, &subject, &body, &created, &message, &thread, &fromRepo, &fromName)
		out = append(out, map[string]any{"id": delivery, "message_id": message, "thread_id": thread, "attempt": attempt + 1, "sender": sender, "subject": subject, "body": body, "created_at": time.UnixMilli(created).UTC().Format(time.RFC3339Nano),
			"from_repository": fromName, "from_repository_id": fromRepo, "cross_repository": fromRepo != e.Repo})
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return nil, err
	}
	for _, d := range out {
		if _, err = t.exec("UPDATE Delivery SET execution_id=?,attempt=?,attention=0 WHERE id=?", e.ID, d["attempt"], d["id"]); err != nil {
			return nil, err
		}
	}
	return map[string]any{"deliveries": out}, nil
}
func (t *transaction) ack(r Request, delivery string) (map[string]any, error) {
	e, err := t.live(r.ExecutionID, r.Generation)
	if err != nil {
		return nil, err
	}
	var recipient string
	var assigned sql.NullString
	var attempt int64
	if err = t.row("SELECT recipient_agent_id,execution_id,attempt FROM Delivery WHERE id=?", delivery).Scan(&recipient, &assigned, &attempt); err != nil {
		return nil, fail("not_found", "unknown delivery")
	}
	if recipient != e.AgentID || !assigned.Valid || assigned.String != e.ID || attempt != r.Attempt || attempt <= 0 {
		return nil, fail("conflict", "acknowledgement is stale or belongs to another Execution")
	}
	var acknowledged int
	err = t.row("SELECT 1 FROM Ack WHERE delivery_id=?", delivery).Scan(&acknowledged)
	if err == nil {
		return map[string]any{}, nil
	}

	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	_, err = t.exec("INSERT INTO Ack VALUES(?,?,?,?)", delivery, e.ID, r.Attempt, t.s.clock.Now().UnixMilli())
	return map[string]any{}, err
}
