package hook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/config"
	"github.com/mateusz-klatt/iris/internal/gitinfo"
	"github.com/mateusz-klatt/iris/internal/hubclient"
)

// The hooks' side of ADR 017: what a session is told is decided by the attention identity, under
// the sessions directory's lock, and a Codex wake is consumed by its own turn's prompt hook.

const testStream = "3d5f8f1e-8a1b-4c2d-9e0f-112233445566"

// hubHighWater is far past every cursor these tests name, as a busy hub's is. A test about a
// restore names a lower one itself.
const hubHighWater = 1 << 40

// pending is a hub's answer naming the identity at cursor, with the counts that go with it.
func pending(cursor int64, unread, available int) hubclient.Attention {
	return hubclient.Attention{Unread: unread, Available: available, HighWater: hubHighWater,
		Identity: hubclient.Identity{Known: true, Pending: true, Stream: testStream, Cursor: cursor}}
}

// sessionsFor is the memory a Runner with getenv keeps, beside the token.
func sessionsFor(getenv func(string) string) fileMemory {
	return fileMemory{dir: filepath.Join(filepath.Dir(getenv("IRIS_TOKEN_FILE")), "sessions"), now: time.Now, restrict: config.OwnerOnly}
}

func toldOf(m fileMemory, client Client, session string) toldState {
	return parseTold(m.read(m.counts(string(client), session)))
}

// A note is said when the identity is newer than the one told: a larger cursor or another
// stream. An equal or smaller one says nothing, and neither does nothing pending. A message
// handled and another arriving leave the count as it was and are said all the same.
func TestANoteIsSaidOnlyForANewerIdentity(t *testing.T) {
	root := gitRepo(t)
	env := failureEnv(t)
	now := &hubclient.Attention{}
	calls := 0
	hub := attentionHub{now: now, calls: &calls}
	prompt := func(at hubclient.Attention) string {
		t.Helper()
		*now = at
		event, payload := promptPayload(ClientClaude, root)
		out := Runner{ClientName: "claude", EventName: event, Stdin: strings.NewReader(payload), Hub: hub, Getenv: env, after: never, logAfter: never}.Run()
		if out.Exit != 0 {
			t.Fatalf("exit %d: %s", out.Exit, out.Stderr)
		}
		return string(out.Stdout)
	}
	for i, step := range []struct {
		at   hubclient.Attention
		want string
	}{
		{pending(100, 1, 0), "Iris: 1 unread message."},
		{pending(100, 1, 0), ""},
		{pending(90, 1, 0), ""},
		{pending(120, 1, 0), "Iris: 1 unread message."},
		{pending(120, 2, 1), ""},
		{hubclient.Attention{Unread: 1, Available: 2, HighWater: 9, Identity: hubclient.Identity{Known: true, Pending: true, Stream: "another stream", Cursor: 4}}, "Iris: 1 unread message; 2 paths you were refused are free."},
		{hubclient.Attention{HighWater: 130, Identity: hubclient.Identity{Known: true}}, ""},
	} {
		got := prompt(step.at)
		if step.want == "" && strings.Contains(got, "Iris:") || step.want != "" && !strings.Contains(got, step.want) {
			t.Fatalf("step %d: %s, want %q", i, got, step.want)
		}
	}
	if told := toldOf(sessionsFor(env), ClientClaude, "s"); told.Stream != "another stream" || told.Cursor != 4 {
		t.Fatalf("told %+v", told)
	}
}

// Two hooks of one session can ask the hub in one order and decide in the other. The told cursor
// never goes down within a stream, and an older answer that arrives late is not taken for a
// restore: only an answer that no other hook's telling preceded can show that the hub went back.
func TestTwoHooksRacingNeverLowerTheToldCursor(t *testing.T) {
	m := newFileMemory(t)
	r := Runner{Memory: m}
	ev := Event{Client: ClientClaude, Kind: KindPrompt, SessionID: "s"}
	if note := r.attentionNote(context.Background(), ev, pending(100, 1, 0), false, r.toldRevision(ev)); note == "" {
		t.Fatal("the first identity was not said")
	}
	slow := r.toldRevision(ev) // the slower hook reads this before asking the hub
	if note := r.attentionNote(context.Background(), ev, pending(200, 2, 0), false, r.toldRevision(ev)); note == "" {
		t.Fatal("the newer identity was not said")
	}
	older := hubclient.Attention{Unread: 1, HighWater: 160, Identity: hubclient.Identity{Known: true, Pending: true, Stream: testStream, Cursor: 150}}
	if note := r.attentionNote(context.Background(), ev, older, false, slow); note != "" {
		t.Fatalf("an older answer reopened what was said: %s", note)
	}
	if told := toldOf(m, ClientClaude, "s"); told.Cursor != 200 {
		t.Fatalf("the told cursor went down: %+v", told)
	}
	// Many hooks at once, each asking after it read the told state, as real ones do.
	var hub atomic.Int64
	hub.Store(1000)
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for range 10 {
				rev := r.toldRevision(ev)
				cursor := hub.Add(1)
				r.attentionNote(context.Background(), ev, hubclient.Attention{Unread: 1, HighWater: cursor, Identity: hubclient.Identity{Known: true, Pending: true, Stream: testStream, Cursor: cursor}}, false, rev)
			}
		})
	}
	wg.Wait()
	if told := toldOf(m, ClientClaude, "s"); told.Cursor != hub.Load() {
		t.Fatalf("told cursor %d after the newest %d", told.Cursor, hub.Load())
	}
}

// A touch answer can be the replay of an earlier call's, attention and all. It never counts as
// evidence that the hub went back, so a retried edit hook whose cached answer is older than what
// a later hook told cannot lower the told cursor or repeat the note.
func TestAReplayedTouchCannotPassForARestore(t *testing.T) {
	root := gitRepo(t)
	env := failureEnv(t)
	calls := 0
	cached := hubclient.Attention{Unread: 1, HighWater: 160, Identity: hubclient.Identity{Known: true, Pending: true, Stream: testStream, Cursor: 100}}
	hub := attentionTouchHub{attentionHub: attentionHub{now: ptr(pending(200, 2, 0)), calls: &calls}, touchAt: &cached}
	prompt := `{"session_id":"s1","cwd":` + quote(root) + `,"hook_event_name":"UserPromptSubmit","prompt":"go"}`
	if out := (Runner{ClientName: "codex", EventName: "UserPromptSubmit", Stdin: strings.NewReader(prompt), Hub: hub, Getenv: env, after: never, logAfter: never}).Run(); !strings.Contains(string(out.Stdout), "2 unread messages") {
		t.Fatalf("the newer identity was not said: %s", out.Stdout)
	}
	out := Runner{ClientName: "codex", EventName: "PreToolUse", Stdin: strings.NewReader(editPayload(ClientCodex, root)), Hub: hub, Getenv: env, after: never, logAfter: never}.Run()
	if strings.Contains(string(out.Stdout), "Iris:") {
		t.Fatalf("a replayed answer was taken for a restore: %s", out.Stdout)
	}
	if told := toldOf(sessionsFor(env), ClientCodex, "s1"); told.Cursor != 200 {
		t.Fatalf("the told cursor went down: %+v", told)
	}
}

// If the caller has stopped waiting, as a shell reconcile past its budget has, when the attention
// check runs under the lock, no note is decided and the told state is not written: recording an
// unseen note as told could suppress a later notification.
func TestANoteIsNotDecidedOnceTheCallerHasGivenUp(t *testing.T) {
	m := newFileMemory(t)
	r := Runner{Memory: m}
	ev := Event{Client: ClientCodex, Kind: KindPostTool, SessionID: "s"}
	done, cancel := context.WithCancel(context.Background())
	cancel()
	if note := r.attentionNote(done, ev, pending(100, 1, 0), false, r.toldRevision(ev)); note != "" {
		t.Fatalf("decided after the caller gave up: %s", note)
	}
	if _, err := os.Stat(m.counts("codex", "s")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("wrote after the caller gave up: %v", err)
	}
	if note := r.attentionNote(context.Background(), ev, pending(100, 1, 0), false, r.toldRevision(ev)); note == "" {
		t.Fatal("the note was lost")
	}
}

// A restore keeps the stream and takes the sequence back. A told cursor above the hub's
// high-water mark, in an answer no telling preceded, means the hub went back: what is pending is
// said once more, even an identity older than the one told, and only once.
func TestAToldCursorAboveTheHighWaterMarkIsSaidOnceMore(t *testing.T) {
	m := newFileMemory(t)
	r := Runner{Memory: m}
	ev := Event{Client: ClientCodex, Kind: KindPrompt, SessionID: "s"}
	r.attentionNote(context.Background(), ev, pending(200, 2, 0), false, r.toldRevision(ev))
	restored := hubclient.Attention{Unread: 1, HighWater: 160, Identity: hubclient.Identity{Known: true, Pending: true, Stream: testStream, Cursor: 150}}
	if note := r.attentionNote(context.Background(), ev, restored, false, r.toldRevision(ev)); !strings.Contains(note, "1 unread message") {
		t.Fatalf("a restored hub's attention was not said again: %q", note)
	}
	if note := r.attentionNote(context.Background(), ev, restored, false, r.toldRevision(ev)); note != "" {
		t.Fatalf("said a third time: %s", note)
	}
	// With nothing pending, a restore voids the told state, and the next attention is new.
	r.attentionNote(context.Background(), ev, pending(300, 1, 0), false, r.toldRevision(ev))
	r.attentionNote(context.Background(), ev, hubclient.Attention{HighWater: 40, Identity: hubclient.Identity{Known: true}}, false, r.toldRevision(ev))
	if note := r.attentionNote(context.Background(), ev, hubclient.Attention{Unread: 1, HighWater: 42, Identity: hubclient.Identity{Known: true, Pending: true, Stream: testStream, Cursor: 41}}, false, r.toldRevision(ev)); note == "" {
		t.Fatal("attention after a restore with nothing pending was not said")
	}
	// The SessionStart context states what is pending, so a restore found there is said by it.
	r.attentionNote(context.Background(), ev, pending(500, 1, 0), false, r.toldRevision(ev))
	r.attentionNote(context.Background(), ev, hubclient.Attention{Unread: 1, HighWater: 60, Identity: hubclient.Identity{Known: true, Pending: true, Stream: testStream, Cursor: 55}}, true, r.toldRevision(ev))
	if told := toldOf(m, ClientCodex, "s"); told.Cursor != 55 {
		t.Fatalf("the context's restore left %+v", told)
	}
}

// The same identity still pending ten minutes after it was told is said once more, by a prompt
// hook only, and then not again (decision 5). A newer identity starts its own ten minutes.
func TestTheSameIdentityIsSaidOnceMoreAfterTenMinutes(t *testing.T) {
	m := newFileMemory(t)
	clock := time.Unix(1_800_000_000, 0)
	r := Runner{Memory: m, Now: func() time.Time { return clock }}
	prompt := Event{Client: ClientClaude, Kind: KindPrompt, SessionID: "s"}
	edit := Event{Client: ClientClaude, Kind: KindPreTool, SessionID: "s"}
	note := func(ev Event, at hubclient.Attention) string {
		return r.attentionNote(context.Background(), ev, at, false, r.toldRevision(ev))
	}
	if note(prompt, pending(100, 1, 0)) == "" {
		t.Fatal("first note missing")
	}
	clock = clock.Add(secondNoteAfter - time.Second)
	if got := note(prompt, pending(100, 1, 0)); got != "" {
		t.Fatalf("said again too soon: %s", got)
	}
	clock = clock.Add(time.Second)
	if got := note(edit, pending(100, 1, 0)); got != "" {
		t.Fatalf("an edit hook said it a second time: %s", got)
	}
	if got := note(prompt, pending(100, 1, 0)); !strings.Contains(got, "1 unread message") {
		t.Fatalf("not said a second time: %q", got)
	}
	clock = clock.Add(3 * secondNoteAfter)
	if got := note(prompt, pending(100, 1, 0)); got != "" {
		t.Fatalf("said a third time: %s", got)
	}
	if note(prompt, pending(130, 1, 0)) == "" {
		t.Fatal("a newer identity was not said")
	}
	clock = clock.Add(secondNoteAfter)
	if note(prompt, pending(130, 1, 0)) == "" {
		t.Fatal("the newer identity was not said a second time")
	}
	// A resumed session's context restates what was told: that is the second telling, so the
	// next prompt does not repeat it at once.
	if note(prompt, pending(150, 1, 0)) == "" {
		t.Fatal("a newer identity was not said")
	}
	clock = clock.Add(2 * secondNoteAfter)
	r.attentionNote(context.Background(), prompt, pending(150, 1, 0), true, r.toldRevision(prompt))
	if got := note(prompt, pending(150, 1, 0)); got != "" {
		t.Fatalf("the context's telling was repeated: %s", got)
	}
}

// One pruning removes at most pruneLimit stale entries, so a month away does not hold the lock
// for every session's files at once; the next pruning goes on.
func TestOnePruningRemovesAtMostItsLimit(t *testing.T) {
	m := newFileMemory(t)
	if err := m.directory(); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-memoryAge - time.Hour)
	for i := range pruneLimit + 36 {
		path := filepath.Join(m.dir, "stale-"+strconv.Itoa(i))
		if err := os.WriteFile(path, []byte("1"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	left := func() int {
		n := 0
		for _, info := range mustList(t, m.dir) {
			if strings.HasPrefix(info.Name(), "stale-") {
				n++
			}
		}
		return n
	}
	if err := m.prune(); err != nil || left() != 36 {
		t.Fatalf("%d left after one pruning: %v", left(), err)
	}
	if err := m.prune(); err != nil || left() != 0 {
		t.Fatalf("%d left after two: %v", left(), err)
	}
}

// A hook reads the told state and the wake file under the lock too. On Windows the rename used
// here fails when another process holds the destination file open, so a read without the lock
// could keep a writer from marking an executed wake consumed (ADR 017). With the
// lock held, a prompt that is not the wake's waits and gives up, and the revision read before
// asking the hub is one no told state has, so that answer is no evidence of a restore.
func TestSessionFilesAreReadOnlyUnderTheLock(t *testing.T) {
	m := newFileMemory(t)
	writeWake(t, m, "codex", "s", wakeFile("wake", 0))
	if err := m.updateAttention("codex", "s", func(s *toldState) bool { s.Rev = 3; return true }); err != nil {
		t.Fatal(err)
	}
	if got, err := m.toldRevision("codex", "s"); got != 3 || err != nil {
		t.Fatalf("revision %d, want 3: %v", got, err)
	}
	release, err := m.lock(hookLockBudget)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if err := m.consumeWake("codex", "s", "not the wake"); !errors.Is(err, failSessionsLock) {
		t.Fatalf("read the wake file without the lock: %v", err)
	}
	if got, err := m.toldRevision("codex", "s"); got != fromReplay || !errors.Is(err, failSessionsLock) {
		t.Fatalf("read the told state without the lock: %d, %v", got, err)
	}
}

// A failed revision read is recorded even if the update after it succeeds once the lock is
// released. The hook cannot use that answer as evidence of a restore; doctor counts a skipped
// session-file operation.
func TestARevisionTheLockKeptFromBeingReadIsRecorded(t *testing.T) {
	env := failureEnv(t)
	m := sessionsFor(env)
	r := Runner{ClientName: "codex", Getenv: env, logAfter: never}
	ev := Event{Client: ClientCodex, Kind: KindPrompt, SessionID: "s"}
	r.attentionNote(context.Background(), ev, pending(200, 2, 0), false, r.toldRevision(ev))
	release, err := m.lock(hookLockBudget)
	if err != nil {
		t.Fatal(err)
	}
	rev := r.toldRevision(ev)
	release()
	restored := hubclient.Attention{Unread: 1, HighWater: 160, Identity: hubclient.Identity{Known: true, Pending: true, Stream: testStream, Cursor: 150}}
	if note := r.attentionNote(context.Background(), ev, restored, false, rev); note != "" || rev != fromReplay {
		t.Fatalf("an unread revision was taken as evidence: %q, revision %d", note, rev)
	}
	if got := waitForFailure(t, env("IRIS_TOKEN_FILE")); got.Stage != stageSessionFiles || got.Error != string(failSessionsLock) || !got.Skipped() {
		t.Fatalf("recorded %+v", got)
	}
}

// A wake that matched, but whose second lock, after the directory's protection, is held past the
// budget, stays outstanding, and the failure is reported.
func TestAWakeThatMatchedButFoundTheLockHeldStaysOutstanding(t *testing.T) {
	m := newFileMemory(t)
	writeWake(t, m, "codex", "s", wakeFile("wake", 0))
	entered, resume := make(chan struct{}), make(chan struct{})
	var once sync.Once
	paused := m
	paused.restrict = func(path string) error {
		if path == paused.dir {
			once.Do(func() {
				close(entered)
				<-resume
			})
		}
		return config.OwnerOnly(path)
	}
	done := make(chan error, 1)
	go func() { done <- paused.consumeWake("codex", "s", "wake") }()
	<-entered
	release, err := m.lock(hookLockBudget)
	if err != nil {
		t.Fatal(err)
	}
	close(resume)
	if err := <-done; !errors.Is(err, failSessionsLock) {
		t.Fatalf("the second lock: %v", err)
	}
	release()
	if newestAttempt(wakeOf(t, m, "s"))["consumed_ms"] != nil {
		t.Fatal("consumed without the lock")
	}
}

// Another hook can consume the wake between this hook's two reads, while this hook does not hold
// the lock. This hook then leaves the file as the other wrote it.
func TestAWakeConsumedMeanwhileIsNotConsumedAgain(t *testing.T) {
	m := newFileMemory(t)
	writeWake(t, m, "codex", "s", wakeFile("wake", 0))
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	paused := m
	paused.restrict = func(path string) error {
		if path == paused.dir {
			once.Do(func() {
				close(entered)
				<-release
			})
		}
		return config.OwnerOnly(path)
	}
	done := make(chan error, 1)
	go func() { done <- paused.consumeWake("codex", "s", "wake") }()
	<-entered
	if err := m.consumeWake("codex", "s", "wake"); err != nil {
		t.Fatal(err)
	}
	first := m.read(m.wakePath("codex", "s"))
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := m.read(m.wakePath("codex", "s")); got != first || newestAttempt(wakeOf(t, m, "s"))["consumed_ms"] == nil {
		t.Fatalf("consumed again: %s", got)
	}
}

// A file from before identities, and a hub from before them, behave as before: the counts
// decide. After the counts decided once, a hub that names attention is followed by identity.
func TestAnOldFileAndAnOlderHubBehaveAsBefore(t *testing.T) {
	m := newFileMemory(t)
	r := Runner{Memory: m}
	ev := Event{Client: ClientClaude, Kind: KindPrompt, SessionID: "s"}
	if err := m.write(m.counts("claude", "s"), "2 0 2 0"); err != nil {
		t.Fatal(err)
	}
	if note := r.attentionNote(context.Background(), ev, pending(100, 2, 0), false, r.toldRevision(ev)); note != "" {
		t.Fatalf("the old file's told counts were said again: %s", note)
	}
	if told := toldOf(m, ClientClaude, "s"); told.Stream != testStream || told.Cursor != 100 {
		t.Fatalf("the identity was not taken as told: %+v", told)
	}
	if note := r.attentionNote(context.Background(), ev, pending(130, 2, 0), false, r.toldRevision(ev)); !strings.Contains(note, "2 unread messages") {
		t.Fatalf("one handled and one new were not said: %q", note)
	}
	if note := r.attentionNote(context.Background(), ev, hubclient.Attention{Unread: 3}, false, r.toldRevision(ev)); !strings.Contains(note, "3 unread messages") {
		t.Fatalf("an older hub's counts: %q", note)
	}
	if told := toldOf(m, ClientClaude, "s"); told.Cursor != 130 || told.Told != (Counts{3, 0}) {
		t.Fatalf("an older hub moved the told identity: %+v", told)
	}
}

// SessionStart's context states what is pending, so the first prompt does not repeat it. A beating
// monitor says it instead, and what was told stays as it was until the monitor is gone.
func TestTheContextAndAMonitorCountAsInTheCounts(t *testing.T) {
	root := gitRepo(t)
	env := failureEnv(t)
	now := &hubclient.Attention{}
	calls := 0
	hub := attentionHub{now: now, calls: &calls}
	run := func(event, payload string, at hubclient.Attention) string {
		*now = at
		return string(Runner{ClientName: "claude", EventName: event, Stdin: strings.NewReader(payload), Hub: hub, Getenv: env, after: never, logAfter: never}.Run().Stdout)
	}
	start := `{"session_id":"s","cwd":` + quote(root) + `,"hook_event_name":"SessionStart"}`
	run("SessionStart", start, pending(100, 4, 0))
	event, payload := promptPayload(ClientClaude, root)
	if got := run(event, payload, pending(100, 4, 0)); strings.Contains(got, "Iris:") {
		t.Fatalf("SessionStart's attention repeated: %s", got)
	}
	monitored := pending(140, 5, 0)
	monitored.Monitored = true
	if got := run(event, payload, monitored); strings.Contains(got, "Iris:") {
		t.Fatalf("repeated what the monitor said: %s", got)
	}
	if got := run(event, payload, pending(140, 5, 0)); !strings.Contains(got, "5 unread messages") {
		t.Fatalf("what only the monitor saw was hidden for good: %s", got)
	}
}

// A lock held past the hook's budget leaves the session's told state unchanged: no note is said,
// the failure is recorded, and the call goes on (D3).
func TestALockHeldPastItsBudgetWritesNothing(t *testing.T) {
	root := gitRepo(t)
	env := failureEnv(t)
	m := sessionsFor(env)
	release, err := m.lock(hookLockBudget)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	calls := 0
	event, payload := promptPayload(ClientClaude, root)
	out := Runner{ClientName: "claude", EventName: event, Stdin: strings.NewReader(payload), Hub: attentionHub{now: ptr(pending(100, 1, 0)), calls: &calls}, Getenv: env, after: never, logAfter: never}.Run()
	if out.Exit != 0 || strings.Contains(string(out.Stdout), "Iris:") || calls != 1 {
		t.Fatalf("exit %d, %d calls: %s", out.Exit, calls, out.Stdout)
	}
	// Doctor grades it as a skipped operation, not a broken hook: the hook still allowed the turn.
	if got := waitForFailure(t, env("IRIS_TOKEN_FILE")); got.Stage != stageSessionFiles || got.Error != string(failSessionsLock) || !got.Skipped() || got.Degraded() {
		t.Fatalf("recorded %+v", got)
	}
	if _, err := os.Stat(m.counts("claude", "s")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the told state was written: %v", err)
	}
	if (Failure{Stage: "session", Error: string(failSessionsLock)}).Skipped() {
		t.Fatal("a failure at another stage was graded as skipped")
	}
}

func ptr[T any](v T) *T { return &v }

// If the told state cannot be written, no note is said, so nothing is said that was not
// remembered, and the failure is recorded.
func TestAToldStateThatCannotBeWrittenSaysNothing(t *testing.T) {
	root := gitRepo(t)
	env := failureEnv(t)
	m := sessionsFor(env)
	if err := os.MkdirAll(filepath.Join(m.counts("claude", "s"), "in the way"), 0o700); err != nil {
		t.Fatal(err)
	}
	calls := 0
	event, payload := promptPayload(ClientClaude, root)
	out := Runner{ClientName: "claude", EventName: event, Stdin: strings.NewReader(payload), Hub: attentionHub{now: ptr(pending(100, 1, 0)), calls: &calls}, Getenv: env, after: never, logAfter: never}.Run()
	if strings.Contains(string(out.Stdout), "Iris:") {
		t.Fatalf("said what it could not remember: %s", out.Stdout)
	}
	if got := waitForFailure(t, env("IRIS_TOKEN_FILE")); got.Stage != stageSessionFiles {
		t.Fatalf("recorded %+v", got)
	}
}

// attentionShellHub answers a shell reconcile with attention, as a hub that names it does.
type attentionShellHub struct {
	reconcileHub
	at      *hubclient.Attention
	message string
}

func (h attentionShellHub) Reconcile(context.Context, hubclient.ReconcileRequest) (hubclient.CheckResponse, error) {
	return hubclient.CheckResponse{Message: h.message, Attention: h.at}, nil
}

// After a command, a shell hook says new attention as an edit's hook does, for the clients that
// show its context: Claude Code and Codex. Before a command it says nothing, and neither does it
// after one on a hub that sends no attention with the reconcile.
func TestAShellHookSaysNewAttentionAfterTheCommand(t *testing.T) {
	shell := func(c Client, kind Kind, hub attentionShellHub, m fileMemory) string {
		r := Runner{Find: func(string) (gitinfo.Repo, error) { return gitinfo.Repo{Worktree: "/repo"}, nil }, Hub: hub, Status: func(context.Context, string, gitinfo.Scope, *gitinfo.Attempt) (map[string]string, error) {
			return map[string]string{}, nil
		}, ShellDeadline: time.Hour, Memory: m}
		out := r.reconcile(context.Background(), Event{Client: c, Kind: kind, ToolClass: ClassShell, SessionID: "shell", CWD: "/repo"}, &runState{})
		return string(out.Stdout)
	}
	for _, c := range Clients {
		m := newFileMemory(t)
		said := strings.Contains(shell(c, KindPostTool, attentionShellHub{at: ptr(pending(100, 1, 0))}, m), "1 unread message")
		if said != carriesToolContext(c) {
			t.Fatalf("%s: said %v", c, said)
		}
	}
	m := newFileMemory(t)
	if got := shell(ClientCodex, KindPreTool, attentionShellHub{at: ptr(pending(100, 1, 0))}, m); strings.Contains(got, "unread") {
		t.Fatalf("said before the command: %s", got)
	}
	if got := shell(ClientCodex, KindPostTool, attentionShellHub{message: "Shell edit detected."}, m); strings.Contains(got, "unread") {
		t.Fatalf("an older hub's answer was said: %s", got)
	}
	if _, err := os.Stat(m.counts("codex", "shell")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("an answer without attention wrote the told state: %v", err)
	}
	got := shell(ClientClaude, KindPostTool, attentionShellHub{at: ptr(pending(100, 2, 0)), message: "Shell edit detected."}, m)
	if !strings.Contains(got, "Shell edit detected.") || !strings.Contains(got, "2 unread messages") {
		t.Fatalf("the warning and the note: %s", got)
	}
}

// queued is the captured prompt hook of a queued wake's turn, and its prompt.
func queued(t *testing.T) (payload []byte, session, prompt string) {
	t.Helper()
	payload, err := os.ReadFile(filepath.Join("testdata", "codex", "UserPromptSubmit-queued.json"))
	if err != nil {
		t.Fatal(err)
	}
	var p struct {
		SessionID string `json:"session_id"`
		Prompt    string `json:"prompt"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		t.Fatal(err)
	}
	return payload, p.SessionID, p.Prompt
}

// wakeFile builds a file in the planned runner's format. The hook reads the attempts' message and
// consumed_ms, sets consumed_ms, and drops the token, message and answer of attempts consumed more
// than thirty days ago; every other field stays.
func wakeFile(message string, consumed int64) string {
	attempt := `{"slot":1,"identity":{"stream":"` + testStream + `","cursor":100},"token":"f5dfce6779181026","message":` + quote(message) + `,"written_ms":1794322984000,"answer":"submitted"`
	if consumed != 0 {
		attempt += `,"consumed_ms":` + strconv.FormatInt(consumed, 10)
	}
	return `{"version":1,"mark":{"stream":"` + testStream + `","cursor":100},"slots":[{"spent":true},{"spent":false}],"binary":{"path":"/usr/bin/codex"},"attempts":[` + attempt + `}]}`
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

// wakeOf decodes a session's wake file.
func wakeOf(t *testing.T, m fileMemory, session string) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal(must(os.ReadFile(m.wakePath("codex", session))), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func newestAttempt(v map[string]any) map[string]any {
	attempts := v["attempts"].([]any)
	return attempts[len(attempts)-1].(map[string]any)
}

// writeWake writes a wake file as the planned runner will, its directory first.
func writeWake(t *testing.T, m fileMemory, client, session, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(m.dir, wakeDir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := m.write(m.wakePath(client, session), body); err != nil {
		t.Fatal(err)
	}
}

// The captured prompt hook of a queued turn consumes its wake, before the hub is asked, so a hub
// that is down cannot leave the executed wake outstanding. Every other field in the file stays.
func TestAQueuedTurnConsumesItsWakeWithTheHubDown(t *testing.T) {
	payload, session, prompt := queued(t)
	env := failureEnv(t)
	m := sessionsFor(env)
	writeWake(t, m, "codex", session, wakeFile(prompt, 0))
	before := wakeOf(t, m, session)
	out := Runner{ClientName: "codex", EventName: "UserPromptSubmit", Stdin: bytes.NewReader(payload), Hub: scriptHub{regErr: context.DeadlineExceeded},
		Find: func(string) (gitinfo.Repo, error) { return gitinfo.Repo{Worktree: "/workspace/project"}, nil }, Getenv: env, after: never, logAfter: never}.Run()
	if out.Exit != 0 {
		t.Fatalf("exit %d", out.Exit)
	}
	after := wakeOf(t, m, session)
	if newestAttempt(after)["consumed_ms"] == nil {
		t.Fatalf("the wake was not consumed: %v", after)
	}
	delete(newestAttempt(after), "consumed_ms")
	if !jsonEqual(before, after) {
		t.Fatalf("the hook changed what the runner wrote:\n%v\n%v", before, after)
	}
}

func jsonEqual(a, b any) bool {
	return bytes.Equal(must(json.Marshal(a)), must(json.Marshal(b)))
}

// Only the whole message of the newest attempt consumes it: not the captured typed prompt, not one
// byte off, not the token alone, not one that contains the message, and not a second time.
func TestOnlyTheWakesOwnPromptConsumesIt(t *testing.T) {
	payload, session, prompt := queued(t)
	var fields map[string]any
	if err := json.Unmarshal(payload, &fields); err != nil {
		t.Fatal(err)
	}
	with := func(p string) []byte {
		fields["prompt"] = p
		return must(json.Marshal(fields))
	}
	typed, err := os.ReadFile(filepath.Join("testdata", "codex", "UserPromptSubmit.json"))
	if err != nil {
		t.Fatal(err)
	}
	var typedFields map[string]any
	if err := json.Unmarshal(typed, &typedFields); err != nil {
		t.Fatal(err)
	}
	typedFields["session_id"] = session
	for name, body := range map[string][]byte{
		"a typed prompt":    must(json.Marshal(typedFields)),
		"one byte off":      with(prompt[:len(prompt)-1] + "7"),
		"the token alone":   with("f5dfce6779181026"),
		"the message, more": with(prompt + " and more"),
	} {
		env := failureEnv(t)
		m := sessionsFor(env)
		writeWake(t, m, "codex", session, wakeFile(prompt, 0))
		Runner{ClientName: "codex", EventName: "UserPromptSubmit", Stdin: bytes.NewReader(body), Hub: scriptHub{regErr: context.DeadlineExceeded}, Getenv: env, after: never, logAfter: never}.Run()
		if newestAttempt(wakeOf(t, m, session))["consumed_ms"] != nil {
			t.Fatalf("%s consumed the wake", name)
		}
	}
	m := newFileMemory(t)
	writeWake(t, m, "codex", session, wakeFile(prompt, 5))
	if err := m.consumeWake("codex", session, prompt); err != nil || newestAttempt(wakeOf(t, m, session))["consumed_ms"] != float64(5) {
		t.Fatalf("a consumed wake was consumed again: %v", err)
	}
}

// A wake file the hook cannot read, or of another version, is left as it is. Without a wake file,
// the hook takes no lock.
func TestAWakeFileTheHookCannotReadIsLeftAlone(t *testing.T) {
	m := newFileMemory(t)
	release, err := m.lock(hookLockBudget)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.consumeWake("codex", "none", "anything"); err != nil {
		t.Fatalf("no wake file still took the lock: %v", err)
	}
	release()
	if err := os.MkdirAll(filepath.Join(m.wakePath("codex", "directory"), "inside"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := m.consumeWake("codex", "directory", "anything"); err != nil {
		t.Fatal(err)
	}
	if err := m.prune(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(m.wakePath("codex", "directory"), "inside")); err != nil {
		t.Fatalf("a directory in a wake file's place was touched: %v", err)
	}
	for name, body := range map[string]string{
		"not JSON":        "{",
		"another version": strings.Replace(wakeFile("wake", 0), `"version":1`, `"version":2`, 1),
		"no version":      `{"attempts":[]}`,
		"no attempts":     `{"version":1}`,
		"no attempt":      `{"version":1,"attempts":[]}`,
	} {
		writeWake(t, m, "codex", name, body)
		if err := m.consumeWake("codex", name, "wake"); err != nil {
			t.Fatal(err)
		}
		if got := m.read(m.wakePath("codex", name)); got != body {
			t.Fatalf("%s was changed to %s", name, got)
		}
	}
}

// Without a token file there is nowhere private for session files: a Codex prompt hook then
// consumes nothing, says nothing, and asks the hub as before.
func TestACodexPromptWithoutSessionFilesStillAsksTheHub(t *testing.T) {
	payload, _, _ := queued(t)
	calls := 0
	out := Runner{ClientName: "codex", EventName: "UserPromptSubmit", Stdin: bytes.NewReader(payload), Hub: attentionHub{now: ptr(pending(1, 1, 0)), calls: &calls},
		Find: func(string) (gitinfo.Repo, error) { return gitinfo.Repo{Worktree: "/workspace/project"}, nil }, after: never, logAfter: never}.Run()
	if out.Exit != 0 || calls != 1 || strings.Contains(string(out.Stdout), "Iris:") {
		t.Fatalf("exit %d, %d calls: %s", out.Exit, calls, out.Stdout)
	}
}

// A wake whose consumption the lock held off stays outstanding, and the failure is recorded.
func TestAWakeTheLockHeldOffStaysOutstanding(t *testing.T) {
	payload, session, prompt := queued(t)
	env := failureEnv(t)
	m := sessionsFor(env)
	writeWake(t, m, "codex", session, wakeFile(prompt, 0))
	release, err := m.lock(hookLockBudget)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	Runner{ClientName: "codex", EventName: "UserPromptSubmit", Stdin: bytes.NewReader(payload), Hub: scriptHub{regErr: context.DeadlineExceeded}, Getenv: env, after: never, logAfter: never}.Run()
	if newestAttempt(wakeOf(t, m, session))["consumed_ms"] != nil {
		t.Fatal("consumed without the lock")
	}
	entries, err := ReadRecentFailures(env("IRIS_TOKEN_FILE"), 10)
	if err != nil || len(entries) == 0 || entries[0].Stage != stageSessionFiles || entries[0].Error != string(failSessionsLock) {
		t.Fatalf("recorded %+v, %v", entries, err)
	}
}

// Pruning removes entries untouched for thirty days, under the lock, but never the lock, and it
// neither lists nor removes the wake files. A session resumed after 31 days, whose two wakes for
// an identity were consumed while its mail stayed unfetched, still has the mark and the spent
// slots that the planned runner will use to refuse a third (ADR 017, decision 3).
func TestPruningKeepsTheLockAndEveryWakeFile(t *testing.T) {
	now := time.Now()
	m := fileMemory{dir: filepath.Join(t.TempDir(), "sessions"), now: func() time.Time { return now }, restrict: config.OwnerOnly}
	old := now.Add(-memoryAge - 24*time.Hour)
	wakes := map[string]string{
		"resumed": strings.Replace(wakeFile("wake", old.UnixMilli()), `{"spent":false}`, `{"spent":true}`, 1),
		"damaged": "{",
	}
	for session, body := range wakes {
		writeWake(t, m, "codex", session, body)
	}
	files := map[string]string{
		m.name("codex", "stale"):       "3",
		m.counts("codex", "stale"):     "1 0 1 0",
		m.name("codex", "fresh"):       "4",
		filepath.Join(m.dir, ".tmp-1"): "left",
		filepath.Join(m.dir, ".lock"):  "",
	}
	for path, body := range files {
		if err := m.write(path, body); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{m.name("codex", "stale"), m.counts("codex", "stale"), filepath.Join(m.dir, ".tmp-1"), filepath.Join(m.dir, ".lock"),
		m.wakePath("codex", "resumed"), m.wakePath("codex", "damaged"), filepath.Join(m.dir, wakeDir)} {
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	held, err := m.lock(hookLockBudget)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.prune(); !errors.Is(err, failSessionsLock) {
		t.Fatalf("pruned without the lock: %v", err)
	}
	if _, err := os.Stat(m.name("codex", "stale")); err != nil {
		t.Fatal("a pruning that could not lock removed something")
	}
	held()
	if err := m.prune(); err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{m.name("codex", "stale"), m.counts("codex", "stale"), filepath.Join(m.dir, ".tmp-1")} {
		if _, err := os.Stat(gone); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s survived: %v", gone, err)
		}
	}
	for _, kept := range []string{m.name("codex", "fresh"), filepath.Join(m.dir, ".lock")} {
		if got := m.read(kept); got != files[kept] {
			t.Fatalf("%s became %q", kept, got)
		}
	}
	for session, body := range wakes {
		if got := m.read(m.wakePath("codex", session)); got != body {
			t.Fatalf("the wake file of %s became %q", session, got)
		}
	}
}

// When the hook writes a wake file, the token, message and answer of attempts consumed more than
// thirty days ago go from it. That is all that ever goes: the attempts, the mark and the slots
// stay.
func TestConsumingAWakeDropsOnlyLongConsumedDetails(t *testing.T) {
	now := time.Now()
	m := fileMemory{dir: filepath.Join(t.TempDir(), "sessions"), now: func() time.Time { return now }, restrict: config.OwnerOnly}
	day := 24 * time.Hour
	consumed := func(at time.Time) string { return strconv.FormatInt(at.UnixMilli(), 10) }
	body := `{"version":1,"mark":{"stream":"` + testStream + `","cursor":100},"slots":[{"spent":true},{"spent":true}],"attempts":[` +
		`{"slot":1,"token":"aa","message":"wake aa","answer":"submitted","consumed_ms":` + consumed(now.Add(-31*day)) + `},` +
		`{"slot":2,"token":"bb","message":"wake bb","answer":"stored","consumed_ms":` + consumed(now.Add(-29*day)) + `},` +
		`{"slot":1,"token":"cc","message":"wake cc","answer":"submitted"}]}`
	writeWake(t, m, "codex", "s", body)
	if err := m.consumeWake("codex", "s", "wake cc"); err != nil {
		t.Fatal(err)
	}
	v := wakeOf(t, m, "s")
	attempts := v["attempts"].([]any)
	first, second, third := attempts[0].(map[string]any), attempts[1].(map[string]any), attempts[2].(map[string]any)
	if first["token"] != nil || first["message"] != nil || first["answer"] != nil || first["slot"] != float64(1) || first["consumed_ms"] == nil {
		t.Fatalf("the long-consumed attempt: %v", first)
	}
	if second["token"] != "bb" || second["message"] != "wake bb" || second["answer"] != "stored" {
		t.Fatalf("a recent attempt lost its details: %v", second)
	}
	if third["consumed_ms"] != float64(now.UnixMilli()) || third["token"] != "cc" {
		t.Fatalf("the consumed attempt: %v", third)
	}
	var want map[string]any
	if err := json.Unmarshal([]byte(body), &want); err != nil {
		t.Fatal(err)
	}
	if !jsonEqual(v["mark"], want["mark"]) || !jsonEqual(v["slots"], want["slots"]) || v["version"] != float64(1) {
		t.Fatalf("the runner's fields changed: %v", v)
	}
}

// The lock is exclusive between processes and hooks alike, and a lock held past the budget is
// failSessionsLock. Where the directory or the lock file cannot be made, it says so.
func TestTheSessionsLock(t *testing.T) {
	m := newFileMemory(t)
	release, err := m.lock(hookLockBudget)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if _, err := m.lock(10 * time.Millisecond); !errors.Is(err, failSessionsLock) || time.Since(started) < 10*time.Millisecond {
		t.Fatalf("a held lock was taken, or given up before its budget: %v", err)
	}
	release()
	// An Iris from before ADR 017 prunes a file untouched for 30 days; every lock keeps .lock young.
	old := time.Now().Add(-memoryAge - time.Hour)
	if err := os.Chtimes(filepath.Join(m.dir, sessionsLock), old, old); err != nil {
		t.Fatal(err)
	}
	again, err := m.lock(hookLockBudget)
	if err != nil {
		t.Fatal("a released lock could not be taken", err)
	}
	again()
	if info, err := os.Stat(filepath.Join(m.dir, sessionsLock)); err != nil || !info.ModTime().After(old.Add(memoryAge)) {
		t.Fatalf("the lock did not keep its file young: %v", err)
	}

	blocked := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocked, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	under := fileMemory{dir: filepath.Join(blocked, "sessions"), now: time.Now, restrict: config.OwnerOnly}
	if _, err := under.lock(hookLockBudget); err == nil {
		t.Fatal("a directory under a file took the lock")
	}
	if err := under.forgetAttention("claude", "s"); err == nil {
		t.Fatal("a directory under a file forgot the told state")
	}
	// A directory that cannot be protected where it is made gives no lock then, and later keeps no
	// told state, since every write protects its file and its directory first.
	refused := errors.New("refused")
	unprotected := fileMemory{dir: filepath.Join(t.TempDir(), "sessions"), now: time.Now, restrict: func(string) error { return refused }}
	if _, err := unprotected.lock(hookLockBudget); !errors.Is(err, refused) {
		t.Fatalf("an unprotected directory gave the lock: %v", err)
	}
	if err := unprotected.updateAttention("claude", "s", func(*toldState) bool { return true }); !errors.Is(err, refused) {
		t.Fatalf("an unprotected directory took the told state: %v", err)
	}
	if _, err := os.Stat(unprotected.counts("claude", "s")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the told state was written unprotected: %v", err)
	}
	m = newFileMemory(t)
	if err := os.MkdirAll(filepath.Join(m.dir, sessionsLock), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := m.lock(hookLockBudget); err == nil {
		t.Fatal("a directory in the lock's place was locked")
	}
}

// Protecting the sessions directory can be slow: Windows applies its inheritable protection to
// every file in it, about 80 ms for a thousand sessions' files. Writes protect it before they take
// the lock, never under it. So while one hook is still protecting the directory, another takes
// the lock, and a wake's own prompt hook consumes the wake; the first then writes as well.
func TestProtectingTheDirectoryNeverHoldsTheLock(t *testing.T) {
	for name, write := range map[string]func(fileMemory) error{
		"the told state": func(m fileMemory) error {
			return m.updateAttention("claude", "a", func(*toldState) bool { return true })
		},
		"a wake": func(m fileMemory) error { return m.consumeWake("codex", "first", "wake") },
	} {
		m := newFileMemory(t)
		writeWake(t, m, "codex", "first", wakeFile("wake", 0))
		writeWake(t, m, "codex", "woken", wakeFile("wake", 0))
		entered, release := make(chan struct{}), make(chan struct{})
		var once sync.Once
		paused := m
		paused.restrict = func(path string) error {
			if path == paused.dir {
				once.Do(func() {
					close(entered)
					<-release
				})
			}
			return config.OwnerOnly(path)
		}
		done := make(chan error, 1)
		go func() { done <- write(paused) }()
		<-entered
		if err := m.consumeWake("codex", "woken", "wake"); err != nil {
			t.Fatalf("%s held the lock while it protected the directory: %v", name, err)
		}
		if newestAttempt(wakeOf(t, m, "woken"))["consumed_ms"] == nil {
			t.Fatalf("%s: the wake was not consumed", name)
		}
		close(release)
		if err := <-done; err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	m := newFileMemory(t)
	// A wake file whose directory cannot be protected is not consumed.
	writeWake(t, m, "codex", "unprotected", wakeFile("wake", 0))
	refused := m
	refused.restrict = func(string) error { return errors.New("refused") }
	if err := refused.consumeWake("codex", "unprotected", "wake"); err == nil {
		t.Fatal("consumed without protecting the directory")
	}
	if newestAttempt(wakeOf(t, m, "unprotected"))["consumed_ms"] != nil {
		t.Fatal("the unprotected wake was consumed")
	}
}

// The told state's line reads back as written, a file from before reads as counts only, and the
// wholly damaged line here reads as nothing told.
func TestTheToldStateReadsBackAsWritten(t *testing.T) {
	for _, s := range []toldState{
		{},
		{Latest: Counts{3, 1}, Told: Counts{2, 1}, Stream: testStream, Cursor: 120, At: 1_800_000_000_000, Twice: true, Rev: 7},
		{Latest: Counts{1, 0}, Rev: 2},
		{Stream: "a stream with spaces", Cursor: 3, Rev: 1},
		{Stream: "-", Cursor: 4, Rev: 1},
	} {
		if got := parseTold(s.line()); got != s {
			t.Fatalf("%+v read back as %+v", s, got)
		}
	}
	if got := parseTold("4 2 3 1"); got != (toldState{Latest: Counts{4, 2}, Told: Counts{3, 1}}) {
		t.Fatalf("a file from before: %+v", got)
	}
	if got := parseTold("x y z"); got != (toldState{}) {
		t.Fatalf("damage: %+v", got)
	}
}

// SessionEnd forgets what the session was told, under the lock, and keeps its wake file. When the
// lock is held, the told state stays and the failure is recorded.
func TestSessionEndForgetsTheToldStateAndKeepsTheWake(t *testing.T) {
	root := gitRepo(t)
	for _, held := range []bool{false, true} {
		env := failureEnv(t)
		m := sessionsFor(env)
		if err := m.updateAttention("claude", "s", func(s *toldState) bool { s.Stream, s.Cursor = testStream, 100; return true }); err != nil {
			t.Fatal(err)
		}
		writeWake(t, m, "claude", "s", wakeFile("wake", 1))
		if held {
			release, err := m.lock(hookLockBudget)
			if err != nil {
				t.Fatal(err)
			}
			defer release()
		}
		end := `{"session_id":"s","cwd":` + quote(root) + `,"hook_event_name":"SessionEnd"}`
		Runner{ClientName: "claude", EventName: "SessionEnd", Stdin: strings.NewReader(end), Hub: scriptHub{}, Getenv: env, after: never, logAfter: never}.Run()
		_, err := os.Stat(m.counts("claude", "s"))
		if held == errors.Is(err, os.ErrNotExist) {
			t.Fatalf("held %v: the told state %v", held, err)
		}
		if _, err := os.Stat(m.wakePath("claude", "s")); err != nil {
			t.Fatalf("the wake file went: %v", err)
		}
		if held {
			if got := waitForFailure(t, env("IRIS_TOKEN_FILE")); got.Error != string(failSessionsLock) {
				t.Fatalf("recorded %+v", got)
			}
		}
	}
}

// A SessionStart that finds the lock held still starts the session. Its revision read, its
// pruning and its told state are each recorded as not done.
func TestASessionStartWithTheLockHeldStillStarts(t *testing.T) {
	root := gitRepo(t)
	env := failureEnv(t)
	m := sessionsFor(env)
	release, err := m.lock(hookLockBudget)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	stale := m.name("codex", "long gone")
	if err := m.write(stale, "1"); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-memoryAge - time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	start := `{"session_id":"s","cwd":` + quote(root) + `,"hook_event_name":"SessionStart"}`
	calls := 0
	out := Runner{ClientName: "claude", EventName: "SessionStart", Stdin: strings.NewReader(start), Hub: attentionHub{now: ptr(pending(1, 1, 0)), calls: &calls}, Getenv: env, after: never, logAfter: never}.Run()
	if out.Exit != 0 || !strings.Contains(string(out.Stdout), "Iris handle") {
		t.Fatalf("exit %d: %s", out.Exit, out.Stdout)
	}
	entries, err := ReadRecentFailures(env("IRIS_TOKEN_FILE"), 10)
	if err != nil || len(entries) != 3 {
		t.Fatalf("the revision, the pruning and the told state were not all recorded: %+v %v", entries, err)
	}
	for _, e := range entries {
		if e.Stage != stageSessionFiles || e.Error != string(failSessionsLock) {
			t.Fatalf("recorded %+v", e)
		}
	}
	if _, err := os.Stat(stale); err != nil {
		t.Fatalf("pruned without the lock: %v", err)
	}
}

// Pruning lists the sessions directory before it takes the lock, which other hooks wait at most
// hookLockBudget for. This measures a pruning of a thousand sessions, two files each, none stale.
func BenchmarkPruningAThousandSessions(b *testing.B) {
	m := fileMemory{dir: filepath.Join(b.TempDir(), "sessions"), now: time.Now, restrict: config.OwnerOnly}
	for i := range 1000 {
		m.Remember("codex", strconv.Itoa(i), 1)
		if err := m.write(m.counts("codex", strconv.Itoa(i)), "1 0 1 0"); err != nil {
			b.Fatal(err)
		}
	}
	b.ResetTimer()
	for range b.N {
		if err := m.prune(); err != nil {
			b.Fatal(err)
		}
	}
}

// After a month away, the first SessionStart finds every session's files stale. This measures one
// pruning with two thousand stale files present, which holds the lock for its rechecks and
// removals, at most pruneLimit of them.
func BenchmarkPruningTwoThousandStaleFiles(b *testing.B) {
	m := fileMemory{dir: filepath.Join(b.TempDir(), "sessions"), now: time.Now, restrict: config.OwnerOnly}
	if err := m.directory(); err != nil {
		b.Fatal(err)
	}
	old := time.Now().Add(-memoryAge - time.Hour)
	b.ResetTimer()
	for range b.N {
		b.StopTimer()
		for i := range 2000 {
			path := filepath.Join(m.dir, "stale-"+strconv.Itoa(i))
			if err := os.WriteFile(path, []byte("1"), 0o600); err != nil {
				b.Fatal(err)
			}
			if err := os.Chtimes(path, old, old); err != nil {
				b.Fatal(err)
			}
		}
		b.StartTimer()
		if err := m.prune(); err != nil {
			b.Fatal(err)
		}
	}
}

// A write protects the sessions directory first, and Windows applies that protection to every file
// below it that inherits it. This measures a told-state write beside twenty thousand wake files,
// once in a wake directory that inherits and once in one with protection of its own, which stops
// the walk there. Elsewhere protection costs nothing, so only a Windows run says anything.
func BenchmarkAToldStateWriteBesideTwentyThousandWakeFiles(b *testing.B) {
	for name, own := range map[string]bool{"inherits": false, "own protection": true} {
		b.Run(name, func(b *testing.B) {
			m := fileMemory{dir: filepath.Join(b.TempDir(), "sessions"), now: time.Now, restrict: config.OwnerOnly}
			wake := filepath.Join(m.dir, wakeDir)
			if err := os.MkdirAll(wake, 0o700); err != nil {
				b.Fatal(err)
			}
			if err := m.directory(); err != nil {
				b.Fatal(err)
			}
			if own {
				if err := config.OwnerOnly(wake); err != nil {
					b.Fatal(err)
				}
			}
			for i := range 20000 {
				if err := os.WriteFile(filepath.Join(wake, strconv.Itoa(i)), []byte("{}"), 0o600); err != nil {
					b.Fatal(err)
				}
			}
			b.ResetTimer()
			for range b.N {
				if err := m.updateAttention("codex", "s", func(*toldState) bool { return true }); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// A hook rewrites a wake file whole, under the lock, when it consumes the wake. This measures the
// whole consumption of a wake in a file with a hundred earlier attempts.
func BenchmarkConsumingAWakeWithAHundredAttempts(b *testing.B) {
	m := fileMemory{dir: filepath.Join(b.TempDir(), "sessions"), now: time.Now, restrict: config.OwnerOnly}
	if err := os.MkdirAll(filepath.Join(m.dir, wakeDir), 0o700); err != nil {
		b.Fatal(err)
	}
	var attempts []string
	for i := range 100 {
		attempts = append(attempts, `{"slot":1,"identity":{"stream":"`+testStream+`","cursor":`+strconv.Itoa(i)+`},"written_ms":1,"consumed_ms":`+strconv.Itoa(i+1)+`}`)
	}
	attempts = append(attempts, `{"slot":1,"token":"cc","message":"wake cc","written_ms":1}`)
	body := `{"version":1,"attempts":[` + strings.Join(attempts, ",") + `]}`
	b.ResetTimer()
	for range b.N {
		b.StopTimer()
		if err := m.write(m.wakePath("codex", "s"), body); err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
		if err := m.consumeWake("codex", "s", "wake cc"); err != nil {
			b.Fatal(err)
		}
	}
}
