package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"time"
)

// Agent visibility is deliberately narrower than the operator's repository view.
// A visible message never grants access to the rest of its thread (ADR 011).
const agentMessage = `(x.agent_id=? OR EXISTS (SELECT 1 FROM Delivery d WHERE d.message_id=m.id AND d.recipient_agent_id=?))`
const historyFrom = ` FROM Message m JOIN Execution x ON x.id=m.sender_execution_id JOIN Agent a ON a.id=x.agent_id LEFT JOIN Repository r ON r.id=m.repository_id WHERE ` + agentMessage

type historyCursor struct {
	Version int
	Scope   [32]byte
	ID      string
	At      int64
}

func (t *transaction) agentHistory(r Request) (map[string]any, error) {
	e, err := t.live(r.ExecutionID, 0)
	if err != nil {
		return nil, err
	}
	// Bind the cursor to the view without copying an arbitrarily long search into it.
	rawScope, _ := json.Marshal([]string{e.AgentID, r.Q, r.Key})
	scope := sha256.Sum256(rawScope)
	cursor := historyCursor{Version: 1, Scope: scope}
	if r.Before != "" {
		raw, err := base64.RawURLEncoding.DecodeString(r.Before)
		var decoded historyCursor
		if err != nil || json.Unmarshal(raw, &decoded) != nil || decoded.Version != 1 || decoded.Scope != scope || decoded.ID == "" {
			return nil, fail("bad_request", "invalid history cursor or changed scope")
		}
		cursor = decoded
	}
	if r.Key != "" {
		var exists bool
		if err := t.row(`SELECT EXISTS(SELECT 1`+historyFrom+` AND m.thread_id=?)`, e.AgentID, e.AgentID, r.Key).Scan(&exists); err != nil {
			return nil, err
		}
		if !exists {
			return nil, fail("not_found", "unknown thread")
		}
	}
	q := `SELECT m.id,m.thread_id,CASE WHEN a.client='human' THEN 'human' ELSE a.name END,m.subject,m.body,m.created_ms,m.repository_id,COALESCE(r.name,''),x.agent_id=?` + historyFrom
	args := []any{e.AgentID, e.AgentID, e.AgentID}
	if r.Key != "" {
		q += ` AND m.thread_id=?`
		args = append(args, r.Key)
	}
	if match := literalMatch(r.Q); match != "" {
		q += ` AND m.rowid IN (SELECT rowid FROM MessageSearch WHERE MessageSearch MATCH ?)`
		args = append(args, match)
	}
	if r.Before != "" {
		q += ` AND (m.created_ms<? OR (m.created_ms=? AND m.id<?))`
		args = append(args, cursor.At, cursor.At, cursor.ID)
	}
	limit := boundedLimit(r.Limit, 20, 100)
	q += ` ORDER BY m.created_ms DESC,m.id DESC LIMIT ?`
	rows, err := t.rows(q, append(args, limit+1)...)
	if err != nil {
		return nil, matchError(err)
	}
	defer func() { _ = rows.Close() }()
	items := []map[string]any{}
	size := len(`{"messages":[],"next":""}`)
	next := ""
	for rows.Next() {
		var id, thread, sender, subject, body, repo, repoName string
		var created int64
		var sent bool
		rows.Scan(&id, &thread, &sender, &subject, &body, &created, &repo, &repoName, &sent)
		item := map[string]any{"message_id": id, "thread_id": thread, "sender": sender, "subject": subject, "body": body, "created_at": time.UnixMilli(created).UTC().Format(time.RFC3339Nano), "from_repository_id": repo, "from_repository": repoName, "cross_repository": repo != e.Repo, "sent": sent}
		raw, _ := json.Marshal(item)
		candidate := cursor
		candidate.At, candidate.ID = created, id
		cursorRaw, _ := json.Marshal(candidate)
		if len(items) == limit || (len(items) > 0 && size+len(raw)+1+base64.RawURLEncoding.EncodedLen(len(cursorRaw)) > inboxBudget) {
			raw, _ = json.Marshal(cursor)
			next = base64.RawURLEncoding.EncodeToString(raw)
			break
		}
		size += len(raw) + 1
		items = append(items, item)
		cursor = candidate
	}
	return map[string]any{"messages": items, "next": next}, matchError(rows.Err())
}

func (t *transaction) readMessageBody(r Request) (map[string]any, error) {
	e, err := t.live(r.ExecutionID, 0)
	if err != nil {
		return nil, err
	}
	var sender, body string
	err = t.row(`SELECT CASE WHEN a.client='human' THEN 'human' ELSE a.name END,m.body`+historyFrom+` AND m.id=?`, e.AgentID, e.AgentID, r.Key).Scan(&sender, &body)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fail("not_found", "unknown message")
	}
	if err != nil {
		return nil, err
	}
	return bodyPart(r, "message_id", sender, body)
}
