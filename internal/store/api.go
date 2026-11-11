package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/mateusz-klatt/iris/internal/buildinfo"
)

// Call dispatches a domain operation. All observations and changes share a single
// BEGIN IMMEDIATE snapshot; only a successful commit becomes visible to readers.
func (s *Store) Call(ctx context.Context, operation string, r Request) (map[string]any, error) {
	var out map[string]any
	var refused *Error
	var change *GenerationChange
	err := s.transact(ctx, func(t *transaction) error {
		if principal, ok := ctx.Value(authorizationKey{}).(Principal); ok {
			if err := t.authorize(principal, operation, r); err != nil {
				return err
			}
		}
		// Expiry runs before every operation except merge, so each one decides on
		// current leases, claims and receipts in its own snapshot. The named operation
		// "expire" runs it too: the hub's stream worker calls it once a second so
		// time-driven changes wake streams when no client calls. With no stream
		// connected, this sweep stops; those transitions wait for another operation.
		// Within Call, only "expire" also cleans up console sessions by their expiry time.
		// A merge plan and a refused merge must leave even expired leases and
		// delivery attempts untouched. The merge checks liveness without expiring.
		if operation != "merge" {
			if err := t.expire(); err != nil {
				return err
			}
		}
		// Long-lived clients may still carry a repository id retired by an
		// operator merge. Their stable Execution determines local read scope.
		if (operation == "claims" || (operation == "agents" && !r.All)) && r.ExecutionID != "" {
			e, err := t.live(r.ExecutionID, r.Generation)
			if err != nil {
				return err
			}
			r.RepositoryID = e.Repo
		}
		var err error
		switch operation {
		case "touch", "claim", "release", "messages", "ack", "end", "heartbeat", "reconcile", "reconcile_scoped", "inbox_receipt":
			// A shell reconcile that names its session registers it first, and a first
			// registration has no generation to give.
			if r.Generation <= 0 && !shellSession(operation, r) {
				return fail("bad_request", "generation is required")
			}
		}
		switch operation {
		case "expire":
			// Session cleanup belongs to shared maintenance, not unrelated domain
			// calls. Authorization independently rejects sessions at their deadline.
			_, err = t.exec(expireConsoleSessionsSQL, t.s.clock.Now().UnixMilli())
			out = map[string]any{}
		case "ui_receipts":
			if t.console == nil {
				return fail("forbidden", "a console account session is required")
			}
			out, err = t.consoleReceipts(r)
		case "ui_overview", "ui_deliveries", "ui_threads", "ui_thread", "ui_read", "ui_messages", "ui_revoke", "ui_doctor_get", "ui_doctor_put", "ui_events":
			if t.console != nil && operation == "ui_messages" {
				out, err = t.consoleSend(r)
			} else if t.console != nil && operation == "ui_events" {
				out, err = t.consoleEvents()
			} else {
				out, err = t.operator(operation, r)
			}
			if err == nil && t.console != nil && (operation == "ui_read" || operation == "ui_revoke") {
				err = t.consoleAction(t.console.ID, "", operation)
			}
		case "reconcile", "reconcile_scoped":
			if err = scopeFits(operation, r); err != nil {
				return err
			}
			if shellSession(operation, r) {
				out, err = t.shellSession(&r)
			} else {
				out, err = t.reconcile(r)
			}
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
		case "inbox_receipt":
			out, err = t.inboxReceipt(r)
		case "ack":
			out, err = t.ack(r, r.Key)
		case "receipts":
			out, err = t.receipts(r, r.Key)
		case "read_body":
			out, err = t.readBody(r, r.Key)
		case "read_message_body":
			out, err = t.readMessageBody(r)
		case "history":
			r.Key = ""
			out, err = t.agentHistory(r)
		case "thread":
			if r.Key == "" {
				return fail("bad_request", "thread id is required")
			}
			out, err = t.agentHistory(r)
		case "agents":
			var v []map[string]any
			if r.All {
				v, err = t.everyAgent()
			} else {
				v, err = t.agents(r.RepositoryID)
			}
			out = map[string]any{"agents": v}
		case "events":
			out, err = t.events(r)
		case "contact":
			out, err = t.contact(r)
		case "hooks_seen":
			out, err = t.hooksSeen(r.Host)
		case "forget":
			out, err = t.forget(r)
		case "merge":
			out, err = t.mergeRepositories(r)
		case "monitor_cursor":
			out, err = t.monitorCursor(r)
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
			// The database grows with mail, which nothing prunes (ADR 006), so its size is shown.
			var bytes int64
			if err = t.row("SELECT page_count*page_size FROM pragma_page_count(),pragma_page_size()").Scan(&bytes); err != nil {
				return err
			}
			out = map[string]any{"version": buildinfo.String(), "stream": s.stream, "boot_epoch": s.epoch, "counts": counts, "database_bytes": bytes}
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
		// Only hooks register with source hook, claim automatically or reconcile a shell, so
		// these say when this client's hooks last reached the hub. A heartbeat does not.
		if err == nil && (operation == "touch" || operation == "reconcile" || operation == "reconcile_scoped" || (operation == "sessions" && r.Source == "hook")) {
			ex := r.ExecutionID
			if operation == "sessions" {
				ex, _ = out["execution_id"].(string)
			}
			_, err = t.exec("UPDATE Execution SET hook_seen_ms=? WHERE id=?", t.s.clock.Now().UnixMilli(), ex)
		}
		if err == nil {
			switch operation {
			case "sessions", "end", "touch", "claim", "release", "messages", "ack":
				err = t.event("", "", "changed", "")
			}
		}
		refused = t.refused
		change = t.generationChange
		return err
	})
	if observe, ok := ctx.Value(generationObserverKey{}).(func(GenerationChange)); ok && err == nil && change != nil {
		// transact has committed and released the store lock: a slow logger must
		// not prevent unrelated sessions from renewing their leases.
		observe(*change)
	}
	if err == nil && refused != nil {
		return nil, refused
	}
	return out, err
}

// pendingDeliveries counts mail that needs its recipient's attention: not fetched yet, or
// fetched by an Execution that is gone. A fetched delivery stays unread until it is
// acknowledged, but while its fetcher lives it needs no one's attention.
const pendingDeliveriesSQL = `SELECT COUNT(*) FROM Delivery WHERE recipient_agent_id=? AND attention=1 AND id NOT IN (SELECT delivery_id FROM Ack)`

func (t *transaction) pendingDeliveries(agent string) (int, error) {
	var n int
	err := t.row(pendingDeliveriesSQL, agent).Scan(&n)
	return n, err
}

// monitorCursor says where a monitor resumes. A new session is a new Execution
// with no cursor, and it must not hear the agent's whole history again, so it
// starts at the newest event and is told only about mail still waiting.
//
// Only the monitor writes its cursor, through events with its consumer id, and it
// does so once it has printed everything up to that point. So a cursor here proves
// the waiting mail behind it was announced. A first connection that fails before
// its reset frame leaves no cursor, and the next one is told about the mail again.
func (t *transaction) monitorCursor(r Request) (map[string]any, error) {
	e, err := t.live(r.ExecutionID, 0)
	if err != nil {
		return nil, err
	}
	consumer := r.ExecutionID + ":iris-watch"
	var cursor int64
	err = t.row("SELECT seq FROM Cursor WHERE consumer_id=?", consumer).Scan(&cursor)
	if err == nil {
		return map[string]any{"cursor": cursor, "pending": 0}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if err = t.row("SELECT COALESCE((SELECT seq FROM sqlite_sequence WHERE name='Event'),0)").Scan(&cursor); err != nil {
		return nil, err
	}
	pending, err := t.pendingDeliveries(e.AgentID)
	if err != nil {
		return nil, err
	}
	return map[string]any{"cursor": cursor, "pending": pending}, nil
}

const agentEventsSQL = `SELECT seq,kind,kind='inbox_changed' AND object_id NOT IN (SELECT id FROM Delivery WHERE attention=1 AND id NOT IN (SELECT delivery_id FROM Ack)) FROM Event WHERE seq>? AND agent_id=? AND (execution_id IS NULL OR execution_id=?) ORDER BY seq LIMIT ?`

func (t *transaction) events(r Request) (map[string]any, error) {
	e, err := t.live(r.ExecutionID, r.Generation)
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
	pending, err := t.pendingDeliveries(e.AgentID)
	if err != nil {
		return nil, err
	}
	reset := (r.Stream != "" && r.Stream != t.s.stream) || ((low > 0 && r.After < low-1) || (low == 0 && r.After < high)) || r.After > high
	out := map[string]any{"stream": t.s.stream, "events": []map[string]any{}, "high_water": high, "pending": pending}
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
		// A delivery that is no longer waiting was already seen, so its event needs no attention.
		rows, err := t.rows(agentEventsSQL, r.After, e.AgentID, e.ID, boundedLimit(r.Limit, 100, 1000))
		if err != nil {
			return nil, err
		}
		events := []map[string]any{}
		for rows.Next() {
			var seq int64
			var kind string
			var settled bool
			rows.Scan(&seq, &kind, &settled)
			event := map[string]any{"cursor": seq, "kind": kind}
			if settled {
				event["settled"] = true
			}
			events = append(events, event)
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return nil, err
		}
		out["events"] = events
	}
	if r.ConsumerID != "" {
		// Namespace consumers by Execution, so names do not allow one stream to move another.
		consumer := fmt.Sprintf("%s:%s", e.ID, r.ConsumerID)
		if _, err = t.execQuiet("INSERT INTO Cursor VALUES(?,?,?) ON CONFLICT(consumer_id) DO UPDATE SET seq=excluded.seq WHERE excluded.seq>Cursor.seq", consumer, e.ID, r.After); err != nil {
			return nil, err
		}
	}
	// Only a read that asks for the attention block receives it, as planned for Codex's wake (ADR
	// 017). A watch socket reads events on every wake but needs no attention block, so it pays
	// nothing for one.
	if r.Attention {
		out["attention"], err = t.attention(e)
	}
	return out, err
}

// PruneEvents removes an acknowledged retention prefix. Reconnecting clients get a
// reset and consistent snapshot when their cursor precedes this prefix.
func (s *Store) PruneEvents(ctx context.Context, through int64) error {
	return s.transact(ctx, func(t *transaction) error { _, err := t.exec("DELETE FROM Event WHERE seq<=?", through); return err })
}
