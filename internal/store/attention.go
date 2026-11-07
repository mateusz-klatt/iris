package store

import "time"

// Attention is what a session has not been told yet, for the hooks of clients without a
// monitor (D7's next-turn tier, gap review A4). The counts only grow while nothing is
// handled, so a hook can say each new event once by remembering what it last announced.

// watchFresh is how long a watch heartbeat shows a live monitor. iris watch beats every 20 s.
const watchFresh = 90 * time.Second

// attention counts e's deliveries that still need a wake (Delivery.attention, gap A10) and the claim_available events addressed
// to it, and says whether an iris watch for e beat recently. Where one does, the monitor
// already wakes the session, so a hook stays quiet rather than say the same thing twice.
func (t *transaction) attention(e execution) (map[string]any, error) {
	var unread, available int
	var watched int64
	if err := t.row("SELECT COUNT(*) FROM Delivery WHERE recipient_agent_id=? AND attention=1 AND id NOT IN (SELECT delivery_id FROM Ack)", e.AgentID).Scan(&unread); err != nil {
		return nil, err
	}
	if err := t.row("SELECT COUNT(*) FROM Event WHERE kind='claim_available' AND execution_id=?", e.ID).Scan(&available); err != nil {
		return nil, err
	}
	if err := t.row("SELECT watch_seen_ms FROM Execution WHERE id=?", e.ID).Scan(&watched); err != nil {
		return nil, err
	}
	monitored := watched > 0 && t.s.clock.Now().UnixMilli()-watched < watchFresh.Milliseconds()
	return map[string]any{"unread": unread, "available": available, "monitored": monitored}, nil
}

// watchSeen records a heartbeat from e's iris watch.
func (t *transaction) watchSeen(id string) error {
	_, err := t.exec("UPDATE Execution SET watch_seen_ms=? WHERE id=?", t.s.clock.Now().UnixMilli(), id)
	return err
}
