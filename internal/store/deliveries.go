package store

import (
	"database/sql"
	"time"
)

// deliveryWindow is how far back the delivery-problems list looks at the human's own mail.
const deliveryWindow = 7 * 24 * time.Hour

// deliveryWait is the age from message creation after which an unacked delivery to a reachable
// recipient appears in the list. It is a threshold, not a verdict: the recipient may be busy.
const deliveryWait = 15 * time.Minute

// deliveryMessageLimit bounds the newest messages with at least one recipient in trouble.
const deliveryMessageLimit = 200

// deliveries lists the human's messages of the last deliveryWindow with a delivery in trouble:
// unacked, to a recipient with no active Execution ("unreachable"), or from a message created
// longer than deliveryWait ago ("waiting"). An acked or handled delivery is never listed, so
// nothing stored is offered again just because nobody has said it was handled. The legacy
// repository filter selects the sending view. Named accounts see the permitted message
// envelopes and recipients, including an incoming delivery in their selected view. No body
// is returned.
func (t *transaction) deliveries(r Request) (map[string]any, error) {
	now := t.s.clock.Now()
	visible, visibleArgs := t.visibleIn("m", r.RepositoryID)
	recipientScope, recipientArgs := t.repositoryVisibleSQL("a.repository_id")
	originFilter, originArgs := "(?='' OR m.repository_id=?)", []any{r.RepositoryID, r.RepositoryID}
	if t.console != nil {
		originFilter, originArgs = "1=1", nil
	}
	rows, err := t.rows(`SELECT m.id,m.thread_id,m.subject,m.created_ms,a.name,d.state,
 COALESCE((SELECT x.state FROM Execution x WHERE x.agent_id=a.id ORDER BY x.rowid DESC LIMIT 1),'none'),
 EXISTS(SELECT 1 FROM Execution x WHERE x.agent_id=a.id AND x.state='active'),
 COALESCE(m.sender_account_id,''),COALESCE(m.sender_username,''),COALESCE(m.sender_display_name,'')
 FROM Message m JOIN Execution e ON e.id=m.sender_execution_id
 JOIN DeliveryState d ON d.message_id=m.id JOIN Agent a ON a.id=d.recipient_agent_id
 WHERE e.client='human' AND a.client<>'human' AND d.state IN ('pending','inflight') AND m.created_ms>=?
 AND `+originFilter+` AND `+visible+` AND `+recipientScope+`
 ORDER BY m.created_ms DESC,m.id,a.name`, append(append(append([]any{now.Add(-deliveryWindow).UnixMilli()}, originArgs...), visibleArgs...), recipientArgs...)...)
	if err != nil {
		return nil, err
	}
	messages := []map[string]any{}
	byID := map[string]map[string]any{}
	more := false
	for rows.Next() {
		var id, thread, subject, name, state, latest, accountID, username, displayName string
		var created int64
		var reachable bool
		rows.Scan(&id, &thread, &subject, &created, &name, &state, &latest, &reachable, &accountID, &username, &displayName)
		reason := "unreachable"
		if reachable {
			if now.Sub(time.UnixMilli(created)) <= deliveryWait {
				continue
			}
			reason = "waiting"
		}
		m := byID[id]
		if m == nil {
			// Rows are grouped by message. Only an additional message with a recipient
			// in trouble proves truncation; every listed message keeps all its recipients.
			if len(messages) == deliveryMessageLimit {
				more = true
				break
			}
			m = map[string]any{"id": id, "thread_id": thread, "subject": subject, "created_at": timestamp(sql.NullInt64{Int64: created, Valid: true}), "recipients": []map[string]any{}}
			messageAttribution(accountID, username, displayName, m)
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
	return map[string]any{"messages": messages, "more": more, "window_s": int64(deliveryWindow / time.Second), "wait_s": int64(deliveryWait / time.Second)}, nil
}
