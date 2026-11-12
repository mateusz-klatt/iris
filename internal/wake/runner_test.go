package wake

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/mateusz-klatt/iris/internal/binding"
	"github.com/mateusz-klatt/iris/internal/hook"
	"github.com/mateusz-klatt/iris/internal/hubclient"
)

type runnerResolver struct {
	mu       sync.Mutex
	target   binding.WakeTarget
	eligible bool
	primary  func() (binding.WakeTarget, bool)
}

func (r *runnerResolver) Primary() (binding.WakeTarget, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.primary != nil {
		return r.primary()
	}
	return r.target, r.eligible
}
func (r *runnerResolver) StartIfPrimary(expected binding.WakeTarget, start func() error) (bool, error) {
	target, ok := r.Primary()
	if !ok || target != expected {
		return false, nil
	}
	err := start()
	return err == nil, err
}
func (r *runnerResolver) set(target binding.WakeTarget, eligible bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.target, r.eligible = target, eligible
}

type runnerSource struct {
	hook      func(context.Context, binding.WakeTarget) (bool, error)
	attention func(context.Context, binding.WakeTarget, int64, string) (attentionRead, error)
	connect   func(context.Context, binding.WakeTarget, int64, string) (eventConnection, error)
}

func (s runnerSource) HookSeen(ctx context.Context, target binding.WakeTarget) (bool, error) {
	if s.hook != nil {
		return s.hook(ctx, target)
	}
	return true, nil
}
func (s runnerSource) Attention(ctx context.Context, target binding.WakeTarget, cursor int64, stream string) (attentionRead, error) {
	if s.attention != nil {
		return s.attention(ctx, target, cursor, stream)
	}
	return attentionRead{Stream: "stream", Attention: hubclient.Attention{Identity: hubclient.Identity{Known: true}}}, nil
}
func (s runnerSource) Connect(ctx context.Context, target binding.WakeTarget, cursor int64, stream string) (eventConnection, error) {
	return s.connect(ctx, target, cursor, stream)
}

type runnerRead struct {
	frame eventFrame
	err   error
}
type runnerStream struct {
	reads     chan runnerRead
	closed    chan struct{}
	readCount atomic.Int64
	read      func(context.Context) (eventFrame, error)
	close     func()
	once      sync.Once
}

func newRunnerStream() *runnerStream {
	return &runnerStream{reads: make(chan runnerRead, 512), closed: make(chan struct{})}
}
func (s *runnerStream) Read(ctx context.Context) (eventFrame, error) {
	if s.read != nil {
		return s.read(ctx)
	}
	select {
	case <-ctx.Done():
		return eventFrame{}, ctx.Err()
	case result := <-s.reads:
		s.readCount.Add(1)
		return result.frame, result.err
	}
}
func (s *runnerStream) Close() {
	s.once.Do(func() {
		if s.close != nil {
			s.close()
		}
		close(s.closed)
	})
}

func runnerTarget() binding.WakeTarget {
	return binding.WakeTarget{SessionID: "019a6d62-1c75-7c28-8c15-5f7a17d4d110", Host: "runner-host", Bound: binding.Bound{ExecutionID: "execution", Generation: 1, Rung: 2}}
}
func runnerFixture(t *testing.T, resolver targetResolver, source wakeSource) *Runner {
	t.Helper()
	return &Runner{resolver: resolver, source: source, sessions: hook.NewSessions(filepath.Join(t.TempDir(), "token")),
		commands: &commands{pathFailure: "Codex executable is not available on PATH", now: time.Now}, clientVersion: func() string { return "0.167.0" },
		now: time.Now, targetPoll: time.Second, settle: 10 * time.Second, filePoll: 15 * time.Second, reconnectBase: time.Second, reconnectMax: 30 * time.Second}
}
func runnerDone(t *testing.T, done <-chan struct{}) {
	t.Helper()
	synctest.Wait()
	select {
	case <-done:
	default:
		t.Fatal("runner did not join its worker/reader")
	}
}
func runnerClosed(t *testing.T, conn *runnerStream) {
	t.Helper()
	select {
	case <-conn.closed:
	default:
		t.Fatal("runner retained an open stream")
	}
}

func TestRunnerNewWaitsForPositiveBindingAndDoesNotCreateState(t *testing.T) {
	client := &hubclient.Client{}
	resolver := binding.New(client, "codex")
	sessions := hook.NewSessions(filepath.Join(t.TempDir(), "token"))
	r := New(resolver, sessions, func() string { return "0.167.0" }, nil)
	if r.source.(hubSource).client != client || r.resolver != resolver || r.sessions != sessions || r.clientVersion() != "0.167.0" || r.commands == nil {
		t.Fatal("constructor lost its configured dependencies")
	}
	if r.targetPoll != time.Second || r.settle != 10*time.Second || r.filePoll != 15*time.Second || r.reconnectBase != time.Second || r.reconnectMax != 30*time.Second || r.now().IsZero() {
		t.Fatal("constructor lost bounded scheduling defaults")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	r.Run(ctx)
	if _, err := os.Stat(sessions.Dir()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unarmed runner created session files: %v", err)
	}
}

func TestRunnerScheduleKeepsEarliestHintAndRequiredRetry(t *testing.T) {
	now := time.Unix(1800000000, 0)
	var s schedule
	s.hint(now, 10*time.Second)
	s.hint(now.Add(9*time.Second), 10*time.Second)
	if !s.due.Equal(now.Add(10 * time.Second)) {
		t.Fatal("event burst postponed earliest settlement")
	}
	s.hint(now.Add(-time.Second), 10*time.Second)
	if !s.due.Equal(now.Add(9 * time.Second)) {
		t.Fatal("earlier candidate did not win")
	}
	s.result(stepResult{Due: now.Add(time.Minute), NotBefore: now.Add(time.Minute)})
	s.hint(now.Add(20*time.Second), 10*time.Second)
	s.hint(now.Add(80*time.Second), 10*time.Second)
	if !s.due.Equal(now.Add(time.Minute)) || !s.required.Equal(s.due) {
		t.Fatal("event bypassed or postponed required retry")
	}
	s.result(stepResult{Due: now.Add(hook.SecondNoteAfter)})
	s.hint(now.Add(time.Minute), 10*time.Second)
	if !s.due.Equal(now.Add(time.Minute+10*time.Second)) || !s.required.IsZero() {
		t.Fatal("an older identity's second-note timer delayed new attention")
	}
	s.result(stepResult{Due: now.Add(15 * time.Second), FileWait: true})
	s.hint(now, 10*time.Second)
	if !s.fileWait || !s.due.Equal(now.Add(15*time.Second)) {
		t.Fatal("incoming event bypassed file-only wait")
	}
	s.result(stepResult{})
	s.hint(now, 10*time.Second)
	if s.fileWait || !s.required.IsZero() || !s.due.Equal(now.Add(10*time.Second)) {
		t.Fatal("completed step retained a stale scheduling constraint")
	}
	hints := make(chan struct{}, 1)
	for range 100 {
		signal(hints)
	}
	if len(hints) != 1 {
		t.Fatal("burst produced more than one retained hint")
	}
	<-hints
	signal(hints)
	if len(hints) != 1 {
		t.Fatal("drained hint could not be signaled again")
	}
}

func TestRunnerNewAttentionSettlesBeforeAnOlderSecondNoteDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		target := runnerTarget()
		resolver := &runnerResolver{target: target, eligible: true}
		conn := newRunnerStream()
		cursor, reads := int64(1), 0
		source := runnerSource{
			connect: func(context.Context, binding.WakeTarget, int64, string) (eventConnection, error) {
				return conn, nil
			},
			attention: func(context.Context, binding.WakeTarget, int64, string) (attentionRead, error) {
				reads++
				return attentionRead{Stream: "stream", HighWater: 20, Attention: hubclient.Attention{
					Identity: hubclient.Identity{Known: true, Pending: true, Stream: "stream", Cursor: cursor},
				}}, nil
			},
		}
		r := runnerFixture(t, resolver, source)
		state := newStateStore(r.sessions, target.SessionID)
		engineTold(t, state, "stream", 1, time.Now(), false)
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() { defer close(done); r.Run(ctx) }()
		synctest.Wait()
		time.Sleep(10 * time.Second)
		synctest.Wait()
		if reads != 1 || mustSnapshot(t, state).Present {
			t.Fatal("already told attention did not wait for its second note")
		}
		time.Sleep(50 * time.Second)
		cursor = 2
		conn.reads <- runnerRead{frame: eventFrame{Stream: "stream", Cursor: 2, Kind: "inbox_changed"}}
		synctest.Wait()
		time.Sleep(9 * time.Second)
		synctest.Wait()
		if reads != 1 {
			t.Fatal("new attention bypassed settlement")
		}
		time.Sleep(time.Second)
		synctest.Wait()
		got := mustSnapshot(t, state)
		if reads != 2 || len(got.Document.Attempts) != 1 || got.Document.mark().Cursor != 2 {
			t.Fatalf("new attention waited for the old ten-minute timer: reads=%d, state=%+v", reads, got.Document)
		}
		cancel()
		runnerDone(t, done)
		runnerClosed(t, conn)
	})
}

func TestRunnerRunSerializesEligibleTargetReplacementAndJoinsReaders(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		target := runnerTarget()
		invalid := target
		invalid.SessionID = "not-a-uuid"
		resolver := &runnerResolver{target: invalid, eligible: true}
		var mu sync.Mutex
		var connections []*runnerStream
		var active, maxActive atomic.Int64
		closing, release := make(chan struct{}), make(chan struct{})
		source := runnerSource{connect: func(context.Context, binding.WakeTarget, int64, string) (eventConnection, error) {
			conn := newRunnerStream()
			mu.Lock()
			index := len(connections)
			connections = append(connections, conn)
			mu.Unlock()
			current := active.Add(1)
			if current > maxActive.Load() {
				maxActive.Store(current)
			}
			conn.close = func() {
				if index == 0 {
					close(closing)
					<-release
				}
				active.Add(-1)
			}
			return conn, nil
		}}
		r := runnerFixture(t, resolver, source)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan struct{})
		go func() { defer close(done); r.Run(ctx) }()
		synctest.Wait()
		time.Sleep(time.Second)
		synctest.Wait()
		if len(connections) != 0 {
			t.Fatal("invalid UUID armed a worker")
		}
		resolver.set(target, false)
		time.Sleep(time.Second)
		synctest.Wait()
		if len(connections) != 0 {
			t.Fatal("ineligible root armed a worker")
		}
		resolver.set(target, true)
		time.Sleep(time.Second)
		synctest.Wait()
		if len(connections) != 1 {
			t.Fatal("positive target did not arm")
		}
		replacement := target
		replacement.Bound.Generation++
		resolver.set(replacement, true)
		time.Sleep(time.Second)
		synctest.Wait()
		select {
		case <-closing:
		default:
			t.Fatal("generation replacement did not cancel old reader")
		}
		if len(connections) != 1 {
			t.Fatal("replacement started before old reader joined")
		}
		close(release)
		synctest.Wait()
		if len(connections) != 2 || active.Load() != 1 {
			t.Fatal("replacement failed to start after reader joined")
		}
		time.Sleep(2 * time.Second)
		synctest.Wait()
		if len(connections) != 2 {
			t.Fatal("unchanged target restarted its worker")
		}
		resolver.set(replacement, false)
		time.Sleep(time.Second)
		synctest.Wait()
		if active.Load() != 0 {
			t.Fatal("disappearing target retained a reader")
		}
		resolver.set(target, true)
		time.Sleep(time.Second)
		synctest.Wait()
		if len(connections) != 3 {
			t.Fatal("new eligible target did not recover")
		}
		cancel()
		runnerDone(t, done)
		for _, conn := range connections {
			runnerClosed(t, conn)
		}
		if active.Load() != 0 || maxActive.Load() != 1 {
			t.Fatalf("target workers overlapped: active %d maximum %d", active.Load(), maxActive.Load())
		}
	})
}

func TestRunnerDrainsFramesWhileAdmissionIsBlockedAndSettlesOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		target := runnerTarget()
		resolver := &runnerResolver{target: target, eligible: true}
		conn := newRunnerStream()
		release := make(chan struct{})
		var calls atomic.Int64
		source := runnerSource{connect: func(context.Context, binding.WakeTarget, int64, string) (eventConnection, error) { return conn, nil }, attention: func(ctx context.Context, _ binding.WakeTarget, _ int64, _ string) (attentionRead, error) {
			if calls.Add(1) == 1 {
				select {
				case <-release:
				case <-ctx.Done():
					return attentionRead{}, ctx.Err()
				}
			}
			return attentionRead{Stream: "stream", Attention: hubclient.Attention{Identity: hubclient.Identity{Known: true}}}, nil
		}}
		r := runnerFixture(t, resolver, source)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan struct{})
		go func() { defer close(done); r.runTarget(ctx, target) }()
		synctest.Wait()
		time.Sleep(9 * time.Second)
		conn.reads <- runnerRead{frame: eventFrame{Stream: "stream", Cursor: 1, Kind: "inbox_changed"}}
		synctest.Wait()
		if calls.Load() != 0 {
			t.Fatal("initial hint did not settle")
		}
		time.Sleep(time.Second)
		synctest.Wait()
		if calls.Load() != 1 {
			t.Fatal("later hint postponed the first admission")
		}
		for n := 2; n <= 301; n++ {
			conn.reads <- runnerRead{frame: eventFrame{Stream: "stream", Cursor: int64(n), Kind: "claim_available"}}
		}
		synctest.Wait()
		if conn.readCount.Load() != 301 || len(conn.reads) != 0 || calls.Load() != 1 {
			t.Fatal("blocked admission stopped stream draining or overlapped another admission")
		}
		close(release)
		synctest.Wait()
		time.Sleep(9 * time.Second)
		synctest.Wait()
		if calls.Load() != 1 {
			t.Fatal("coalesced hints bypassed the next settle time")
		}
		time.Sleep(time.Second)
		synctest.Wait()
		if calls.Load() != 2 {
			t.Fatal("coalesced hints did not produce one authoritative resample")
		}
		cancel()
		runnerDone(t, done)
		runnerClosed(t, conn)
	})
}

func TestRunnerRetainsOperationalRetryWithoutNewSocketTraffic(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		target := runnerTarget()
		resolver := &runnerResolver{target: target, eligible: true}
		conn := newRunnerStream()
		var calls atomic.Int64
		source := runnerSource{connect: func(context.Context, binding.WakeTarget, int64, string) (eventConnection, error) { return conn, nil }, hook: func(context.Context, binding.WakeTarget) (bool, error) { calls.Add(1); return false, nil }}
		r := runnerFixture(t, resolver, source)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan struct{})
		go func() { defer close(done); r.runTarget(ctx, target) }()
		synctest.Wait()
		time.Sleep(10 * time.Second)
		synctest.Wait()
		if calls.Load() != 1 {
			t.Fatal("first hook-evidence read missing")
		}
		time.Sleep(10 * time.Second)
		conn.reads <- runnerRead{frame: eventFrame{Kind: "inbox_changed", Stream: "stream", Cursor: 1}}
		synctest.Wait()
		time.Sleep(19 * time.Second)
		synctest.Wait()
		if calls.Load() != 1 {
			t.Fatal("incoming hint bypassed required retry deadline")
		}
		time.Sleep(time.Second)
		synctest.Wait()
		if calls.Load() != 2 {
			t.Fatal("transient failure did not retry without another event")
		}
		cancel()
		runnerDone(t, done)
		runnerClosed(t, conn)
	})
}

func TestRunnerFileWaitPollsOnlyStateUntilConsumptionThenSettles(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		target := runnerTarget()
		resolver := &runnerResolver{target: target, eligible: true}
		conn := newRunnerStream()
		var reads atomic.Int64
		source := runnerSource{connect: func(context.Context, binding.WakeTarget, int64, string) (eventConnection, error) { return conn, nil }, attention: func(context.Context, binding.WakeTarget, int64, string) (attentionRead, error) {
			reads.Add(1)
			return attentionRead{Stream: "stream", Attention: hubclient.Attention{Identity: hubclient.Identity{Known: true}}}, nil
		}}
		r := runnerFixture(t, resolver, source)
		store := newStateStore(r.sessions, target.SessionID)
		a := testAttempt(1, "queue", "")
		mustPublish(t, store, mustSnapshot(t, store), testDocument(a))
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan struct{})
		go func() { defer close(done); r.runTarget(ctx, target) }()
		synctest.Wait()
		time.Sleep(10 * time.Second)
		synctest.Wait()
		conn.reads <- runnerRead{frame: eventFrame{Kind: "inbox_changed", Stream: "new-stream", Cursor: 10}}
		synctest.Wait()
		time.Sleep(15 * time.Second)
		synctest.Wait()
		if reads.Load() != 0 {
			t.Fatal("outstanding state caused a hub read")
		}
		d := mustSnapshot(t, store).Document
		d.Attempts[0].ConsumedMS = time.Now().UnixMilli()
		unlock, err := r.sessions.Lock(time.Second)
		if err != nil {
			t.Fatal(err)
		}
		writeState(t, store, d)
		unlock()
		time.Sleep(15 * time.Second)
		synctest.Wait()
		if reads.Load() != 0 {
			t.Fatal("file consumption bypassed settlement")
		}
		time.Sleep(9 * time.Second)
		synctest.Wait()
		if reads.Load() != 0 {
			t.Fatal("consumed guard caused an early read")
		}
		time.Sleep(time.Second)
		synctest.Wait()
		if reads.Load() != 1 {
			t.Fatal("consumed guard failed to resume authoritative reads")
		}
		cancel()
		runnerDone(t, done)
		runnerClosed(t, conn)
	})
}

func TestRunnerPollFileClassifiesUnusableIOAndLockFailures(t *testing.T) {
	for _, kind := range []string{"absent", "outstanding", "consumed", "unusable", "io", "lock"} {
		t.Run(kind, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				r := runnerFixture(t, &runnerResolver{}, runnerSource{})
				store := testStore(t)
				delay := r.settle
				blocked := false
				var unlock func()
				switch kind {
				case "outstanding", "consumed":
					a := testAttempt(1, "queue", "")
					if kind == "consumed" {
						a.ConsumedMS = 200
					} else {
						blocked = true
						delay = r.filePoll
					}
					mustPublish(t, store, mustSnapshot(t, store), testDocument(a))
				case "unusable":
					_ = mustSnapshot(t, store)
					if err := os.WriteFile(store.path(), []byte("damaged guard"), 0600); err != nil {
						t.Fatal(err)
					}
					blocked = true
					delay = r.filePoll
				case "io":
					store.io.readFile = func(string) ([]byte, error) { return nil, errStateTest }
					blocked = true
					delay = time.Minute
				case "lock":
					_ = mustSnapshot(t, store)
					var err error
					unlock, err = store.sessions.Lock(time.Second)
					if err != nil {
						t.Fatal(err)
					}
					defer unlock()
					blocked = true
					delay = 10 * time.Second
				}
				result := r.pollFile(store)
				if result.fileWait != blocked || !result.due.Equal(time.Now().Add(delay)) || !result.required.IsZero() {
					t.Fatalf("wrong file-only retry for %s: %+v", kind, result)
				}
			})
		})
	}
}

func TestRunnerStreamHintsOnlyAttentionButRetainsEveryReconnectCursor(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		conn := newRunnerStream()
		hints := make(chan struct{}, 1)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		var cursor int64
		var stream string
		done := make(chan bool, 1)
		r := &Runner{}
		go func() { done <- r.readStream(ctx, conn, hints, &cursor, &stream) }()
		synctest.Wait()
		for _, tc := range []struct {
			frame  eventFrame
			hint   bool
			cursor int64
			stream string
		}{
			{eventFrame{"stream", 5, "changed"}, false, 5, "stream"},
			{eventFrame{"stream", 4, "heartbeat"}, false, 5, "stream"},
			{eventFrame{"stream", 6, "inbox_changed"}, true, 6, "stream"},
			{eventFrame{"stream", 7, "claim_available"}, true, 7, "stream"},
			{eventFrame{"stream", 9, "claim_released"}, false, 9, "stream"},
			{eventFrame{"", 99, "inbox_changed"}, true, 9, "stream"},
			{eventFrame{"stream", 2, "reset"}, true, 2, "stream"},
			{eventFrame{"stream", 1, "registration"}, false, 2, "stream"},
			{eventFrame{"restored", 1, "changed"}, false, 1, "restored"},
		} {
			conn.reads <- runnerRead{frame: tc.frame}
			synctest.Wait()
			if (len(hints) == 1) != tc.hint || cursor != tc.cursor || stream != tc.stream {
				t.Fatalf("frame routing changed hint/cursor: %+v, hint %d cursor %d stream %q", tc, len(hints), cursor, stream)
			}
			if tc.hint {
				<-hints
			}
		}
		cancel()
		synctest.Wait()
		if received := <-done; !received {
			t.Fatal("valid stream did not reset reconnect backoff")
		}
		runnerClosed(t, conn)
	})
}

func TestRunnerReconnectBackoffCapsAndResetsOnlyAfterAFrame(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		var calls []time.Duration
		var cursors []int64
		var streams []string
		first, last := newRunnerStream(), newRunnerStream()
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		source := runnerSource{connect: func(_ context.Context, _ binding.WakeTarget, cursor int64, stream string) (eventConnection, error) {
			calls = append(calls, time.Since(start))
			cursors = append(cursors, cursor)
			streams = append(streams, stream)
			switch len(calls) {
			case 1, 2:
				return nil, errStateTest
			case 3:
				closed := newRunnerStream()
				closed.reads <- runnerRead{err: errStateTest}
				return closed, nil
			case 4:
				return nil, errStateTest
			case 5:
				return first, nil
			default:
				return last, nil
			}
		}}
		r := runnerFixture(t, &runnerResolver{}, source)
		r.reconnectMax = 4 * time.Second
		hints := make(chan struct{}, 1)
		done := make(chan struct{})
		go func() { defer close(done); r.followStream(ctx, runnerTarget(), hints) }()
		synctest.Wait()
		time.Sleep(11 * time.Second)
		synctest.Wait()
		if !reflect.DeepEqual(calls, []time.Duration{0, time.Second, 3 * time.Second, 7 * time.Second, 11 * time.Second}) {
			t.Fatalf("connection acceptance reset exponential failure backoff: %v", calls)
		}
		first.reads <- runnerRead{frame: eventFrame{Stream: "stream", Cursor: 8, Kind: "changed"}}
		first.reads <- runnerRead{err: errStateTest}
		synctest.Wait()
		runnerClosed(t, first)
		time.Sleep(time.Second)
		synctest.Wait()
		if len(calls) != 6 || calls[5] != 12*time.Second || cursors[5] != 8 || streams[5] != "stream" {
			t.Fatalf("frame did not reset backoff or preserve reconnect identity: %v %v %v", calls, cursors, streams)
		}
		cancel()
		runnerDone(t, done)
		runnerClosed(t, last)
		// A canceled parent never starts a connection, even with a previously queued hint.
		before := len(calls)
		r.followStream(ctx, runnerTarget(), hints)
		if len(calls) != before {
			t.Fatal("canceled follow started another connection")
		}
	})
}

func TestRunnerPauseStopsOnCancellationAndCompletesDelay(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if pause(ctx, time.Hour) {
			t.Fatal("cancellation waited for reconnect delay")
		}
		before := time.Now()
		if !pause(t.Context(), time.Second) || time.Since(before) != time.Second {
			t.Fatal("reconnect delay was skipped")
		}
	})
}

func TestRunnerPanicsCloseWorkersWithoutCancelingTheCallerOrLeakingText(t *testing.T) {
	for _, where := range []string{"primary", "connect", "read", "hook", "attention"} {
		t.Run(where, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				target := runnerTarget()
				resolver := &runnerResolver{target: target, eligible: true}
				conn := newRunnerStream()
				var connectCalls atomic.Int64
				var mu sync.Mutex
				var observed []string
				panicNow := func() { panic("sensitive panic text must not become an observation") }
				source := runnerSource{connect: func(context.Context, binding.WakeTarget, int64, string) (eventConnection, error) {
					connectCalls.Add(1)
					if where == "connect" {
						panicNow()
					}
					return conn, nil
				}}
				switch where {
				case "primary":
					resolver.primary = func() (binding.WakeTarget, bool) { panicNow(); return target, true }
				case "read":
					conn.read = func(context.Context) (eventFrame, error) { panicNow(); return eventFrame{}, nil }
				case "hook":
					source.hook = func(context.Context, binding.WakeTarget) (bool, error) { panicNow(); return false, nil }
				case "attention":
					source.attention = func(context.Context, binding.WakeTarget, int64, string) (attentionRead, error) {
						panicNow()
						return attentionRead{}, nil
					}
				}
				r := runnerFixture(t, resolver, source)
				r.observe = func(reason string) { mu.Lock(); defer mu.Unlock(); observed = append(observed, reason) }
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				done := make(chan struct{})
				go func() { defer close(done); r.Run(ctx) }()
				synctest.Wait()
				if where == "hook" || where == "attention" {
					time.Sleep(10 * time.Second)
					synctest.Wait()
				}
				if ctx.Err() != nil {
					t.Fatal("worker panic canceled the MCP caller")
				}
				mu.Lock()
				panics := 0
				for _, reason := range observed {
					if strings.Contains(reason, "sensitive") {
						t.Fatal("panic text leaked")
					}
					if reason == "wake_worker_panic" {
						panics++
					}
				}
				mu.Unlock()
				if panics != 1 {
					t.Fatalf("panic did not produce exactly one fixed observation: %v", observed)
				}
				if where == "read" || where == "hook" || where == "attention" {
					runnerClosed(t, conn)
				}
				before := connectCalls.Load()
				time.Sleep(time.Minute)
				synctest.Wait()
				if connectCalls.Load() != before {
					t.Fatal("worker panic automatically retried the same target")
				}
				cancel()
				runnerDone(t, done)
			})
		})
	}
}
