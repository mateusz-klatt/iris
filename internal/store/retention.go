package store

import (
	"context"
	"fmt"
	"io"
	"time"
)

// Retention horizons (ADR 006). A client keeps a retry key for at most ten minutes, so a day of
// idempotency records always covers a retry.
const (
	eventHorizon     = 7 * 24 * time.Hour
	eventSettled     = time.Hour
	operationHorizon = 24 * time.Hour
	finishedHorizon  = 30 * 24 * time.Hour
)

// Sweep deletes what ADR 006 no longer keeps, in one transaction: events older than 7 days, or
// over an hour old and passed by every live monitor's saved cursor; idempotency records over a
// day old; claims finished more than 30 days ago; and Executions not active and last seen more
// than 30 days ago that no message, delivery, acknowledgement, live claim or other Execution
// refers to, with their cursors, waiters and shell snapshots. Mail is never deleted.
func (s *Store) Sweep(ctx context.Context) (map[string]int64, error) {
	removed := map[string]int64{}
	err := s.transact(ctx, func(t *transaction) error {
		now := t.s.clock.Now()
		ms := func(d time.Duration) int64 { return now.Add(-d).UnixMilli() }
		steps := []struct {
			name, query string
			args        []any
		}{
			{"events", `DELETE FROM Event WHERE created_ms<? OR (created_ms<? AND seq<=(SELECT MIN(c.seq) FROM Cursor c JOIN Execution e ON e.id=c.execution_id WHERE e.state='active'))`, []any{ms(eventHorizon), ms(eventSettled)}},
			{"operations", `DELETE FROM Operation WHERE created_ms<?`, []any{ms(operationHorizon)}},
			{"claims", `DELETE FROM Claim WHERE COALESCE(revoked_ms,expires_ms)<?`, []any{ms(finishedHorizon)}},
		}
		for _, step := range steps {
			res, err := t.exec(step.query, step.args...)
			if err != nil {
				return err
			}
			removed[step.name], _ = res.RowsAffected() // modernc's sqlite always reports it
		}
		// An Execution is history while mail or another Execution names it, so it stays.
		const gone = `SELECT id FROM Execution x WHERE x.state<>'active' AND x.last_seen_ms<?
 AND NOT EXISTS (SELECT 1 FROM Message WHERE sender_execution_id=x.id)
 AND NOT EXISTS (SELECT 1 FROM Delivery WHERE execution_id=x.id)
 AND NOT EXISTS (SELECT 1 FROM Ack WHERE execution_id=x.id)
 AND NOT EXISTS (SELECT 1 FROM Claim WHERE execution_id=x.id)
 AND NOT EXISTS (SELECT 1 FROM Execution c WHERE c.parent_id=x.id)`
		cutoff := ms(finishedHorizon)
		for _, table := range []string{"Cursor", "Waiter", "ShellSnapshot", "Event"} {
			if _, err := t.exec("DELETE FROM "+table+" WHERE execution_id IN ("+gone+")", cutoff); err != nil {
				return err
			}
		}
		res, err := t.exec("DELETE FROM Execution WHERE id IN ("+gone+")", cutoff)
		if err != nil {
			return err
		}
		removed["executions"], _ = res.RowsAffected()
		return nil
	})
	return removed, err
}

// RunSweeps sweeps at once and then every interval until ctx ends. The first sweep after a
// deploy can remove a lot, so its counts are written to log; later ones write only what they
// removed, and a failed sweep is reported and retried at the next tick.
func (s *Store) RunSweeps(ctx context.Context, every time.Duration, log io.Writer) {
	report := func(first bool) {
		removed, err := s.Sweep(ctx)
		switch {
		case ctx.Err() != nil:
			// The hub is stopping; a sweep it cut short is not a failure to report.
		case err != nil:
			_, _ = fmt.Fprintf(log, "iris: retention sweep failed: %v\n", err)
		case first || removed["events"]+removed["operations"]+removed["claims"]+removed["executions"] > 0:
			_, _ = fmt.Fprintf(log, "iris: retention sweep removed %d events, %d idempotency records, %d finished claims, %d Executions\n", removed["events"], removed["operations"], removed["claims"], removed["executions"])
		}
	}
	report(true)
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			report(false)
		}
	}
}
