package wake

import (
	"context"
	"errors"
	"time"

	"github.com/mateusz-klatt/iris/internal/hook"
	"github.com/mateusz-klatt/iris/internal/hubclient"
)

// step admits at most one reservation. Every hub read is outside the shared file
// lock, and both snapshots are compared again by publish before installation.
func (e *engine) step(ctx context.Context) stepResult {
	session, valid := canonicalSession(e.target.SessionID)
	if !valid || !e.current(ctx) {
		return stepResult{}
	}
	before, err := e.store.snapshot()
	if err != nil {
		return e.fileFailure(err)
	}
	if before.Document.outstanding() != nil {
		return e.waitFile()
	}
	if !e.hookSeen {
		call, cancel := context.WithTimeout(ctx, 5*time.Second)
		seen, err := e.source.HookSeen(call, e.target)
		cancel()
		if err != nil || !seen {
			e.note("wake_failed")
			return e.after(30 * time.Second)
		}
		e.hookSeen = true
	}
	call, cancel := context.WithTimeout(ctx, 5*time.Second)
	read, err := e.source.Attention(call, e.target, e.cursor, e.stream)
	cancel()
	if err != nil {
		e.note("wake_failed")
		return e.after(30 * time.Second)
	}
	if !e.current(ctx) {
		return stepResult{}
	}
	current, err := e.store.snapshot()
	if err != nil {
		return e.fileFailure(err)
	}
	if !sameState(before, current) {
		return e.after(10 * time.Second)
	}
	id := read.Attention.Identity
	if !id.Known || read.Stream == "" || read.HighWater < 0 ||
		(id.Pending && (id.Stream != read.Stream || id.Cursor <= 0 || id.Cursor > read.HighWater)) {
		e.note("wake_invalid_state")
		return e.after(30 * time.Second)
	}
	e.stream, e.cursor = read.Stream, read.HighWater
	if !id.Pending {
		return stepResult{}
	}
	next, slot, due, eligible := admission(current, id, read.HighWater, e.now())
	if !eligible {
		return stepResult{Due: due}
	}
	info, detail := e.commands.check(ctx, e.clientVersion())
	if !e.current(ctx) {
		return stepResult{}
	}
	at := e.now().UnixMilli()
	a := attempt{ID: attemptID(), Kind: "binary_check", Stream: id.Stream, Cursor: id.Cursor, Slot: slot, StartedMS: at}
	next.Binary, next.LastFailure = info, nil
	if detail != "" {
		a.Answer, a.AnsweredMS, a.Detail = "failed", at, detail
		next.LastFailure = &failure{AtMS: at, Detail: detail}
	} else {
		a.Kind, a.Token = "queue", attemptID()
		a.Message = promptFor(a.Token)
	}
	next = next.appendAttempt(a)
	published, err := e.store.publish(current, next)
	if err != nil {
		if published.Published {
			if mergeErr := e.merge(published.ID, "aborted", "Wake reservation was not ready for command start"); mergeErr != nil && a.Kind == "queue" {
				e.note("wake_unknown")
				return e.waitFile()
			}
		}
		return e.fileFailure(err)
	}
	if !e.current(ctx) {
		if err := e.merge(a.ID, "aborted", "Root binding changed before command start"); err != nil {
			e.note("wake_unknown")
			if a.Kind == "queue" {
				return e.waitFile()
			}
			return e.after(60 * time.Second)
		}
		return stepResult{}
	}
	if a.Kind == "binary_check" {
		e.note("wake_failed")
		return e.failedRetry(next, slot)
	}
	answer := e.commands.queue(ctx, session, a.Message, func(start func() error) (bool, error) {
		return e.resolver.StartIfPrimary(e.target, start)
	})
	if err := e.merge(a.ID, answer.answer, answer.detail); err != nil {
		e.note("wake_unknown")
		return e.waitFile()
	}
	switch answer.answer {
	case "aborted":
		return stepResult{}
	case "failed":
		e.note("wake_failed")
		return e.failedRetry(next, slot)
	case "unknown":
		e.note("wake_unknown")
	}
	return e.waitFile()
}

// admission never resets a retained outstanding attempt; step checked that
// before asking the hub. An older cursor is not a new identity unless the same
// full file snapshots and the hub's high-water mark prove a restore.
func admission(snapshot stateSnapshot, id hubclient.Identity, highWater int64, now time.Time) (document, int, time.Time, bool) {
	next, told := snapshot.Document, snapshot.Told
	mark := next.mark()
	// Only this runner's mark can prove its quota predates a restore. A hook's
	// older told cursor can remain above highWater after a wake was consumed;
	// using it to reset this ledger again would reopen already spent quota.
	if mark.Pending {
		switch {
		case mark.Stream != id.Stream || id.Cursor > mark.Cursor || mark.Cursor > highWater:
			next.Attempts, next.LastFailure = nil, nil
		case id.Cursor < mark.Cursor:
			return next, 0, time.Time{}, false
		}
	}
	slot := 0
	var due time.Time
	if told.Identity.Pending && told.Identity.Stream == id.Stream && told.Identity.Cursor <= highWater && id.Cursor <= told.Identity.Cursor {
		if id.Cursor < told.Identity.Cursor || told.Twice || told.ToldMS <= 0 {
			return next, 0, time.Time{}, false
		}
		slot = 1
		due = time.UnixMilli(told.ToldMS).Add(hook.SecondNoteAfter)
	}
	if next.quota(slot) >= 2 {
		return next, slot, time.Time{}, false
	}
	for _, old := range next.Attempts {
		if old.Slot == slot && old.spent() {
			if retry := time.UnixMilli(old.AnsweredMS).Add(60 * time.Second); retry.After(due) {
				due = retry
			}
		}
	}
	return next, slot, due, !due.After(now)
}

func (e *engine) current(ctx context.Context) bool {
	if ctx.Err() != nil {
		return false
	}
	target, ok := e.resolver.Primary()
	return ok && target == e.target
}

func (e *engine) after(delay time.Duration) stepResult { return stepResult{Due: e.now().Add(delay)} }

func (e *engine) waitFile() stepResult {
	result := e.after(15 * time.Second)
	result.FileWait = true
	return result
}

func (e *engine) fileFailure(err error) stepResult {
	if errors.Is(err, errStateUnusable) {
		e.note("wake_invalid_state")
		return e.waitFile()
	}
	e.note("wake_failed")
	if errors.Is(err, hook.ErrSessionsLocked) || errors.Is(err, errStateChanged) {
		return e.after(10 * time.Second)
	}
	return e.after(60 * time.Second)
}

// A persisted binary failure already counts in next. A queue failure replaces
// its still-unanswered reservation, which next also counts; two tries exhaust it.
func (e *engine) failedRetry(next document, slot int) stepResult {
	if next.tries(slot) >= 2 {
		return stepResult{}
	}
	return e.after(60 * time.Second)
}

// Outcome persistence outlives cancellation of the command's context. Each retry
// rereads by ID, preserving a hook's consumption and any newer reservation.
func (e *engine) merge(id, answer, detail string) error {
	var err error
	for try := 0; try < 3; try++ {
		err = e.store.answer(id, answer, e.now().UnixMilli(), detail)
		if err == nil {
			return nil
		}
		if try < 2 {
			time.Sleep(100 * time.Millisecond)
		}
	}
	return err
}

func (e *engine) note(reason string) {
	if e.observe != nil {
		e.observe(reason)
	}
}
