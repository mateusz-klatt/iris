// Package wake queues bounded, content-free notifications to its own loaded native session.
package wake

import (
	"context"
	"errors"
	"time"

	"github.com/mateusz-klatt/iris/internal/binding"
	"github.com/mateusz-klatt/iris/internal/hook"
)

// Runner belongs to one MCP child. Its durable guard belongs to the session, so
// neither replacing a binding nor restarting this process grants another wake.
// Construct and run it only when the Codex integration's effective wake preference is enabled.
type Runner struct {
	resolver      targetResolver
	source        wakeSource
	sessions      *hook.Sessions
	commands      nativeCommands
	clientVersion func() string
	observe       func(string)
	now           func() time.Time
	targetPoll    time.Duration
	settle        time.Duration
	filePoll      time.Duration
	reconnectBase time.Duration
	reconnectMax  time.Duration
}

// New resolves the executable but opens no stream or state file and starts no
// process. Run waits for the resolver's positive root binding before doing work.
func New(resolver *binding.Resolver, sessions *hook.Sessions, clientVersion func() string, observe func(string)) *Runner {
	return newRunner(resolver, sessions, clientVersion, observe, newCommands())
}

// NewKimi uses the local Kimi web server belonging to the MCP process's inherited home.
func NewKimi(resolver *binding.Resolver, sessions *hook.Sessions, clientVersion func() string, observe func(string)) *Runner {
	return newRunner(resolver, sessions, clientVersion, observe, newKimiCommands())
}

func newRunner(resolver *binding.Resolver, sessions *hook.Sessions, clientVersion func() string, observe func(string), commands nativeCommands) *Runner {
	return &Runner{
		resolver: resolver, source: hubSource{client: resolver.Hub}, sessions: sessions,
		commands: commands, clientVersion: clientVersion, observe: observe,
		now: time.Now, targetPoll: time.Second, settle: 10 * time.Second,
		filePoll: 15 * time.Second, reconnectBase: time.Second, reconnectMax: 30 * time.Second,
	}
}

func (r *Runner) recovered() {
	if recover() != nil {
		r.note("wake_worker_panic")
	}
}

func (r *Runner) note(text string) {
	if r.observe != nil {
		r.observe(text)
	}
}

// Run stops its worker and connection before returning. A worker panic is
// isolated from the MCP tools and heartbeat, and is never an automatic retry.
func (r *Runner) Run(ctx context.Context) {
	defer r.recovered()
	poll := time.NewTicker(r.targetPoll)
	defer poll.Stop()
	var current binding.WakeTarget
	var stop context.CancelFunc
	var done chan struct{}
	defer func() {
		if stop != nil {
			stop()
			<-done
		}
	}()
	for {
		target, eligible := r.resolver.Primary()
		if _, valid := canonicalTargetSession(target); !valid {
			eligible = false
		}
		if !eligible || target != current {
			if stop != nil {
				stop()
				<-done
				stop, done = nil, nil
			}
			current = binding.WakeTarget{}
			if eligible {
				r.note("wake_root_eligible")
				current = target
				workerCtx, cancel := context.WithCancel(ctx)
				stop, done = cancel, make(chan struct{})
				go func() { defer close(done); r.runTarget(workerCtx, target) }()
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-poll.C:
		}
	}
}

// schedule keeps the earliest candidate while retaining a failed admission's
// required retry time. A burst of later frames cannot postpone an earlier one.
type schedule struct {
	due      time.Time
	required time.Time
	fileWait bool
}

func (s *schedule) hint(now time.Time, settle time.Duration) {
	if s.fileWait {
		return
	}
	due := now.Add(settle)
	if due.Before(s.required) {
		due = s.required
	}
	if s.due.IsZero() || due.Before(s.due) {
		s.due = due
	}
}

func (s *schedule) result(result stepResult) {
	s.due, s.required, s.fileWait = result.Due, result.NotBefore, result.FileWait
}

func (r *Runner) runTarget(ctx context.Context, target binding.WakeTarget) {
	defer r.recovered()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	store := newClientStateStore(r.sessions, target.Client, target.SessionID)
	e := engine{target: target, resolver: r.resolver, source: r.source, store: store,
		commands: r.commands, clientVersion: r.clientVersion, now: r.now, observe: r.observe}
	hints := make(chan struct{}, 1)
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		defer cancel()
		defer r.recovered()
		r.followStream(ctx, target, hints)
	}()
	defer func() { cancel(); <-readerDone }()
	var pending schedule
	for {
		var ready <-chan time.Time
		var timer *time.Timer
		if !pending.due.IsZero() {
			timer = time.NewTimer(max(time.Duration(0), pending.due.Sub(r.now())))
			ready = timer.C
		}
		select {
		case <-ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			return
		case <-hints:
			if timer != nil {
				timer.Stop()
			}
			pending.hint(r.now(), r.settle)
		case <-ready:
			if pending.fileWait {
				pending = r.pollFile(store)
			} else {
				pending.result(e.step(ctx))
			}
		}
	}
}

// While a retained reservation blocks, this is a file-only check. A released
// guard first settles before another authoritative read; it never queues here.
func (r *Runner) pollFile(store *stateStore) schedule {
	snapshot, err := store.snapshot()
	if err == nil && snapshot.Document.outstanding() == nil {
		return schedule{due: r.now().Add(r.settle)}
	}
	delay := r.filePoll
	if err != nil && !errors.Is(err, errStateUnusable) {
		delay = 60 * time.Second
		if errors.Is(err, hook.ErrSessionsLocked) {
			delay = 10 * time.Second
		}
	}
	return schedule{due: r.now().Add(delay), fileWait: true}
}

func signal(hints chan<- struct{}) {
	select {
	case hints <- struct{}{}:
	default:
	}
}

func pause(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// The reader drains frames independently of command execution. One retained
// hint suffices: the subsequent events read asks for current attention, not the
// contents of the frame. All frame kinds still advance the reconnect cursor.
func (r *Runner) followStream(ctx context.Context, target binding.WakeTarget, hints chan<- struct{}) {
	var cursor int64
	var stream string
	backoff := r.reconnectBase
	for ctx.Err() == nil {
		conn, err := r.source.Connect(ctx, target, cursor, stream)
		if err == nil {
			r.note("wake_stream_following")
			signal(hints)
			if r.readStream(ctx, conn, hints, &cursor, &stream) {
				backoff = r.reconnectBase
			}
		}
		if !pause(ctx, backoff) {
			return
		}
		backoff = min(2*backoff, r.reconnectMax)
	}
}

func (r *Runner) readStream(ctx context.Context, conn eventConnection, hints chan<- struct{}, cursor *int64, stream *string) bool {
	defer conn.Close()
	received := false
	for {
		frame, err := conn.Read(ctx)
		if err != nil {
			return received
		}
		received = true
		if frame.Stream != "" {
			if *stream != frame.Stream || frame.Kind == "reset" {
				*stream, *cursor = frame.Stream, frame.Cursor
			} else if frame.Cursor > *cursor {
				*cursor = frame.Cursor
			}
		}
		if frame.Kind == "reset" || frame.Kind == "inbox_changed" || frame.Kind == "claim_available" {
			signal(hints)
		}
	}
}
