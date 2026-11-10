package store

import (
	"database/sql"
	"time"
)

// Attention is what is pending for a session, for the hooks of clients without a monitor (D7's
// next-turn tier, gap review A4) and for Codex's planned wake (ADR 017). The counts are the text
// of a note. The planned hook and wake changes will use the identity to detect attention that is
// new since the last announcement.

// watchFresh is how long a watch heartbeat shows a live monitor. iris watch beats every 20 s.
const watchFresh = 90 * time.Second

// pendingMailSQL counts e's deliveries that still need a wake (Delivery.attention, gap A10) and
// gives the newest cursor among them, with the event sequence's high-water mark. Every such
// delivery carries the cursor of its newest inbox_changed event (attention_seq), which outlives
// the event. sqlite_sequence keeps the high-water mark after retention deletes every event.
const pendingMailSQL = `SELECT COUNT(*),MAX(attention_seq),COALESCE((SELECT seq FROM sqlite_sequence WHERE name='Event'),0) FROM Delivery WHERE recipient_agent_id=? AND attention=1 AND id NOT IN (SELECT delivery_id FROM Ack)`

// Migration 023 gives a cursor to mail that still needs attention. Each such delivery first takes
// the event sequence's high-water mark. If any inbox_changed event for it survived retention, it
// then takes the newest one's cursor, so mail whose events are gone reads as new once. The
// migration gathers the newest events in one pass over Event and does not read them again for
// each delivery.
const (
	attentionHighWaterSQL   = `UPDATE Delivery SET attention_seq=COALESCE((SELECT seq FROM sqlite_sequence WHERE name='Event'),0) WHERE attention=1 AND id NOT IN (SELECT delivery_id FROM Ack)`
	attentionNewestEventSQL = `UPDATE Delivery SET attention_seq=newest.seq FROM (SELECT object_id,MAX(seq) AS seq FROM Event WHERE kind='inbox_changed' GROUP BY object_id) AS newest
 WHERE newest.object_id=Delivery.id AND Delivery.attention=1 AND Delivery.id NOT IN (SELECT delivery_id FROM Ack)`
)

// freedPathsSQL counts the claim_available events addressed to e and gives the newest cursor.
// Such an event always carries e's agent, which the index on (agent_id, seq) uses. It stays
// until retention removes it, even after the path was taken: availability is historical.
const freedPathsSQL = `SELECT COUNT(*),MAX(seq) FROM Event WHERE agent_id=? AND kind='claim_available' AND execution_id=?`

// attention counts e's mail that needs a wake and the paths freed for it, says whether an iris
// watch for e beat recently, and names the newest pending event. Where a monitor beats, it
// already wakes the session, so a hook stays quiet rather than say the same thing twice.
//
// identity is the hub's stream with the largest cursor of that pending mail and those freed
// paths, or null when there is none. A hub that knows identities always sends the field, null
// included, so a client can tell it from an older hub, which sends none (ADR 017, decision 4).
// high_water is the event sequence's high-water mark. A restore keeps the stream and takes the
// sequence back, so a remembered cursor above it means the hub went back (ADR 006, decision 7).
func (t *transaction) attention(e execution) (map[string]any, error) {
	var unread, available int
	var mail, paths sql.NullInt64
	var watched, high int64
	if err := t.row(pendingMailSQL, e.AgentID).Scan(&unread, &mail, &high); err != nil {
		return nil, err
	}
	if err := t.row(freedPathsSQL, e.AgentID, e.ID).Scan(&available, &paths); err != nil {
		return nil, err
	}
	if err := t.row("SELECT watch_seen_ms FROM Execution WHERE id=?", e.ID).Scan(&watched); err != nil {
		return nil, err
	}
	monitored := watched > 0 && t.s.clock.Now().UnixMilli()-watched < watchFresh.Milliseconds()
	var identity any
	if mail.Valid || paths.Valid {
		identity = map[string]any{"stream": t.s.stream, "cursor": max(mail.Int64, paths.Int64)}
	}
	return map[string]any{"unread": unread, "available": available, "monitored": monitored, "identity": identity, "high_water": high}, nil
}

// watchSeen records a heartbeat from e's iris watch.
func (t *transaction) watchSeen(id string) error {
	_, err := t.exec("UPDATE Execution SET watch_seen_ms=? WHERE id=?", t.s.clock.Now().UnixMilli(), id)
	return err
}
