package store

import (
	"database/sql"
	"time"
)

// deliveryWindow is how far back the delivery-problems list looks at the human's own mail.
const deliveryWindow = 7 * 24 * time.Hour

// deliveryWait is how long an unacked delivery to a reachable recipient may wait before the list
// names it. It is a threshold, not a verdict: the recipient may simply be busy.
const deliveryWait = 15 * time.Minute

// deliveries lists the human's messages of the last deliveryWindow with a delivery in trouble:
// unacked, to a recipient with no active Execution ("unreachable"), or unacked for longer than
// deliveryWait ("waiting"). An acked or handled delivery is never listed, so nothing stored is
// offered again just because nobody has said it was handled. With a repository, only messages
// the human sent from that repository's view are listed. No body is returned.
func (t *transaction) deliveries(r Request) (map[string]any, error) {
	now := t.s.clock.Now()
	rows, err := t.rows(`SELECT m.id,m.thread_id,m.subject,m.created_ms,a.name,d.state,
 COALESCE((SELECT x.state FROM Execution x WHERE x.agent_id=a.id ORDER BY x.rowid DESC LIMIT 1),'none'),
 EXISTS(SELECT 1 FROM Execution x WHERE x.agent_id=a.id AND x.state='active')
 FROM Message m JOIN Execution e ON e.id=m.sender_execution_id
 JOIN DeliveryState d ON d.message_id=m.id JOIN Agent a ON a.id=d.recipient_agent_id
 WHERE e.client='human' AND a.client<>'human' AND d.state IN ('pending','inflight') AND m.created_ms>=?
 AND (?='' OR m.repository_id=?)
 ORDER BY m.created_ms DESC,m.id,a.name`, now.Add(-deliveryWindow).UnixMilli(), r.RepositoryID, r.RepositoryID)
	if err != nil {
		return nil, err
	}
	messages := []map[string]any{}
	byID := map[string]map[string]any{}
	for rows.Next() {
		var id, thread, subject, name, state, latest string
		var created int64
		var reachable bool
		rows.Scan(&id, &thread, &subject, &created, &name, &state, &latest, &reachable)
		reason := "unreachable"
		if reachable {
			if now.Sub(time.UnixMilli(created)) <= deliveryWait {
				continue
			}
			reason = "waiting"
		}
		m := byID[id]
		if m == nil {
			m = map[string]any{"id": id, "thread_id": thread, "subject": subject, "created_at": timestamp(sql.NullInt64{Int64: created, Valid: true}), "recipients": []map[string]any{}}
			byID[id] = m
			messages = append(messages, m)
		}
		// execution is the recipient's latest Execution state: active, lost, ended, or none.
		m["recipients"] = append(m["recipients"].([]map[string]any), map[string]any{"name": name, "state": state, "reason": reason, "execution": latest})
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return nil, err
	}
	return map[string]any{"messages": messages, "window_s": int64(deliveryWindow / time.Second), "wait_s": int64(deliveryWait / time.Second)}, nil
}
