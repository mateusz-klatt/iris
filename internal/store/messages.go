package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/mateusz-klatt/iris/internal/budget"
	"time"
	"unicode/utf8"
)

// inboxBudget bounds an inbox page's encoded size. A client reads at most 1 MiB of a response,
// and a page of 100 maximal bodies of characters JSON escapes six-fold would be about 10 MiB.
// The largest single delivery is about 100 KiB, so a page always has room for at least one.
const inboxBudget = 512 << 10

func (t *transaction) send(r Request) (map[string]any, error) {
	var e execution
	var err error
	var repo string
	if r.Source == "operator" {
		repo, err = t.operatorRepository(r)
	} else {
		e, err = t.live(r.ExecutionID, r.Generation)
	}
	if err != nil {
		return nil, err
	}
	if len([]byte(r.Body)) > 16384 || len(r.Subject) > 1024 {
		return nil, fail("too_large", "body is limited to 16 KiB and subject to 1024 bytes")
	}
	if r.Source == "operator" {
		out, found, err := t.operatorCached(repo, r)
		if err != nil || found {
			return out, err
		}
		e, err = t.operatorSender(repo)
		if err != nil {
			return nil, err
		}
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
	cross := 0
	for _, a := range recipients {
		if a.repo != e.Repo {
			cross++
		}
	}
	// Counted before the inserts, so a send that then fails on storage still uses its
	// share. That errs on the side of the limit, and the count is in memory, not rolled back.
	if cross > 0 && !t.s.allowCross(principalOf(t.ctx), cross, t.s.clock.Now()) {
		return nil, crossRefused()
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
			return nil, missing(err, "you can reply only to a message you sent or received")
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
	// size counts the page as encoded: the envelope, then each delivery and its comma. A
	// delivery that would pass the budget waits for the next page, unless the page is empty.
	size := len(`{"deliveries":[],"more":false}`)
	for rows.Next() {
		var delivery, sender, subject, body, message, thread, fromRepo, fromName string
		var attempt, created int64
		rows.Scan(&delivery, &attempt, &sender, &subject, &body, &created, &message, &thread, &fromRepo, &fromName)
		item := map[string]any{"id": delivery, "message_id": message, "thread_id": thread, "attempt": attempt + 1, "sender": sender, "subject": subject, "body": body, "created_at": time.UnixMilli(created).UTC().Format(time.RFC3339Nano),
			"from_repository": fromName, "from_repository_id": fromRepo, "cross_repository": fromRepo != e.Repo}
		raw, _ := json.Marshal(item) // strings, numbers and a bool always encode
		if len(out) > 0 && size+len(raw)+1 > inboxBudget {
			break
		}
		size += len(raw) + 1
		out = append(out, item)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return nil, err
	}
	// A fetch that asks for a receipt leaves the mail needing attention until the receipt says
	// the response arrived; one that does not, from a client before ADR 010, quiets it at once.
	due := sql.NullInt64{}
	if r.Receipt {
		due = sql.NullInt64{Int64: t.s.clock.Now().Add(receiptWindow).UnixMilli(), Valid: true}
	}
	for _, d := range out {
		if _, err = t.exec("UPDATE Delivery SET execution_id=?,attempt=?,attention=CASE WHEN ? THEN attention ELSE 0 END,receipt_due_ms=? WHERE id=?", e.ID, d["attempt"], due.Valid, due, d["id"]); err != nil {
			return nil, err
		}
	}
	// more says mail is waiting beyond this page. The page stays the caller's until it is
	// acknowledged, so the next page comes from fetching again after the acknowledgements.
	var waiting int
	if err = t.row(`SELECT COUNT(*) FROM Delivery WHERE recipient_agent_id=? AND id NOT IN (SELECT delivery_id FROM Ack) AND (execution_id IS NULL OR execution_id=?)`, e.AgentID, e.ID).Scan(&waiting); err != nil {
		return nil, err
	}
	return map[string]any{"deliveries": out, "more": waiting > len(out)}, nil
}

// receiptWindow is how long a fetch waits for its receipt before its mail is woken again.
const receiptWindow = 60 * time.Second

// inboxReceipt says the response to a fetch arrived: each named delivery still assigned to this
// Execution at that attempt, and awaiting a receipt, stops needing attention. It is not an
// acknowledgement; only ack settles. Anything else named, an older attempt, another Execution's
// delivery or one already received, changes nothing (ADR 010).
func (t *transaction) inboxReceipt(r Request) (map[string]any, error) {
	e, err := t.live(r.ExecutionID, r.Generation)
	if err != nil {
		return nil, err
	}
	for _, d := range r.Receipts {
		if _, err = t.exec("UPDATE Delivery SET attention=0,receipt_due_ms=NULL WHERE id=? AND execution_id=? AND attempt=? AND receipt_due_ms IS NOT NULL", d.ID, e.ID, d.Attempt); err != nil {
			return nil, err
		}
	}
	return map[string]any{}, nil
}

func (t *transaction) ack(r Request, delivery string) (map[string]any, error) {
	e, err := t.live(r.ExecutionID, r.Generation)
	if err != nil {
		return nil, err
	}
	var recipient string
	var assigned sql.NullString
	var attempt int64
	var acked bool
	if err = t.row("SELECT recipient_agent_id,execution_id,attempt,EXISTS(SELECT 1 FROM Ack WHERE delivery_id=Delivery.id) FROM Delivery WHERE id=?", delivery).Scan(&recipient, &assigned, &attempt, &acked); err != nil {
		return nil, missing(err, "unknown delivery")
	}
	if acked && r.Handled {
		// Handled is said once the work is done, often from a later Execution of the same
		// agent, so any of the recipient's live Executions may say it (ADR 005). It is
		// monotone: a repeat keeps the first time, and nothing takes it back.
		if recipient != e.AgentID {
			return nil, fail("not_found", "unknown delivery")
		}
		_, err = t.exec("UPDATE Ack SET handled_ms=? WHERE delivery_id=? AND handled_ms IS NULL", t.s.clock.Now().UnixMilli(), delivery)
		return map[string]any{}, err
	}
	if recipient != e.AgentID || !assigned.Valid || assigned.String != e.ID || attempt != r.Attempt || attempt <= 0 {
		return nil, fail("conflict", "acknowledgement is stale or belongs to another Execution")
	}
	if acked {
		return map[string]any{}, nil
	}
	now := t.s.clock.Now().UnixMilli()
	handled := sql.NullInt64{Int64: now, Valid: r.Handled}
	if _, err = t.exec("INSERT INTO Ack(delivery_id,execution_id,attempt,acked_ms,handled_ms) VALUES(?,?,?,?,?)", delivery, e.ID, r.Attempt, now, handled); err != nil {
		return nil, err
	}
	// An ack settles the delivery, so its fetch receipt is no longer awaited (ADR 010).
	_, err = t.exec("UPDATE Delivery SET receipt_due_ms=NULL WHERE id=?", delivery)
	return map[string]any{}, err
}

// receipts is what the sender of message may see of it: each recipient's name, repository
// and delivery state, and when it was acked or said handled (ADR 005). No subject or body.
// Any Execution of the sending agent may ask; to anyone else the message does not exist.
func (t *transaction) receipts(r Request, message string) (map[string]any, error) {
	e, err := t.live(r.ExecutionID, r.Generation)
	if err != nil {
		return nil, err
	}
	var sender string
	if err = t.row("SELECT x.agent_id FROM Message m JOIN Execution x ON x.id=m.sender_execution_id WHERE m.id=?", message).Scan(&sender); err != nil {
		return nil, missing(err, "unknown message")
	}
	if sender != e.AgentID {
		return nil, fail("not_found", "unknown message")
	}
	rows, err := t.rows(`SELECT CASE WHEN a.client='human' THEN 'human' ELSE a.name END,a.repository_id,d.state,COALESCE(d.handled_ms,d.acked_ms) FROM DeliveryState d JOIN Agent a ON a.id=d.recipient_agent_id WHERE d.message_id=? ORDER BY a.name`, message)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	recipients := []map[string]any{}
	for rows.Next() {
		var name, repo, state string
		var at sql.NullInt64
		rows.Scan(&name, &repo, &state, &at)
		recipients = append(recipients, map[string]any{"recipient": name, "repository_id": repo, "state": state, "at": timestamp(at)})
	}
	return map[string]any{"message_id": message, "recipients": recipients}, rows.Err()
}

// readBody returns part of one delivery's body, from byte offset r.After, at most r.Limit bytes
// cut at a UTF-8 boundary, for a model whose client limits what a tool result may hold (ADR 008).
// Only the recipient may read it, and reading changes no attempt and settles nothing: only an
// ack does. next_offset is set while more remains.
func (t *transaction) readBody(r Request, delivery string) (map[string]any, error) {
	e, err := t.live(r.ExecutionID, 0)
	if err != nil {
		return nil, err
	}
	var recipient, sender, body string
	err = t.row(`SELECT d.recipient_agent_id,CASE WHEN a.client='human' THEN 'human' ELSE a.name END,m.body FROM Delivery d JOIN Message m ON m.id=d.message_id JOIN Execution x ON x.id=m.sender_execution_id JOIN Agent a ON a.id=x.agent_id WHERE d.id=?`, delivery).Scan(&recipient, &sender, &body)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && recipient != e.AgentID) {
		return nil, fail("not_found", "unknown delivery")
	}
	if err != nil {
		return nil, err
	}
	r.Key = delivery
	return bodyPart(r, "delivery_id", sender, body)
}

func bodyPart(r Request, idField, sender, body string) (map[string]any, error) {
	if r.After < 0 || r.After > int64(len(body)) {
		return nil, fail("bad_request", "offset is outside the body")
	}
	start := runeStart(body, int(r.After))
	part := budget.Cut(body[start:], boundedLimit(r.Limit, budget.Bodies, budget.Bodies))
	out := map[string]any{idField: r.Key, "sender": sender, "body": part, "body_bytes": len(body), "offset": start}
	if end := start + len(part); end < len(body) {
		out["next_offset"] = end
	}
	return out, nil
}

// runeStart moves i back to the start of the character it falls in.
func runeStart(s string, i int) int {
	for i > 0 && i < len(s) && !utf8.RuneStart(s[i]) {
		i--
	}
	return i
}
