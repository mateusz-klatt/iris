package store

import (
	"context"
	"fmt"
)

// Call dispatches a domain operation. All observations and changes share a single
// BEGIN IMMEDIATE snapshot; only a successful commit becomes visible to readers.
func (s *Store) Call(ctx context.Context, operation string, r Request) (map[string]any, error) {
	var out map[string]any
	err := s.transact(ctx, func(t *transaction) error {
		if err := t.expire(); err != nil {
			return err
		}
		var err error
		switch operation {
		case "touch", "claim", "release", "messages", "ack", "end", "heartbeat", "reconcile":
			if r.Generation <= 0 {
				return fail("bad_request", "generation is required")
			}
		}
		switch operation {
		case "ui_overview", "ui_threads", "ui_thread", "ui_messages", "ui_revoke", "ui_doctor_get", "ui_doctor_put", "ui_events":
			out, err = t.operator(operation, r)
		case "reconcile":
			out, err = t.reconcile(r)
		case "check":
			out, err = t.check(r)
		case "sessions":
			out, err = t.session(r)
		case "bind":
			out, err = t.bind(r)
		case "heartbeat":
			out, err = t.heartbeat(r)
		case "end":
			out, err = t.end(r)
		case "touch":
			out, err = t.touch(r)
		case "claim":
			out, err = t.explicit(r)
		case "release":
			out, err = t.release(r)
		case "claims":
			var v []map[string]any
			v, err = t.claims(r.RepositoryID)
			out = map[string]any{"claims": v}
		case "messages":
			r.Source = ""
			out, err = t.send(r)
		case "inbox":
			out, err = t.inbox(r)
		case "ack":
			out, err = t.ack(r, r.Key)
		case "agents":
			var v []map[string]any
			v, err = t.agents(r.RepositoryID)
			out = map[string]any{"agents": v}
		case "events":
			out, err = t.events(r)
		case "monitor_cursor":
			var cursor int64
			err = t.row("SELECT COALESCE((SELECT seq FROM Cursor WHERE consumer_id=?),0)", r.ExecutionID+":iris-watch").Scan(&cursor)
			out = map[string]any{"cursor": cursor}
		case "validate":
			var e execution
			e, err = t.live(r.ExecutionID, r.Generation)
			out = bound(e)
		case "status":
			counts := map[string]int{}
			for _, table := range []string{"Agent", "Execution", "Claim", "Message", "Delivery"} {
				var n int
				if err = t.row("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
					return err
				}
				counts[table] = n
			}
			out = map[string]any{"version": "dev", "stream": s.stream, "boot_epoch": s.epoch, "counts": counts}
		default:
			return fail("not_found", "unknown operation")
		}
		if err == nil && (operation == "sessions" || operation == "heartbeat") {
			ex := r.ExecutionID
			if operation == "sessions" {
				ex, _ = out["execution_id"].(string)
			}
			_, err = t.exec("UPDATE Execution SET last_seen_ms=? WHERE id=?", t.s.clock.Now().UnixMilli(), ex)
		}
		if err == nil {
			switch operation {
			case "sessions", "end", "touch", "claim", "release", "messages", "ack":
				err = t.event("", "", "changed", "")
			}
		}
		return err
	})
	return out, err
}
func (t *transaction) events(r Request) (map[string]any, error) {
	e, err := t.live(r.ExecutionID, 0)
	if err != nil {
		return nil, err
	}
	if r.After < 0 {
		return nil, fail("bad_request", "negative event cursor")
	}
	var high, low int64
	// sqlite_sequence retains the high-water mark even when every event was pruned.
	if err = t.row("SELECT COALESCE((SELECT seq FROM sqlite_sequence WHERE name='Event'),0),COALESCE(MIN(seq),0) FROM Event").Scan(&high, &low); err != nil {
		return nil, err
	}
	reset := (r.Stream != "" && r.Stream != t.s.stream) || ((low > 0 && r.After < low-1) || (low == 0 && r.After < high)) || r.After > high
	out := map[string]any{"stream": t.s.stream, "events": []map[string]any{}, "high_water": high}
	if reset {
		claims, err := t.claims(e.Repo)
		if err != nil {
			return nil, err
		}
		var unread int
		if err = t.row("SELECT COUNT(*) FROM Delivery WHERE recipient_agent_id=? AND id NOT IN (SELECT delivery_id FROM Ack)", e.AgentID).Scan(&unread); err != nil {
			return nil, err
		}
		out["reset"] = true
		out["snapshot"] = map[string]any{"claims": claims, "unread": unread}
	} else {
		rows, err := t.rows("SELECT seq,kind FROM Event WHERE seq>? AND agent_id=? AND (execution_id IS NULL OR execution_id=?) ORDER BY seq LIMIT ?", r.After, e.AgentID, e.ID, boundedLimit(r.Limit, 100, 1000))
		if err != nil {
			return nil, err
		}
		events := []map[string]any{}
		for rows.Next() {
			var seq int64
			var kind string
			rows.Scan(&seq, &kind)
			events = append(events, map[string]any{"cursor": seq, "kind": kind})
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
		out["events"] = events
	}
	if r.ConsumerID != "" {
		// Namespace consumers by Execution, so names do not allow one stream to move another.
		consumer := fmt.Sprintf("%s:%s", e.ID, r.ConsumerID)
		if _, err = t.exec("INSERT INTO Cursor VALUES(?,?,?) ON CONFLICT(consumer_id) DO UPDATE SET seq=MAX(Cursor.seq,excluded.seq)", consumer, e.ID, r.After); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// PruneEvents removes an acknowledged retention prefix. Reconnecting clients get a
// reset and consistent snapshot when their cursor precedes this prefix.
func (s *Store) PruneEvents(ctx context.Context, through int64) error {
	return s.transact(ctx, func(t *transaction) error { _, err := t.exec("DELETE FROM Event WHERE seq<=?", through); return err })
}
