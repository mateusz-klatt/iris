package wake

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/binding"
	"github.com/mateusz-klatt/iris/internal/hook"
	"github.com/mateusz-klatt/iris/internal/hubclient"
)

type engineResolver struct {
	target   binding.WakeTarget
	eligible bool
	admit    func(func() error) (bool, error)
	starts   int
}

func (r *engineResolver) Primary() (binding.WakeTarget, bool) { return r.target, r.eligible }
func (r *engineResolver) StartIfPrimary(target binding.WakeTarget, start func() error) (bool, error) {
	if !r.eligible || target != r.target {
		return false, nil
	}
	if r.admit != nil {
		return r.admit(start)
	}
	r.starts++
	return commandStarted(start)
}

type engineSource struct {
	target           binding.WakeTarget
	seen             bool
	hookErr, readErr error
	hooks, reads     int
	read             attentionRead
	duringRead       func()
	requestCursor    int64
	requestStream    string
	check            func(context.Context)
}

func (s *engineSource) HookSeen(ctx context.Context, target binding.WakeTarget) (bool, error) {
	s.hooks++
	s.target = target
	if s.check != nil {
		s.check(ctx)
	}
	return s.seen, s.hookErr
}
func (s *engineSource) Attention(ctx context.Context, target binding.WakeTarget, cursor int64, stream string) (attentionRead, error) {
	s.reads++
	s.target, s.requestCursor, s.requestStream = target, cursor, stream
	if s.check != nil {
		s.check(ctx)
	}
	if s.duringRead != nil {
		s.duringRead()
	}
	return s.read, s.readErr
}
func (*engineSource) Connect(context.Context, binding.WakeTarget, int64, string) (eventConnection, error) {
	panic("an engine step must not open an event stream")
}

func newEngineTest(t *testing.T) (*engine, *engineResolver, *engineSource, *time.Time) {
	t.Helper()
	now := time.Unix(1800000000, 0)
	target := binding.WakeTarget{SessionID: commandSession, Host: "engine-host", Bound: binding.Bound{ExecutionID: "execution", Generation: 1, Rung: 2}}
	r := &engineResolver{target: target, eligible: true}
	source := &engineSource{seen: true, read: attentionRead{Stream: "stream", HighWater: 20, Attention: hubclient.Attention{
		Identity: hubclient.Identity{Known: true, Pending: true, Stream: "stream", Cursor: 7}, HighWater: 20,
	}}}
	e := &engine{target: target, resolver: r, source: source,
		store:         newStateStore(hook.NewSessions(filepath.Join(t.TempDir(), "token")), target.SessionID),
		commands:      &commands{pathFailure: "Codex executable is not available on PATH", now: func() time.Time { return now }},
		clientVersion: func() string { return commandVersion }, now: func() time.Time { return now },
	}
	return e, r, source, &now
}

func engineDue(now time.Time, delay time.Duration) stepResult {
	due := now.Add(delay)
	return stepResult{Due: due, NotBefore: due}
}

func engineTold(t *testing.T, s *stateStore, stream string, cursor int64, at time.Time, twice bool) {
	t.Helper()
	if err := s.protect(); err != nil {
		t.Fatal(err)
	}
	second := 0
	if twice {
		second = 1
	}
	line := fmt.Sprintf("1 0 1 0 %s %d %d %d 9\n", hex.EncodeToString([]byte(stream)), cursor, at.UnixMilli(), second)
	if err := os.WriteFile(toldPath(s), []byte(line), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestWakeEngineRequiresCurrentValidTargetBeforeAnyIO(t *testing.T) {
	for _, condition := range []string{"invalid UUID", "not primary", "replacement", "cancelled"} {
		t.Run(condition, func(t *testing.T) {
			e, r, source, _ := newEngineTest(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch condition {
			case "invalid UUID":
				e.target.SessionID = "not a UUID"
				r.target = e.target
			case "not primary":
				r.eligible = false
			case "replacement":
				r.target.Bound.Generation++
			case "cancelled":
				cancel()
			}
			if got := e.step(ctx); got != (stepResult{}) || source.hooks != 0 || source.reads != 0 {
				t.Fatalf("invalid target performed work: %+v, source %+v", got, source)
			}
			if _, err := os.Stat(e.store.sessions.Dir()); !os.IsNotExist(err) {
				t.Fatalf("invalid target touched state: %v", err)
			}
		})
	}
}

func TestWakeEngineOutstandingAndUnusableStateNeverAskTheHub(t *testing.T) {
	for _, condition := range []string{"outstanding", "unknown outstanding", "unusable"} {
		t.Run(condition, func(t *testing.T) {
			e, _, source, now := newEngineTest(t)
			_ = mustSnapshot(t, e.store)
			if condition == "unusable" {
				if err := os.WriteFile(e.store.path(), []byte("malformed retained guard"), 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				a := testAttempt(1, "queue", "submitted")
				if condition == "unknown outstanding" {
					a.Answer = "future uncertain outcome"
				}
				writeState(t, e.store, testDocument(a))
			}
			before, err := os.ReadFile(e.store.path())
			if err != nil {
				t.Fatal(err)
			}
			got := e.step(context.Background())
			if got != (stepResult{Due: now.Add(15 * time.Second), FileWait: true}) || source.hooks != 0 || source.reads != 0 {
				t.Fatalf("retained state consulted hub: %+v, source %+v", got, source)
			}
			after, err := os.ReadFile(e.store.path())
			if err != nil || string(after) != string(before) {
				t.Fatal("refusal changed retained guard bytes")
			}
		})
	}
}

func TestWakeEngineSourceEvidenceDeadlinesAndCursor(t *testing.T) {
	e, _, source, now := newEngineTest(t)
	source.check = func(ctx context.Context) {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 5*time.Second || time.Until(deadline) <= 0 {
			t.Errorf("source call has no five-second bound: %v, %t", deadline, ok)
		}
		unlock, err := e.store.sessions.Lock(0)
		if err != nil {
			t.Errorf("source call held sessions lock: %v", err)
			return
		}
		unlock()
	}
	source.seen = false
	if got := e.step(context.Background()); got != engineDue(*now, 30*time.Second) || source.reads != 0 || e.hookSeen {
		t.Fatalf("absent hook evidence armed runner: %+v", got)
	}
	source.seen, source.hookErr = true, errors.New(commandSecret)
	if got := e.step(context.Background()); got != engineDue(*now, 30*time.Second) || e.hookSeen {
		t.Fatalf("failed hook proof armed runner: %+v", got)
	}
	source.hookErr, source.readErr = nil, errors.New(commandSecret)
	if got := e.step(context.Background()); got != engineDue(*now, 30*time.Second) || !e.hookSeen {
		t.Fatalf("attention read failure: %+v", got)
	}
	source.readErr = nil
	source.read.Attention.Identity = hubclient.Identity{Known: true}
	if got := e.step(context.Background()); got != (stepResult{}) || source.hooks != 3 || e.stream != "stream" || e.cursor != 20 {
		t.Fatalf("null pending or cached proof: %+v, hooks %d, stream %q cursor %d", got, source.hooks, e.stream, e.cursor)
	}
	source.read.Stream, source.read.HighWater = "restored", 3
	if got := e.step(context.Background()); got != (stepResult{}) || source.requestCursor != 20 || source.requestStream != "stream" || source.target != e.target || e.stream != "restored" || e.cursor != 3 {
		t.Fatalf("authoritative read did not follow its own cursor/stream: %+v, source %+v", got, source)
	}
}

func TestWakeEngineUnknownOrInconsistentIdentityNeverSubmits(t *testing.T) {
	for _, change := range []func(*attentionRead){
		func(r *attentionRead) { r.Attention.Identity.Known = false },
		func(r *attentionRead) { r.Stream = "" },
		func(r *attentionRead) { r.HighWater = -1 },
		func(r *attentionRead) { r.Attention.Identity.Stream = "other stream" },
		func(r *attentionRead) { r.Attention.Identity.Cursor = 0 },
		func(r *attentionRead) { r.Attention.Identity.Cursor = r.HighWater + 1 },
	} {
		e, _, source, now := newEngineTest(t)
		change(&source.read)
		if got := e.step(context.Background()); got != engineDue(*now, 30*time.Second) || mustSnapshot(t, e.store).Present {
			t.Fatalf("unknown identity created allowance: %+v", got)
		}
	}
}

func TestWakeAdmissionSlotsIdentityBoundariesAndRestoreProof(t *testing.T) {
	now := time.Unix(1800000000, 0)
	id := hubclient.Identity{Known: true, Pending: true, Stream: "stream", Cursor: 7}
	consumed := testAttempt(1, "queue", "submitted")
	consumed.ConsumedMS = now.Add(-time.Minute).UnixMilli()
	for _, tc := range []struct {
		name     string
		change   func(*stateSnapshot)
		slot     int
		due      time.Time
		eligible bool
		retained int
	}{
		{name: "untold", eligible: true},
		{name: "newer than told", change: func(s *stateSnapshot) { s.Told.Identity.Cursor = 6 }, eligible: true},
		{name: "other told stream", change: func(s *stateSnapshot) { s.Told.Identity.Stream = "old" }, eligible: true},
		{name: "older than told", change: func(s *stateSnapshot) { s.Told.Identity.Cursor = 8 }},
		{name: "same told before deadline", change: func(s *stateSnapshot) { s.Told.ToldMS = now.UnixMilli() }, slot: 1, due: now.Add(hook.SecondNoteAfter)},
		{name: "second note deadline", slot: 1, eligible: true},
		{name: "already twice", change: func(s *stateSnapshot) { s.Told.Twice = true }},
		{name: "missing telling time", change: func(s *stateSnapshot) { s.Told.ToldMS = 0 }},
		{name: "spent mark", change: func(s *stateSnapshot) { s.Told.Identity = hubclient.Identity{}; s.Document = testDocument(consumed) }, retained: 1},
		{name: "older than mark", change: func(s *stateSnapshot) {
			s.Told.Identity = hubclient.Identity{}
			a := consumed
			a.Cursor = 8
			s.Document = testDocument(a)
		}, retained: 1},
		{name: "newer than mark", change: func(s *stateSnapshot) { a := consumed; a.Cursor = 6; s.Document = testDocument(a) }, slot: 1, eligible: true},
		{name: "other mark stream", change: func(s *stateSnapshot) { a := consumed; a.Stream = "old"; s.Document = testDocument(a) }, slot: 1, eligible: true},
		{name: "mark proves restore", change: func(s *stateSnapshot) { a := consumed; a.Cursor = 21; s.Document = testDocument(a) }, slot: 1, eligible: true},
		{name: "stale restored told cannot reset mark", change: func(s *stateSnapshot) { s.Told.Identity.Cursor = 21; s.Document = testDocument(consumed) }, retained: 1},
		{name: "one failed retry deadline", change: func(s *stateSnapshot) {
			s.Told.Identity = hubclient.Identity{}
			a := testAttempt(1, "binary_check", "failed")
			a.AnsweredMS = now.UnixMilli()
			s.Document = testDocument(a)
		}, due: now.Add(time.Minute), retained: 1},
		{name: "one failed retry now", change: func(s *stateSnapshot) {
			s.Told.Identity = hubclient.Identity{}
			a := testAttempt(1, "binary_check", "failed")
			a.AnsweredMS = now.Add(-time.Minute).UnixMilli()
			s.Document = testDocument(a)
		}, eligible: true, retained: 1},
		{name: "aborted retry no quota", change: func(s *stateSnapshot) {
			s.Told.Identity = hubclient.Identity{}
			s.Document = testDocument(testAttempt(1, "binary_check", "aborted"))
		}, eligible: true, retained: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snapshot := stateSnapshot{Document: testDocument(), Told: hook.ToldSnapshot{Identity: id, ToldMS: now.Add(-hook.SecondNoteAfter).UnixMilli()}}
			if tc.name == "untold" {
				snapshot.Told = hook.ToldSnapshot{}
			}
			if tc.change != nil {
				tc.change(&snapshot)
			}
			next, slot, due, eligible := admission(snapshot, id, 20, now)
			if slot != tc.slot || !due.Equal(tc.due) && !(tc.eligible && !due.After(now)) || eligible != tc.eligible || len(next.Attempts) != tc.retained {
				t.Fatalf("admission: slot %d, due %v, eligible %t, retained %d; want %+v", slot, due, eligible, len(next.Attempts), tc)
			}
		})
	}
}

func TestWakeEngineSnapshotRacesNeverSpendQuotaAndEventuallyProgress(t *testing.T) {
	for _, mutation := range []string{"presence", "prune recreate same revision", "twice same revision", "whole bytes", "wake bytes"} {
		t.Run(mutation, func(t *testing.T) {
			e, _, source, now := newEngineTest(t)
			if mutation != "presence" {
				engineTold(t, e.store, "old", 1, now.Add(-time.Hour), false)
			}
			if mutation == "wake bytes" {
				a := testAttempt(1, "binary_check", "aborted")
				writeState(t, e.store, testDocument(a))
			}
			source.duringRead = func() {
				switch mutation {
				case "presence":
					engineTold(t, e.store, "old", 1, now.Add(-time.Hour), false)
				case "prune recreate same revision":
					if err := os.Remove(toldPath(e.store)); err != nil {
						t.Fatal(err)
					}
					engineTold(t, e.store, "old", 1, now.Add(-2*time.Hour), false)
				case "twice same revision":
					engineTold(t, e.store, "old", 1, now.Add(-time.Hour), true)
				case "whole bytes":
					f, err := os.OpenFile(toldPath(e.store), os.O_APPEND|os.O_WRONLY, 0600)
					if err != nil {
						t.Fatal(err)
					}
					_, err = f.WriteString("whole-file bytes outside parsed first line\n")
					if closeErr := f.Close(); err != nil || closeErr != nil {
						t.Fatalf("mutate told bytes: %v, %v", err, closeErr)
					}
				case "wake bytes":
					d := mustSnapshot(t, e.store).Document
					d.Attempts[0].Detail = "changed without revision"
					writeState(t, e.store, d)
				}
			}
			if got := e.step(context.Background()); got != engineDue(*now, 10*time.Second) {
				t.Fatalf("stale hub answer admitted: %+v", got)
			}
			before := mustSnapshot(t, e.store)
			if before.Document.quota(0) != 0 {
				t.Fatal("snapshot race spent quota")
			}
			source.duringRead = nil
			*now = now.Add(10 * time.Second)
			if got := e.step(context.Background()); got != engineDue(*now, 60*time.Second) || len(mustSnapshot(t, e.store).Document.Attempts) != 1 {
				t.Fatalf("settled writer could not progress: %+v", got)
			}
		})
	}
}

func TestWakeEngineFileFailureRetryClassesAndPostReadFailure(t *testing.T) {
	for _, tc := range []struct {
		name     string
		err      error
		delay    time.Duration
		fileWait bool
		postRead bool
	}{
		{"lock budget", hook.ErrSessionsLocked, 10 * time.Second, false, false},
		{"changed snapshot", errStateChanged, 10 * time.Second, false, false},
		{"read failure", errors.New(commandSecret), 60 * time.Second, false, false},
		{"unusable", errStateUnusable, 15 * time.Second, true, false},
		{"post-read failure", errors.New(commandSecret), 60 * time.Second, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, _, source, now := newEngineTest(t)
			var observed []string
			e.observe = func(reason string) { observed = append(observed, reason) }
			fail := func() { e.store.io.readFile = func(string) ([]byte, error) { return nil, tc.err } }
			if tc.postRead {
				source.duringRead = fail
			} else {
				fail()
			}
			want := stepResult{Due: now.Add(tc.delay), FileWait: tc.fileWait}
			if !tc.fileWait {
				want.NotBefore = want.Due
			}
			if got := e.step(context.Background()); got != want || (!tc.postRead && (source.hooks != 0 || source.reads != 0)) {
				t.Fatalf("file failure: %+v, want %+v, source %+v", got, want, source)
			}
			if len(observed) != 1 || strings.Contains(observed[0], commandSecret) {
				t.Fatalf("raw failure reached observation: %q", observed)
			}
		})
	}
}

func TestWakeEngineTargetReplacementStopsEachAdmissionPhase(t *testing.T) {
	for _, phase := range []string{"after hub", "after binary", "after publication"} {
		t.Run(phase, func(t *testing.T) {
			e, r, source, _ := newEngineTest(t)
			replace := func() { r.target.Bound.Generation++ }
			switch phase {
			case "after hub":
				source.duringRead = replace
			case "after binary":
				e.clientVersion = func() string { replace(); return commandVersion }
			case "after publication":
				e.store.io.syncDir = func(string) error { replace(); return nil }
			}
			if got := e.step(context.Background()); got != (stepResult{}) || r.starts != 0 {
				t.Fatalf("replaced target continued admission: %+v", got)
			}
			d := mustSnapshot(t, e.store).Document
			if phase == "after publication" {
				if len(d.Attempts) != 1 || d.Attempts[0].Answer != "aborted" || d.quota(0) != 0 {
					t.Fatalf("replaced binary reservation retained quota: %+v", d)
				}
			} else if len(d.Attempts) != 0 {
				t.Fatal("replaced target published a reservation")
			}
		})
	}
}

func TestWakeEngineBinaryFailuresHaveUniqueIDsAndOneBoundedRetry(t *testing.T) {
	e, _, source, now := newEngineTest(t)
	var observed []string
	e.observe = func(reason string) { observed = append(observed, reason) }
	if got := e.step(context.Background()); got != engineDue(*now, time.Minute) {
		t.Fatalf("first binary failure retry: %+v", got)
	}
	first := mustSnapshot(t, e.store).Document.Attempts[0]
	if first.ID == "" || first.Kind != "binary_check" || first.Token != "" || first.Message != "" || first.Answer != "failed" || first.AnsweredMS != now.UnixMilli() {
		t.Fatalf("binary failure was not a tokenless counted reservation: %+v", first)
	}
	*now = now.Add(59 * time.Second)
	if got := e.step(context.Background()); got.Due != time.UnixMilli(first.AnsweredMS).Add(time.Minute) || len(mustSnapshot(t, e.store).Document.Attempts) != 1 {
		t.Fatalf("binary failure retried early: %+v", got)
	}
	*now = now.Add(time.Second)
	if got := e.step(context.Background()); got != (stepResult{}) {
		t.Fatalf("second known failure granted more retries: %+v", got)
	}
	d := mustSnapshot(t, e.store).Document
	if len(d.Attempts) != 2 || d.Attempts[0] != first || d.Attempts[1].ID == first.ID || d.quota(0) != 2 || d.LastFailure == nil || source.hooks != 1 {
		t.Fatalf("binary failures were reused or over-admitted: %+v", d)
	}
	if !reflect.DeepEqual(observed, []string{"wake_failed", "wake_failed"}) {
		t.Fatalf("non-fixed or extra observations: %q", observed)
	}
}

func TestWakeEngineSecondNoteWaitsExactlyTenMinutes(t *testing.T) {
	e, _, source, now := newEngineTest(t)
	toldAt := now.Add(-hook.SecondNoteAfter + time.Millisecond)
	engineTold(t, e.store, "stream", 7, toldAt, false)
	if got := e.step(context.Background()); got != (stepResult{Due: toldAt.Add(hook.SecondNoteAfter)}) || mustSnapshot(t, e.store).Present {
		t.Fatalf("second note ran before required due time: %+v", got)
	}
	*now = now.Add(time.Millisecond)
	if got := e.step(context.Background()); got != engineDue(*now, time.Minute) || mustSnapshot(t, e.store).Document.Attempts[0].Slot != 1 || source.hooks != 1 {
		t.Fatalf("second-note deadline did not admit its slot: %+v", got)
	}
}

func TestWakeEnginePublishRechecksToldAfterBinaryAndRetriesPreparationFailure(t *testing.T) {
	for _, failure := range []string{"prepare", "changed told"} {
		t.Run(failure, func(t *testing.T) {
			e, _, _, now := newEngineTest(t)
			create := e.store.io.createTemp
			e.store.io.createTemp = func(dir, pattern string) (stateFile, error) {
				if failure == "prepare" {
					return nil, errors.New(commandSecret)
				}
				engineTold(t, e.store, "stream", 7, *now, true)
				return create(dir, pattern)
			}
			delay := time.Minute
			if failure == "changed told" {
				delay = 10 * time.Second
			}
			if got := e.step(context.Background()); got != engineDue(*now, delay) || mustSnapshot(t, e.store).Present {
				t.Fatalf("publication failure spent quota: %+v", got)
			}
		})
	}
}

func TestWakeEngineBinaryPublicationAbortAddressesOnlyItsReservation(t *testing.T) {
	for _, failAbort := range []bool{false, true} {
		t.Run(fmt.Sprintf("abort_failure_%t", failAbort), func(t *testing.T) {
			e, _, _, now := newEngineTest(t)
			first := testAttempt(1, "binary_check", "failed")
			first.AnsweredMS = now.Add(-time.Minute).UnixMilli()
			writeState(t, e.store, testDocument(first))
			syncs, renames := 0, 0
			rename := e.store.io.rename
			e.store.io.rename = func(from, to string) error {
				renames++
				if failAbort && renames > 1 {
					return errors.New(commandSecret)
				}
				return rename(from, to)
			}
			e.store.io.syncDir = func(string) error {
				syncs++
				if syncs == 1 {
					return errors.New(commandSecret)
				}
				return nil
			}
			if got := e.step(context.Background()); got != engineDue(*now, time.Minute) {
				t.Fatalf("failed publication fallback: %+v", got)
			}
			d := mustSnapshot(t, e.store).Document
			if len(d.Attempts) != 2 || d.Attempts[0] != first || d.Attempts[1].ID == first.ID || !d.valid() {
				t.Fatalf("publication addressed the wrong binary failure: %+v", d)
			}
			wantAnswer, wantQuota := "aborted", 1
			if failAbort {
				wantAnswer, wantQuota = "failed", 2
				if renames != 4 {
					t.Fatalf("abort persistence was not bounded to three attempts: %d", renames-1)
				}
			}
			if d.Attempts[1].Answer != wantAnswer || d.quota(0) != wantQuota {
				t.Fatalf("binary abort fallback: %+v", d)
			}
			if !failAbort {
				e.store.io.syncDir, e.store.io.rename = func(string) error { return nil }, rename
				if got := e.step(context.Background()); got != (stepResult{}) || len(mustSnapshot(t, e.store).Document.Attempts) != 2 {
					t.Fatalf("reopened tokenless abort could not progress: %+v", got)
				}
			}
		})
	}
}

func TestWakeEngineStaleRestoredToldNeverReopensSpentQuotaAcrossRestart(t *testing.T) {
	e, _, source, now := newEngineTest(t)
	engineTold(t, e.store, "stream", source.read.HighWater+1, now.Add(-time.Hour), false)
	a := testAttempt(1, "queue", "submitted")
	a.ConsumedMS = now.UnixMilli()
	writeState(t, e.store, testDocument(a))
	before := mustSnapshot(t, e.store)
	for range 3 {
		if got := e.step(context.Background()); got != (stepResult{}) {
			t.Fatalf("stale pre-restore told state reopened spent wake quota: %+v", got)
		}
		after := mustSnapshot(t, e.store)
		if !sameState(before, after) || after.Document.quota(0) != 2 {
			t.Fatal("restored told state changed a spent lower/equal wake mark")
		}
		// Neither a new binding engine nor a process-style state-store reopen
		// can treat the old told cursor as fresh restore evidence for this mark.
		restarted := *e
		restarted.hookSeen = false
		restarted.store = newStateStore(e.store.sessions, e.target.SessionID)
		e = &restarted
		*now = now.Add(time.Hour)
	}
}

func TestWakeEngineProcessAdmissionAndOutcomeMerging(t *testing.T) {
	binary := commandBinary(t)
	for _, tc := range []struct {
		name     string
		scenario commandScenario
		outcome  string
	}{
		{"submitted", commandScenario{Out: commandAccept + "\n"}, "submitted"},
		{"stored", commandScenario{Out: commandAccept + "\n", Err: commandWarning + "\n"}, "stored"},
		{"known rejection", commandScenario{Err: commandReject + "\n", Exit: 1}, "failed"},
		{"unknown output", commandScenario{Err: commandSecret}, "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, r, source, now := newEngineTest(t)
			scenario := tc.scenario
			scenario.VersionOut = "codex-cli " + commandVersion + "\n"
			c, _, _ := commandSetup(t, binary, scenario)
			c.now = e.now
			e.commands = c
			// Queue uses the canonical UUID, but hub and file lookups keep the
			// client's registered spelling throughout the same admission.
			e.target.SessionID = strings.ToUpper(commandSession)
			r.target = e.target
			e.store = newStateStore(e.store.sessions, e.target.SessionID)
			synced := false
			e.store.io.syncDir = func(string) error { synced = true; return nil }
			r.admit = func(start func() error) (bool, error) {
				d := mustSnapshot(t, e.store).Document
				if !synced || len(d.Attempts) != 1 || !d.Attempts[0].outstanding() || d.Attempts[0].Answer != "" {
					t.Fatalf("command started before reservation publication/read-back/sync: %+v", d)
				}
				a := d.Attempts[0]
				token, err := hex.DecodeString(a.Token)
				if err != nil || len(token) != 16 || a.ID == a.Token || a.Message != promptFor(a.Token) || a.Stream != "stream" || a.Cursor != 7 {
					t.Fatalf("queue reservation identity or fixed prompt: %+v", a)
				}
				r.starts++
				return commandStarted(start)
			}
			var observed []string
			e.observe = func(reason string) { observed = append(observed, reason) }
			got := e.step(context.Background())
			want := stepResult{Due: now.Add(15 * time.Second), FileWait: true}
			if tc.outcome == "failed" {
				want = engineDue(*now, time.Minute)
			}
			if got != want || r.starts != 1 || source.target.SessionID != e.target.SessionID {
				t.Fatalf("queue step: %+v, want %+v, starts %d", got, want, r.starts)
			}
			d := mustSnapshot(t, e.store).Document
			if d.Attempts[0].Answer != tc.outcome || d.Attempts[0].AnsweredMS != now.UnixMilli() || strings.Contains(d.Attempts[0].Detail, commandSecret) || d.Binary.Version != commandVersion {
				t.Fatalf("queue outcome was not safely merged: %+v", d)
			}
			for _, reason := range observed {
				if reason != "wake_failed" && reason != "wake_unknown" {
					t.Fatalf("unbounded observation: %q", reason)
				}
			}
		})
	}

	t.Run("admission refusal versus actual Start failure", func(t *testing.T) {
		for _, refuse := range []bool{false, true} {
			t.Run(fmt.Sprintf("refuse_%t", refuse), func(t *testing.T) {
				e, r, _, now := newEngineTest(t)
				c, record, _ := commandSetup(t, binary, commandScenario{VersionOut: "codex-cli " + commandVersion + "\n", Out: commandAccept + "\n"})
				e.commands = c
				if refuse {
					r.admit = func(func() error) (bool, error) { r.eligible = false; return false, nil }
				} else {
					e.store.io.syncDir = func(string) error { c.path = filepath.Join(t.TempDir(), "missing"); return nil }
				}
				got := e.step(context.Background())
				want, outcome := engineDue(*now, time.Minute), "failed"
				if refuse {
					want, outcome = stepResult{}, "aborted"
				}
				if got != want || mustSnapshot(t, e.store).Document.Attempts[0].Answer != outcome {
					t.Fatalf("pre-start result: %+v, want %+v and %s", got, want, outcome)
				}
				body, err := os.ReadFile(record)
				if err != nil || strings.Contains(string(body), `"queue"`) {
					t.Fatalf("pre-start refusal/failure executed a queue process: %v", err)
				}
			})
		}
	})

	t.Run("consumption and newer record survive late answer", func(t *testing.T) {
		e, r, _, now := newEngineTest(t)
		c, _, _ := commandSetup(t, binary, commandScenario{VersionOut: "codex-cli " + commandVersion + "\n", Out: commandAccept + "\n"})
		e.commands = c
		var original, newer attempt
		r.admit = func(start func() error) (bool, error) {
			started, err := commandStarted(start)
			if err != nil {
				return started, err
			}
			d := mustSnapshot(t, e.store).Document
			d.Attempts[0].ConsumedMS = now.UnixMilli()
			original = d.Attempts[0]
			newer = testAttempt(99, "binary_check", "failed")
			d.Attempts = append(d.Attempts, newer)
			writeState(t, e.store, d)
			return started, nil
		}
		if got := e.step(context.Background()); !got.FileWait {
			t.Fatalf("command did not finish its accounting: %+v", got)
		}
		d := mustSnapshot(t, e.store).Document
		if len(d.Attempts) != 2 || d.Attempts[0].ID != original.ID || d.Attempts[0].ConsumedMS != original.ConsumedMS || d.Attempts[0].Answer != "submitted" || d.Attempts[1] != newer {
			t.Fatalf("late answer changed hook consumption or newer record: %+v", d)
		}
	})

	t.Run("answer retries are bounded and retained guard blocks further reads", func(t *testing.T) {
		for _, recoverWrite := range []bool{false, true} {
			t.Run(fmt.Sprintf("write_recovers_%t", recoverWrite), func(t *testing.T) {
				e, r, source, _ := newEngineTest(t)
				c, _, _ := commandSetup(t, binary, commandScenario{VersionOut: "codex-cli " + commandVersion + "\n", Out: commandAccept + "\n"})
				e.commands = c
				attempts := 0
				r.admit = func(start func() error) (bool, error) {
					rename := e.store.io.rename
					e.store.io.rename = func(from, to string) error {
						attempts++
						if !recoverWrite || attempts < 3 {
							return errors.New(commandSecret)
						}
						return rename(from, to)
					}
					return commandStarted(start)
				}
				if got := e.step(context.Background()); !got.FileWait || attempts != 3 {
					t.Fatalf("answer persistence did not use three bounded tries: %+v, %d", got, attempts)
				}
				d := mustSnapshot(t, e.store).Document
				want := ""
				if recoverWrite {
					want = "submitted"
				}
				if d.Attempts[0].Answer != want || d.outstanding() == nil {
					t.Fatalf("answer fallback discarded the guard: %+v", d)
				}
				reads := source.reads
				if got := e.step(context.Background()); !got.FileWait || source.reads != reads {
					t.Fatalf("retained guard allowed another hub read: %+v", got)
				}
			})
		}
	})

	t.Run("failed publication abort or binding loss preserves conservative guard", func(t *testing.T) {
		for _, mode := range []string{"publication abort succeeds", "publication abort fails", "target lost abort succeeds", "target lost abort fails", "binary target lost abort fails"} {
			t.Run(mode, func(t *testing.T) {
				e, r, _, now := newEngineTest(t)
				c, record, _ := commandSetup(t, binary, commandScenario{VersionOut: "codex-cli " + commandVersion + "\n", Out: commandAccept + "\n"})
				if !strings.HasPrefix(mode, "binary") {
					e.commands = c
				}
				syncs := 0
				e.store.io.syncDir = func(string) error {
					syncs++
					if syncs > 1 {
						return nil
					}
					if strings.HasSuffix(mode, "fails") {
						e.store.io.rename = func(string, string) error { return errors.New(commandSecret) }
					}
					if strings.Contains(mode, "target lost") {
						r.eligible = false
						return nil
					}
					return errors.New(commandSecret)
				}
				got := e.step(context.Background())
				want, outcome := engineDue(*now, time.Minute), "aborted"
				if strings.HasSuffix(mode, "fails") {
					outcome = ""
					want = stepResult{Due: now.Add(15 * time.Second), FileWait: true}
					if strings.HasPrefix(mode, "binary") {
						want, outcome = engineDue(*now, time.Minute), "failed"
					}
				} else if strings.Contains(mode, "target lost") {
					want = stepResult{}
				}
				if got != want || mustSnapshot(t, e.store).Document.Attempts[0].Answer != outcome || r.starts != 0 {
					t.Fatalf("unsafe publication/start fallback: %+v, want %+v and %q", got, want, outcome)
				}
				body, err := os.ReadFile(record)
				if strings.HasPrefix(mode, "binary") {
					if !os.IsNotExist(err) {
						t.Fatalf("binary failure launched a process: %v", err)
					}
				} else if err != nil || strings.Contains(string(body), `"queue"`) {
					t.Fatalf("failed admission started queue process: %v", err)
				}
			})
		}
	})
}
