package store

import (
	"database/sql"
	"errors"
	"time"
)

func (t *transaction) send(r Request) (map[string]any, error) {
	e, err := t.live(r.ExecutionID, r.Generation)
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
	type recipient struct{ id, name string }
	recipients := []recipient{}
	seen := map[string]bool{}
	for _, name := range r.To {
		var a recipient
		if err = t.row("SELECT id,name FROM Agent WHERE repository_id=? AND (name=? OR id=?)", e.Repo, name, name).Scan(&a.id, &a.name); err != nil {
			return nil, fail("not_found", "recipient is not in this repository")
		}
		if !seen[a.id] {
			recipients = append(recipients, a)
			seen[a.id] = true
		}
	}
	message := id("ms_")
	if _, err = t.exec("INSERT INTO Message VALUES(?,?,?,?,?)", message, e.ID, r.Subject, r.Body, t.s.clock.Now().UnixMilli()); err != nil {
		return nil, err
	}
	deliveries := []map[string]any{}
	for _, a := range recipients {
		delivery := id("dl_")
		if _, err = t.exec("INSERT INTO Delivery(id,message_id,recipient_agent_id) VALUES(?,?,?)", delivery, message, a.id); err != nil {
			return nil, err
		}
		if err = t.event(a.id, "", "inbox_changed", delivery); err != nil {
			return nil, err
		}
		deliveries = append(deliveries, map[string]any{"id": delivery, "recipient": a.name})
	}
	out = map[string]any{"message_id": message, "deliveries": deliveries}
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
	rows, err := t.rows(`SELECT d.id,d.attempt,a.name,m.subject,m.body,m.created_ms FROM Delivery d JOIN Message m ON m.id=d.message_id JOIN Execution sender ON sender.id=m.sender_execution_id JOIN Agent a ON a.id=sender.agent_id WHERE d.recipient_agent_id=? AND d.id NOT IN (SELECT delivery_id FROM Ack) AND (d.execution_id IS NULL OR d.execution_id=?) ORDER BY m.created_ms,d.id LIMIT ?`, e.AgentID, e.ID, boundedLimit(r.Limit, 20, 100))
	if err != nil {
		return nil, err
	}
	out := []map[string]any{}
	for rows.Next() {
		var delivery, sender, subject, body string
		var attempt, created int64
		if err = rows.Scan(&delivery, &attempt, &sender, &subject, &body, &created); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, map[string]any{"id": delivery, "attempt": attempt + 1, "sender": sender, "subject": subject, "body": body, "created_at": time.UnixMilli(created).UTC().Format(time.RFC3339Nano)})
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	for _, d := range out {
		if _, err = t.exec("UPDATE Delivery SET execution_id=?,attempt=? WHERE id=?", e.ID, d["attempt"], d["id"]); err != nil {
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
	var oldExecution string
	var oldAttempt int64
	err = t.row("SELECT execution_id,attempt FROM Ack WHERE delivery_id=?", delivery).Scan(&oldExecution, &oldAttempt)
	if err == nil {
		if oldExecution == e.ID && oldAttempt == r.Attempt {
			return map[string]any{}, nil
		}
		return nil, fail("conflict", "acknowledgement attempt is stale")
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	_, err = t.exec("INSERT INTO Ack VALUES(?,?,?,?)", delivery, e.ID, r.Attempt, t.s.clock.Now().UnixMilli())
	return map[string]any{}, err
}
