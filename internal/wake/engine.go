package wake

import (
	"context"
	"errors"
	"time"

	"github.com/mateusz-klatt/iris/internal/hook"
	"github.com/mateusz-klatt/iris/internal/hubclient"
)

// Fixed observations of a wake step (ADR 017). Each names a condition and never carries
// error, command or client text: doctor counts observations by their exact text, so a
// free-form part would split one cause into many rows.
const (
	observedHooksUnseen   = "wake_hooks_unseen"
	deferredSessionActive = "wake_deferred: session active"
	abortedSessionActive  = "wake_aborted: session active"
	failedHookCheck       = "wake_failed: hook check"
	failedAttentionRead   = "wake_failed: attention read"
	failedVersionDiffers  = "wake_failed: version differs"
	failedBinaryCheck     = "wake_failed: binary check"
	failedIdleCheck       = "wake_failed: idle check"
	failedQueue           = "wake_failed: queue failed"
	failedStateFile       = "wake_failed: state file"
)

// step admits at most one reservation. Every hub read is outside the shared file
// lock, and both snapshots are compared again by publish before installation.
func (e *engine) step(ctx context.Context) stepResult {
	// Only a step that ends in the same lasting condition sets this again; any other outcome
	// clears it.
	last := e.condition
	e.condition = ""
	session, valid := canonicalTargetSession(e.target)
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
		if err != nil {
			e.note(failedHookCheck)
			return e.after(30 * time.Second)
		}
		if !seen {
			// No command ran and none failed: the hub has no hook report from this
			// execution and generation yet, so the session is not eligible for a wake.
			e.note(observedHooksUnseen)
			return e.after(30 * time.Second)
		}
		e.hookSeen = true
	}
	next, current, id, result, eligible := e.candidate(ctx, before)
	if !eligible {
		return result
	}
	info, detail, differs := e.commands.check(ctx, e.clientVersion())
	if !e.current(ctx) {
		return stepResult{}
	}
	if detail != "" {
		// Without a validated binary the owning daemon's idle state is unknown.
		// Retry the check without reserving a wake or spending its allowance.
		reason := failedBinaryCheck
		if differs {
			reason = failedVersionDiffers
		}
		return e.preflightFailure(current, info, detail, reason)
	}
	idleCtx, idleCancel := context.WithTimeout(ctx, 5*time.Second)
	idle, detail := e.commands.idle(idleCtx, session)
	idleCancel()
	if !e.current(ctx) {
		return stepResult{}
	}
	if !idle {
		if detail == codexActive || detail == antigravityActive {
			return e.recheck(last, deferredSessionActive, 15*time.Second)
		}
		return e.preflightFailure(current, info, detail, failedIdleCheck)
	}
	// A busy turn may have fetched its mail or emitted a hook note while the
	// daemon check ran. Only a new authoritative read after verified idle may
	// decide what is still untold, against a fresh complete file snapshot.
	before, err = e.store.snapshot()
	if err != nil {
		return e.fileFailure(err)
	}
	if before.Document.outstanding() != nil {
		return e.waitFile()
	}
	next, current, id, result, eligible = e.candidate(ctx, before)
	if !eligible {
		return result
	}
	at := e.now().UnixMilli()
	a := attempt{ID: attemptID(), Kind: "queue", Stream: id.Stream, Cursor: id.Cursor, Slot: 0, StartedMS: at, Token: attemptID()}
	a.Message = promptFor(a.Token)
	if init, ok := e.commands.(scanInitializer); ok {
		a.Scan = init.initialScan(session)
	}
	next.Binary, next.LastFailure = info, nil
	next = next.appendAttempt(a)
	published, err := e.store.publish(current, next)
	if err != nil {
		if published.Published {
			if mergeErr := e.merge(published.ID, "aborted", "Wake reservation was not ready for command start"); mergeErr != nil {
				e.note("wake_unknown")
				return e.waitFile()
			}
		}
		return e.fileFailure(err)
	}
	if !e.current(ctx) {
		if err := e.merge(a.ID, "aborted", "Root binding changed before command start"); err != nil {
			e.note("wake_unknown")
			return e.waitFile()
		}
		return stepResult{}
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
		switch answer.detail {
		case antigravityActive:
			return e.recheck(last, abortedSessionActive, 15*time.Second)
		case antigravityExeUnavailable:
			return e.after(60 * time.Second)
		}
		return stepResult{}
	case "failed":
		e.note(failedQueue)
		return e.failedRetry(next)
	case "unknown":
		e.note("wake_unknown")
	}
	return e.waitFile()
}

// recheck tries a lasting condition again after delay. A session can stay active for many
// checks, so the observation is noted only when the previous step did not end in the same one:
// one line each time the outcome changes, not one per check.
func (e *engine) recheck(last, observation string, delay time.Duration) stepResult {
	if observation != last {
		e.note(observation)
	}
	e.condition = observation
	return e.after(delay)
}

// A failed preflight can explain why a wake is deferred without creating a
// reservation or spending quota. Its publication error must never abort an old
// attempt: no new attempt exists, even when diagnostic read-back was uncertain.
func (e *engine) preflightFailure(current stateSnapshot, info binaryInfo, detail, reason string) stepResult {
	// Only publishing a replacement reservation may retire an old identity's
	// mark. An operational failure must retain it if attention later regresses.
	next := current.Document
	next.Binary, next.LastFailure = info, &failure{AtMS: e.now().UnixMilli(), Detail: detail}
	if _, err := e.store.publish(current, next); err != nil {
		return e.fileFailure(err)
	}
	e.note(reason)
	return e.after(60 * time.Second)
}

// candidate binds one authoritative attention read to the complete file bytes
// sampled before it. It never advances quota or accepts an outstanding guard.
func (e *engine) candidate(ctx context.Context, before stateSnapshot) (document, stateSnapshot, hubclient.Identity, stepResult, bool) {
	call, cancel := context.WithTimeout(ctx, 5*time.Second)
	read, err := e.source.Attention(call, e.target, e.cursor, e.stream)
	cancel()
	if err != nil {
		e.note(failedAttentionRead)
		return document{}, stateSnapshot{}, hubclient.Identity{}, e.after(30 * time.Second), false
	}
	if !e.current(ctx) {
		return document{}, stateSnapshot{}, hubclient.Identity{}, stepResult{}, false
	}
	current, err := e.store.snapshot()
	if err != nil {
		return document{}, stateSnapshot{}, hubclient.Identity{}, e.fileFailure(err), false
	}
	if !sameState(before, current) {
		return document{}, stateSnapshot{}, hubclient.Identity{}, e.after(10 * time.Second), false
	}
	id := read.Attention.Identity
	if !id.Known || read.Stream == "" || read.HighWater < 0 ||
		(id.Pending && (id.Stream != read.Stream || id.Cursor <= 0 || id.Cursor > read.HighWater)) {
		e.note("wake_invalid_state")
		return document{}, stateSnapshot{}, hubclient.Identity{}, e.after(30 * time.Second), false
	}
	e.stream, e.cursor = read.Stream, read.HighWater
	if !id.Pending {
		return document{}, current, id, stepResult{}, false
	}
	next, due, eligible := admission(current, id, read.HighWater, e.now())
	result := stepResult{Due: due, NotBefore: due}
	return next, current, id, result, eligible
}

// admission never resets a retained outstanding attempt; step checked that
// before asking the hub. An older cursor is not a new identity unless the same
// full file snapshots and the hub's high-water mark prove a restore.
func admission(snapshot stateSnapshot, id hubclient.Identity, highWater int64, now time.Time) (document, time.Time, bool) {
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
			return next, time.Time{}, false
		}
	}
	if told.Identity.Pending && told.Identity.Stream == id.Stream && told.Identity.Cursor <= highWater && id.Cursor <= told.Identity.Cursor {
		return next, time.Time{}, false
	}
	if nativeQuota(next) >= 2 {
		return next, time.Time{}, false
	}
	due := retryAt(next)
	return next, due, !due.After(now)
}

// Legacy version-one documents may contain slot 1. Their consumed/submitted or
// uncertain attempts still spend this identity, and their known failures share
// its one retry. New attempts always use slot 0; no timer creates a second wake.
func nativeQuota(d document) int {
	if d.quota(0) >= 2 || d.quota(1) >= 2 {
		return 2
	}
	return min(2, d.tries(0)+d.tries(1))
}

func retryAt(d document) time.Time {
	// Admission calls this only when quota permits another try, so there can
	// be at most one retained known failure across the legacy slots.
	for _, a := range d.Attempts {
		if a.Answer == "failed" {
			return time.UnixMilli(a.AnsweredMS).Add(60 * time.Second)
		}
	}
	return time.Time{}
}

func (e *engine) current(ctx context.Context) bool {
	if ctx.Err() != nil {
		return false
	}
	target, ok := e.resolver.Primary()
	return ok && target == e.target
}

func (e *engine) after(delay time.Duration) stepResult {
	due := e.now().Add(delay)
	return stepResult{Due: due, NotBefore: due}
}

func (e *engine) waitFile() stepResult {
	delay := e.filePoll
	if delay <= 0 {
		delay = 15 * time.Second
	}
	return stepResult{Due: e.now().Add(delay), FileWait: true}
}

func (e *engine) fileFailure(err error) stepResult {
	if errors.Is(err, errStateUnusable) {
		e.note("wake_invalid_state")
		return e.waitFile()
	}
	e.note(failedStateFile)
	if errors.Is(err, hook.ErrSessionsLocked) || errors.Is(err, errStateChanged) {
		return e.after(10 * time.Second)
	}
	return e.after(60 * time.Second)
}

// A known queue failure grants one retry, shared with any legacy known failures.
func (e *engine) failedRetry(next document) stepResult {
	if next.tries(0)+next.tries(1) >= 2 {
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
