package wake

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/mateusz-klatt/iris/internal/binding"
	"github.com/mateusz-klatt/iris/internal/hook"
	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/kimiwake"
	"github.com/mateusz-klatt/iris/internal/proc"
)

// Outcome is how Kimi's one-shot watcher ended (ADR 030). Its value is the process exit status, which
// is what Kimi reports in the task's notification.
type Outcome int

// The watcher's outcomes. Only Attention asks the model to start a replacement.
const (
	Attention    Outcome = 0
	Disabled     Outcome = 10
	Duplicate    Outcome = 11
	Failed       Outcome = 12
	Expired      Outcome = 13
	Lost         Outcome = 14
	NotCompleted Outcome = 15
)

const (
	onceLifetime   = 24 * time.Hour
	onceBeat       = 20 * time.Second
	onceWindow     = 30 * time.Second
	onceCall       = 5 * time.Second
	onceLockBudget = 2 * time.Second
	onceMaxBackoff = 30 * time.Second
	// onceUnbound is how long the lookup may answer unbound before the watcher gives up: past the
	// hub's recovery window, which a session's next hook ends by reattaching it.
	onceUnbound = 150 * time.Second
)

// attentionLine is the fixed line printed before the watcher exits 0. It carries no message,
// sender or anything else the hub sent.
const attentionLine = "Iris: new attention. Use the Iris inbox and claims tools, then start one replacement watcher as Iris asked."

// outcomeLines say why a watcher ended without attention. None asks for a replacement.
var outcomeLines = map[Outcome]string{
	Disabled:     "Iris: wake is off for this session, or its process or session changed. Do not start another watcher.",
	Duplicate:    "Iris: another watcher already waits for this session. Do not start another.",
	Failed:       "Iris: this watcher could not start. Do not start another; `iris doctor` can tell why.",
	Expired:      "Iris: this watcher reached its 24-hour limit. Do not start another.",
	Lost:         "Iris: the hub no longer has this session. Do not start another watcher.",
	NotCompleted: "Iris: new attention may be waiting, but Iris could not record it. Use the Iris inbox and claims tools. Do not start another watcher.",
}

// latchFor is the latch an outcome leaves on the record, so the prompt hook does not ask again for
// a watcher that cannot work.
var latchFor = map[Outcome]string{Disabled: kimiwake.LatchDisabled, Failed: kimiwake.LatchFailed, Expired: kimiwake.LatchExpired}

// onceHub is the hub the watcher reads, an interface so tests replace it.
type onceHub interface {
	kimiBind(ctx context.Context, host, cwd, session string, pid int, startMS int64) (binding.Bound, error)
	Attention(ctx context.Context, target binding.WakeTarget, after int64, stream string) (attentionRead, error)
	Connect(ctx context.Context, target binding.WakeTarget, after int64, stream string) (eventConnection, error)
	heartbeat(ctx context.Context, target binding.WakeTarget) error
	checkpoint(ctx context.Context, target binding.WakeTarget, cursor int64) error
}

// kimiOnce is one run of `iris watch --client kimi --once`.
type kimiOnce struct {
	hub     onceHub
	store   *kimiwake.Store
	told    func(session string) (hook.ToldSnapshot, error)
	nearest func() (proc.Ancestor, error)
	alive   func(pid int, startMS int64) bool
	self    kimiwake.Attempt
	host    string
	cwd     string
	out     io.Writer
	now     func() time.Time
}

// RunKimiOnce runs Kimi's one-shot watcher until it ends (ADR 030), and returns how. The hub
// configuration and the wake state come from the OS account's pinned Iris directory, because a
// model starts this command and its environment cannot be trusted to name a hub or a token.
func RunKimiOnce(ctx context.Context, out io.Writer) Outcome {
	w, ok := newKimiOnce(out, loadKimiOnce, proc.Nearest, proc.Lookup, os.Getpid)
	if !ok {
		_, _ = fmt.Fprintln(out, outcomeLines[Failed])
		return Failed
	}
	return w.run(ctx)
}

// loadKimiOnce reads the hub configuration from the OS account's pinned directory. Tests replace it.
var loadKimiOnce = hubclient.PinnedConfig{}.Load

// newKimiOnce wires a watcher from the pinned configuration and this process. ok is false when the
// configuration or this process's own identity is unknown. A configuration that loads always names
// its token file, beside which the hooks keep what they told each session.
func newKimiOnce(out io.Writer, load func() (hubclient.Resolved, string, error), nearest func() (proc.Ancestor, error), lookup func(int) (proc.Snapshot, bool), getpid func() int) (*kimiOnce, bool) {
	resolved, dir, err := load()
	if err != nil || hubclient.ValidateURL(resolved.URL) != nil {
		return nil, false
	}
	token := resolved.TokenFile
	self, ok := lookup(getpid())
	if !ok || self.StartMS <= 0 {
		return nil, false
	}
	host, _ := os.Hostname()
	cwd, _ := os.Getwd()
	sessions := hook.NewSessions(token)
	return &kimiOnce{
		hub:   hubSource{client: &hubclient.Client{Config: resolved}},
		store: &kimiwake.Store{Root: kimiwake.Root(dir)},
		told: func(session string) (hook.ToldSnapshot, error) {
			unlock, err := sessions.Lock(onceLockBudget)
			if err != nil {
				return hook.ToldSnapshot{}, err
			}
			defer unlock()
			return sessions.Told("kimi", session)
		},
		nearest: nearest,
		alive: func(pid int, startMS int64) bool {
			p, ok := lookup(pid)
			return ok && startMS > 0 && p.StartMS == startMS
		},
		self: kimiwake.Attempt{PID: self.PID, StartMS: self.StartMS, ID: attemptID()},
		host: host, cwd: cwd, out: out, now: time.Now,
	}, true
}

func (w *kimiOnce) run(parent context.Context) Outcome {
	ctx, cancel := context.WithTimeout(parent, onceLifetime)
	defer cancel()
	anc, err := w.nearest()
	if err != nil || anc.PID <= 0 || anc.StartMS <= 0 || !proc.SingleSession(anc, "kimi") {
		return w.say(Failed)
	}
	session, ok, err := w.store.Session(anc.PID, anc.StartMS, onceLockBudget)
	if err != nil || !ok {
		return w.say(Failed)
	}
	key := kimiwake.Key{PID: anc.PID, StartMS: anc.StartMS, Session: session}
	if !kimiwake.Enabled(w.store.Root) {
		_ = w.store.LatchIfFree(key, kimiwake.LatchDisabled, onceLockBudget)
		return w.say(Disabled)
	}
	owned, err := w.store.Own(key, w.self, onceLockBudget)
	if err != nil {
		return w.say(Failed)
	}
	if !owned {
		return w.say(Duplicate)
	}
	o := &onceRun{w: w, key: key, held: map[string]int64{}}
	outcome := o.watch(ctx, parent)
	o.release(outcome)
	return w.say(outcome)
}

// say prints why a watcher ended without attention. A watcher with attention printed its line
// before checkpointing it.
func (w *kimiOnce) say(o Outcome) Outcome {
	if line, ok := outcomeLines[o]; ok {
		_, _ = fmt.Fprintln(w.out, line)
	}
	return o
}

// onceRun is a watcher that owns its record.
type onceRun struct {
	w       *kimiOnce
	key     kimiwake.Key
	target  binding.WakeTarget
	bound   bool
	fresh   bool // the record had no baseline before this run
	stream  string
	cursor  int64 // the newest event processed
	saved   int64 // the cursor last checkpointed, in savedIn
	savedIn string
	emitted map[string]kimiwake.Identity
	delays  kimiwake.Delays
	held    map[string]int64 // attention not yet announced, by kind, with its newest cursor
	until   time.Time        // when a held candidate is decided again
	delay   time.Duration
}

func (o *onceRun) watch(ctx, parent context.Context) Outcome {
	tick := time.NewTicker(onceBeat)
	defer tick.Stop()
	if out, done := o.bind(ctx, parent, tick); done {
		return out
	}
	if out, done := o.baseline(ctx, parent, tick); done {
		return out
	}
	return o.follow(ctx, parent, tick)
}

// ended is the outcome when the context ends: the lifetime, or a stop from outside, as removal or
// a turned-off wake sends.
func (o *onceRun) ended(parent context.Context) Outcome {
	if parent.Err() != nil {
		return Disabled
	}
	return Expired
}

// release gives up the watcher's claim and leaves the outcome's latch, but only while this watcher
// still holds the record. A completed watcher has already given it up.
func (o *onceRun) release(out Outcome) {
	latch := latchFor[out]
	_, _ = o.w.store.UpdateOwned(o.key, o.w.self.ID, onceLockBudget, func(r *kimiwake.Record) {
		r.Attempt = nil
		if latch != "" {
			r.Latch, r.ArmRequestedMS = &kimiwake.Latch{State: latch, AtMS: o.w.now().UnixMilli()}, 0
		}
	})
}

// checks are the local conditions of every phase: the Kimi process still runs, wake is still on,
// and the process still runs this session. A map that cannot be read now decides nothing.
func (o *onceRun) checks() (Outcome, bool) {
	if !o.w.alive(o.key.PID, o.key.StartMS) || !kimiwake.Enabled(o.w.store.Root) {
		return Disabled, true
	}
	if session, ok, err := o.w.store.Session(o.key.PID, o.key.StartMS, onceLockBudget); err == nil && (!ok || session != o.key.Session) {
		return Disabled, true
	}
	return 0, false
}

// beat is the 20-second tick of every phase: the pinned generation's heartbeat once bound, then the
// local checks. Only a heartbeat answering lost ends the watcher for its generation; event reads
// refused during a hub's recovery do not.
func (o *onceRun) beat(ctx context.Context) (Outcome, bool) {
	if o.bound {
		call, cancel := context.WithTimeout(ctx, onceCall)
		err := o.w.hub.heartbeat(call, o.target)
		cancel()
		if hubclient.IsCode(err, "lost") {
			return Lost, true
		}
	}
	return o.checks()
}

// pause waits out the backoff, which doubles up to its cap, while the tick goes on.
func (o *onceRun) pause(ctx, parent context.Context, tick *time.Ticker) (Outcome, bool) {
	wait := time.NewTimer(o.delay)
	defer wait.Stop()
	o.delay = min(o.delay*2, onceMaxBackoff)
	for {
		select {
		case <-ctx.Done():
			return o.ended(parent), true
		case <-wait.C:
			return 0, false
		case <-tick.C:
			if out, stop := o.beat(ctx); stop {
				return out, true
			}
		}
	}
}

// bind looks up the hook-registered Execution of this process and session, and never registers
// one. A refusal that a retry cannot change fails the watcher. An Execution not found may be
// recovering after a hub restart, so unbound is retried for the hub's recovery window.
func (o *onceRun) bind(ctx, parent context.Context, tick *time.Ticker) (Outcome, bool) {
	o.delay = time.Second
	start := o.w.now()
	for {
		if out, stop := o.checks(); stop {
			return out, true
		}
		call, cancel := context.WithTimeout(ctx, onceCall)
		b, err := o.w.hub.kimiBind(call, o.w.host, o.w.cwd, o.key.Session, o.key.PID, o.key.StartMS)
		cancel()
		if err == nil {
			o.target = binding.WakeTarget{SessionID: o.key.Session, Bound: b, Host: o.w.host, Client: "kimi"}
			o.bound = true
			return 0, false
		}
		if errors.Is(err, errUnmatched) || hubclient.IsCode(err, "ambiguous") || hubclient.IsCode(err, "bad_request") ||
			hubclient.IsCode(err, "unauthorized") || (hubclient.IsCode(err, "unbound") && o.w.now().Sub(start) >= onceUnbound) {
			return Failed, true
		}
		if out, stop := o.pause(ctx, parent, tick); stop {
			return out, true
		}
	}
}

// read is one authoritative attention read, bounded like every hub call.
func (o *onceRun) read(ctx context.Context) (attentionRead, error) {
	call, cancel := context.WithTimeout(ctx, onceCall)
	defer cancel()
	return o.w.hub.Attention(call, o.target, o.cursor, o.stream)
}

// baseline takes the record's cursor, or sets one. A watcher with no prior record takes the hub's
// current stream and high-water mark, and announces nothing for history: unread mail and freed
// paths that predate it are what SessionStart, the prompt hooks and the inbox already cover.
func (o *onceRun) baseline(ctx, parent context.Context, tick *time.Ticker) (Outcome, bool) {
	r, _, err := o.w.store.Read(o.key, onceLockBudget)
	if err != nil {
		return Failed, true
	}
	o.emitted, o.delays = r.Emitted, r.Coalesce
	if r.Checkpoint.Initialized {
		o.stream, o.cursor = r.Checkpoint.Stream, r.Checkpoint.Cursor
		o.saved, o.savedIn = o.cursor, o.stream
		return 0, false
	}
	o.fresh, o.delay = true, time.Second
	for {
		read, err := o.read(ctx)
		if err == nil && read.Stream != "" && read.HighWater >= 0 {
			o.stream, o.cursor = read.Stream, read.HighWater
			if !o.save() {
				return Failed, true
			}
			return 0, false
		}
		if out, stop := o.pause(ctx, parent, tick); stop {
			return out, true
		}
	}
}

// save checkpoints the cursor in the record, while this watcher holds it.
func (o *onceRun) save() bool {
	owned, err := o.w.store.UpdateOwned(o.key, o.w.self.ID, onceLockBudget, func(r *kimiwake.Record) {
		r.Checkpoint = kimiwake.Checkpoint{Stream: o.stream, Cursor: o.cursor, Initialized: true}
	})
	if err != nil || !owned {
		return false
	}
	o.saved, o.savedIn = o.cursor, o.stream
	return true
}

// follow keeps a stream open from the cursor, and opens it again after it fails.
func (o *onceRun) follow(ctx, parent context.Context, tick *time.Ticker) Outcome {
	o.delay = time.Second
	for {
		call, cancel := context.WithTimeout(ctx, onceCall)
		conn, err := o.w.hub.Connect(call, o.target, o.cursor, o.stream)
		cancel()
		if err == nil {
			out, done := o.serve(ctx, parent, tick, conn)
			conn.Close()
			if done {
				return out
			}
			o.delay = time.Second
		}
		if out, stop := o.pause(ctx, parent, tick); stop {
			return out
		}
	}
}

// serve handles one open stream: its frames, the tick and the time a held candidate is due. It
// returns done=false when the stream fails, to be opened again.
func (o *onceRun) serve(ctx, parent context.Context, tick *time.Ticker, conn eventConnection) (Outcome, bool) {
	reading, stop := context.WithCancel(ctx)
	defer stop()
	frames, failed := make(chan eventFrame), make(chan struct{})
	go func() {
		defer close(failed)
		for {
			f, err := conn.Read(reading)
			if err != nil {
				return
			}
			select {
			case frames <- f:
			case <-reading.Done():
				return
			}
		}
	}()
	for {
		var due <-chan time.Time
		var timer *time.Timer
		if len(o.held) > 0 {
			timer = time.NewTimer(max(0, o.until.Sub(o.w.now())))
			due = timer.C
		}
		var out Outcome
		var done bool
		select {
		case <-ctx.Done():
			out, done = o.ended(parent), true
		case <-failed:
			out, done = 0, false
			stop()
		case <-tick.C:
			out, done = o.beat(ctx)
		case <-due:
			out, done = o.decide(ctx)
		case f := <-frames:
			out, done = o.frame(ctx, f)
		}
		if timer != nil {
			timer.Stop()
		}
		if done || reading.Err() != nil {
			return out, done
		}
	}
}

// frame takes one event. A reset, or a frame from another stream, is answered by an authoritative
// read. A frame at or below the cursor was seen already.
func (o *onceRun) frame(ctx context.Context, f eventFrame) (Outcome, bool) {
	if f.Kind == "reset" || f.Stream != o.stream {
		return o.reset(ctx)
	}
	if f.Cursor <= o.cursor {
		return 0, false
	}
	o.cursor = f.Cursor
	if f.Kind == "inbox_changed" || f.Kind == "claim_available" {
		o.held[f.Kind] = f.Cursor
		o.until = o.w.now()
		return o.decide(ctx)
	}
	o.quiet(ctx)
	return 0, false
}

// reset re-reads attention after the hub restarted the log or the stream.
//   - A new stream: what is pending there may be announced once, unless this run took the first
//     baseline, which never announces history.
//   - The same stream gone back, as after a restore: the cursor re-anchors at the high-water mark,
//     and what was emitted above it no longer counts.
//   - The same stream with events dropped: only an identity newer than the cursor qualifies.
func (o *onceRun) reset(ctx context.Context) (Outcome, bool) {
	read, err := o.read(ctx)
	if err != nil {
		return 0, false
	}
	id := read.Attention.Identity
	newer := id.Pending && id.Stream == read.Stream && id.Cursor > o.cursor
	switch {
	case read.Stream != o.stream:
		newer = id.Pending && id.Stream == read.Stream && !o.fresh
	case read.HighWater < o.cursor:
		newer = false
		for kind, e := range o.emitted {
			if e.Stream == read.Stream && e.Cursor > read.HighWater {
				delete(o.emitted, kind)
			}
		}
		for kind, c := range o.held {
			if c > read.HighWater {
				delete(o.held, kind)
			}
		}
	}
	o.stream, o.cursor = read.Stream, read.HighWater
	if newer {
		kind := "claim_available"
		if read.Attention.Unread > 0 {
			kind = "inbox_changed"
		}
		o.held[kind] = id.Cursor
		o.until = o.w.now()
		return o.decide(ctx)
	}
	o.quiet(ctx)
	return 0, false
}

// decide judges what is held, against authoritative attention and what the hooks told the
// session. Mail already fetched or told, and availability already emitted or told, is not new. A
// told identity above the hub's high-water mark comes from before a restore and covers nothing.
// What is new waits out the rest of its kind's coalescing window, and then completes.
func (o *onceRun) decide(ctx context.Context) (Outcome, bool) {
	if out, stop := o.checks(); stop {
		return out, true
	}
	read, err := o.read(ctx)
	if err != nil {
		o.until = o.w.now().Add(onceCall)
		return 0, false
	}
	told, err := o.w.told(o.key.Session)
	if err != nil {
		told = hook.ToldSnapshot{}
	}
	covered := func(stream string, cursor int64) bool {
		t := told.Identity
		return t.Pending && t.Stream == stream && t.Cursor >= cursor && t.Cursor <= read.HighWater
	}
	news := map[string]int64{}
	var due time.Time
	for kind, cursor := range o.held {
		id := read.Attention.Identity
		if kind == "inbox_changed" && (read.Attention.Unread == 0 || (id.Pending && covered(id.Stream, id.Cursor))) {
			continue
		}
		if e, ok := o.emitted[kind]; kind == "claim_available" && ((ok && e.Stream == o.stream && e.Cursor >= cursor) || covered(o.stream, cursor)) {
			continue
		}
		news[kind] = cursor
		if next := time.UnixMilli(o.delays[kind]); o.delays[kind] > 0 && next.After(due) {
			due = next
		}
	}
	if len(news) == 0 {
		o.held = map[string]int64{}
		o.quiet(ctx)
		return 0, false
	}
	if o.w.now().Before(due) {
		o.until = due
		return 0, false
	}
	return o.complete(ctx, news), true
}

// quiet checkpoints what was processed without a line, but never past attention still held: a
// watcher that dies holding it must leave it for the next one to replay. After a reset the
// checkpoint follows the cursor down, or into the new stream. A failed checkpoint is tried again
// later and changes nothing now.
func (o *onceRun) quiet(ctx context.Context) {
	if len(o.held) > 0 || (o.cursor == o.saved && o.stream == o.savedIn) {
		return
	}
	call, cancel := context.WithTimeout(ctx, onceCall)
	err := o.w.hub.checkpoint(call, o.target, o.cursor)
	cancel()
	if err == nil {
		_ = o.save()
	}
}

// complete announces the new attention and ends with exit 0, in an order that never skips it:
// print the line, save the cursor at the hub, then record it locally with what was emitted and
// each kind's next coalescing time, and give up the record. A failure after the print exits non-zero
// with the old checkpoint, so the next watcher replays: a repeated wake, never a lost one.
func (o *onceRun) complete(ctx context.Context, news map[string]int64) Outcome {
	if out, stop := o.checks(); stop {
		return out
	}
	if _, err := fmt.Fprintln(o.w.out, attentionLine); err != nil {
		return NotCompleted
	}
	call, cancel := context.WithTimeout(ctx, onceCall)
	err := o.w.hub.checkpoint(call, o.target, o.cursor)
	cancel()
	if err != nil {
		return NotCompleted
	}
	next := o.w.now().Add(onceWindow).UnixMilli()
	owned, err := o.w.store.UpdateOwned(o.key, o.w.self.ID, onceLockBudget, func(r *kimiwake.Record) {
		r.Checkpoint = kimiwake.Checkpoint{Stream: o.stream, Cursor: o.cursor, Initialized: true}
		if r.Emitted == nil {
			r.Emitted = map[string]kimiwake.Identity{}
		}
		if r.Coalesce == nil {
			r.Coalesce = kimiwake.Delays{}
		}
		for kind, cursor := range news {
			r.Emitted[kind] = kimiwake.Identity{Stream: o.stream, Cursor: cursor}
			r.Coalesce[kind] = next
		}
		r.Attempt = nil
	})
	if err != nil || !owned {
		return NotCompleted
	}
	return Attention
}
