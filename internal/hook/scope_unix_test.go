//go:build !windows

package hook

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/gitinfo"
	"github.com/mateusz-klatt/iris/internal/hub"
	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/store"
)

const scopeHubURL = "http://127.0.0.1:7878"

// tripBudget is a reconcile budget short enough to pass while a stand-in for git waits.
const tripBudget = 20 * time.Millisecond

// scopeFixture is a worktree with an index file, a memory, a fixed clock and stand-ins for git
// that answer at once. A test changes only what it is about.
type scopeFixture struct {
	r     Runner
	ev    Event
	m     fileMemory
	repo  gitinfo.Repo
	hub   *scopeHub
	env   map[string]string
	clock atomic.Int64
}

func newScopeFixture(t *testing.T) *scopeFixture {
	t.Helper()
	root := t.TempDir()
	gitDir := filepath.Join(root, ".git")
	if err := os.Mkdir(gitDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "index"), []byte("DIRC"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := &scopeFixture{
		repo: gitinfo.Repo{Worktree: root, GitDir: gitDir, CommonDir: gitDir, WorktreeID: "1:2"},
		m:    fileMemory{dir: t.TempDir(), now: time.Now, restrict: func(string) error { return nil }},
		hub:  &scopeHub{generation: 2},
		env:  map[string]string{"IRIS_HUB_URL": scopeHubURL},
	}
	f.clock.Store(time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC).UnixMilli())
	f.r = Runner{
		Hub: f.hub, Memory: f.m,
		Find:     func(string) (gitinfo.Repo, error) { return f.repo, nil },
		Gitlinks: func(context.Context, string) ([]string, error) { return []string{"lib"}, nil },
		IndexID:  func(string) (gitinfo.IndexID, error) { return gitinfo.IndexID{Device: 1, Inode: 2, Size: 4}, nil },
		Now:      f.now,
		Getenv:   func(key string) string { return f.env[key] },
	}
	f.ev = Event{Client: ClientClaude, Kind: KindPreTool, SessionID: "s", CWD: root, ToolClass: ClassShell}
	return f
}

func (f *scopeFixture) now() time.Time           { return time.UnixMilli(f.clock.Load()).UTC() }
func (f *scopeFixture) at(t time.Time)           { f.clock.Store(t.UnixMilli()) }
func (f *scopeFixture) key() string              { return worktreeKey(f.repo) }
func (f *scopeFixture) breaker() (breaker, bool) { return f.m.Breaker(f.key()) }

// reduced gives the worktree the breaker a trip with this cooldown would have left now.
func (f *scopeFixture) reduced(cooldown time.Duration) {
	now := f.now()
	f.m.SetBreaker(f.key(), breaker{V: 1, Worktree: f.repo.Worktree, Scope: scopeReduced, Reason: "tripped", TrippedMS: now.UnixMilli(), NextFullMS: now.Add(cooldown).UnixMilli(), CooldownMS: cooldown.Milliseconds()})
}

// warm is a hook that has already heard this hub say it takes reduced snapshots.
func (f *scopeFixture) warm() { f.m.SetCapable(scopeHubURL, true) }

func (f *scopeFixture) run(git gitStandIn, budget time.Duration) (Output, *runState) {
	return f.runIn(context.Background(), git, budget)
}

func (f *scopeFixture) runIn(ctx context.Context, git gitStandIn, budget time.Duration) (Output, *runState) {
	r := f.r
	r.Status, r.ShellDeadline = git, budget
	state := &runState{}
	return r.reconcile(ctx, f.ev, state), state
}

// quickGit is a git status that finishes at once with the given entries.
func (f *scopeFixture) quickGit(entries map[string]string) gitStandIn {
	return func(ctx context.Context, _ string, scope gitinfo.Scope, a *gitinfo.Attempt) (map[string]string, error) {
		f.hub.note(statusEntry(scope))
		a.Begin(ctx, time.Now())
		return entries, nil
	}
}

// slowGit is a git status that started with left of the budget and is still running when the
// deadline passes.
func (f *scopeFixture) slowGit(left time.Duration) gitStandIn {
	return func(ctx context.Context, _ string, scope gitinfo.Scope, a *gitinfo.Attempt) (map[string]string, error) {
		f.hub.note(statusEntry(scope))
		deadline, _ := ctx.Deadline()
		a.Begin(ctx, deadline.Add(-left))
		<-ctx.Done()
		return nil, ctx.Err()
	}
}

// plainMemory keeps session proofs only, as a Memory without a scope memory does.
type plainMemory struct{}

func (plainMemory) Recall(string, string) int    { return 0 }
func (plainMemory) Remember(string, string, int) {}
func (plainMemory) Forget(string, string)        {}

// A trip needs git to be the slow part: a full status that started with at least half the
// budget left, had not passed the output cap, and was still running when the reconcile's own
// deadline passed, its parent not cancelled (ADR 018). Every run takes one status at most.
func TestABreakerTripsOnlyWhenGitWasTheSlowPart(t *testing.T) {
	waits := func(f *scopeFixture, begin, capped bool) gitStandIn {
		return func(ctx context.Context, _ string, scope gitinfo.Scope, a *gitinfo.Attempt) (map[string]string, error) {
			f.hub.note(statusEntry(scope))
			if begin {
				a.Begin(ctx, time.Now())
			}
			if capped {
				a.Cap()
			}
			<-ctx.Done()
			return nil, ctx.Err()
		}
	}
	cases := []struct {
		name    string
		git     func(*scopeFixture, context.CancelFunc) gitStandIn
		parent  time.Duration // a deadline of the reconcile's parent, if any
		budget  time.Duration
		slowHub bool
		plain   bool // the hook keeps no scope memory
		trips   bool
	}{
		{name: "git still running with the whole budget its own", git: func(f *scopeFixture, _ context.CancelFunc) gitStandIn { return f.slowGit(tripBudget) }, budget: tripBudget, trips: true},
		{name: "git started with half the budget left", git: func(f *scopeFixture, _ context.CancelFunc) gitStandIn { return f.slowGit(tripBudget / 2) }, budget: tripBudget, trips: true},
		{name: "git started with less than half left", git: func(f *scopeFixture, _ context.CancelFunc) gitStandIn {
			return f.slowGit(tripBudget/2 - time.Nanosecond)
		}, budget: tripBudget},
		{name: "git never started", git: func(f *scopeFixture, _ context.CancelFunc) gitStandIn { return waits(f, false, false) }, budget: tripBudget},
		{name: "git passed the output cap", git: func(f *scopeFixture, _ context.CancelFunc) gitStandIn { return waits(f, true, true) }, budget: tripBudget},
		{name: "git failed in time", git: func(f *scopeFixture, _ context.CancelFunc) gitStandIn {
			return func(ctx context.Context, _ string, scope gitinfo.Scope, a *gitinfo.Attempt) (map[string]string, error) {
				f.hub.note(statusEntry(scope))
				a.Begin(ctx, time.Now())
				return nil, errors.New("git failed")
			}
		}, budget: time.Hour},
		{name: "the hub was the slow part", git: func(f *scopeFixture, _ context.CancelFunc) gitStandIn { return f.quickGit(nil) }, budget: tripBudget, slowHub: true},
		{name: "the hook cancelled the reconcile", git: func(f *scopeFixture, cancel context.CancelFunc) gitStandIn {
			return func(ctx context.Context, _ string, scope gitinfo.Scope, a *gitinfo.Attempt) (map[string]string, error) {
				f.hub.note(statusEntry(scope))
				a.Begin(ctx, time.Now())
				cancel()
				<-ctx.Done()
				return nil, ctx.Err()
			}
		}, budget: tripBudget},
		{name: "a parent deadline ended the reconcile", git: func(f *scopeFixture, _ context.CancelFunc) gitStandIn { return f.slowGit(tripBudget) }, parent: tripBudget, budget: time.Hour},
		{name: "the hook keeps no scope memory", git: func(f *scopeFixture, _ context.CancelFunc) gitStandIn { return f.slowGit(tripBudget) }, budget: tripBudget, plain: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newScopeFixture(t)
			f.hub.waitOneCall = tc.slowHub
			if tc.plain {
				f.r.Memory = plainMemory{}
			}
			parent, cancel := context.WithCancel(context.Background())
			if tc.parent > 0 {
				parent, cancel = context.WithTimeout(context.Background(), tc.parent)
			}
			defer cancel()
			out, state := f.runIn(parent, tc.git(f, cancel), tc.budget)
			if !isAllow(out) {
				t.Fatalf("%s", out.Stdout)
			}
			if statuses := occurrences(f.hub.calls(), "status full"); statuses != 1 {
				t.Fatalf("%d statuses: %v", statuses, f.hub.calls())
			}
			b, tripped := f.breaker()
			if tripped != tc.trips {
				t.Fatalf("tripped %t: %+v", tripped, b)
			}
			if !tc.trips {
				return
			}
			now := f.now().UnixMilli()
			want := breaker{V: 1, Worktree: f.repo.Worktree, Scope: scopeReduced, Reason: "a full git status passed its 20ms budget", TrippedMS: now, NextFullMS: now + time.Minute.Milliseconds(), CooldownMS: time.Minute.Milliseconds()}
			if b != want {
				t.Fatalf("breaker %+v, want %+v", b, want)
			}
			if stage, err := state.snapshot(); stage != "shell status" || failureText(err) != shellOverBudget {
				t.Fatalf("stage %q: %v", stage, err)
			}
		})
	}
}

// Each due full attempt that times out doubles the cooldown, up to ten minutes. A full status
// that finishes returns the worktree to full and records when, and the next trip starts from a
// minute again (ADR 018 decision 4).
func TestCooldownsDoubleToTheCapAndAFullSuccessResets(t *testing.T) {
	f := newScopeFixture(t)
	for _, want := range []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 10 * time.Minute, 10 * time.Minute} {
		f.run(f.slowGit(tripBudget), tripBudget)
		b, ok := f.breaker()
		if !ok || b.Scope != scopeReduced || b.CooldownMS != want.Milliseconds() || b.NextFullMS != f.now().Add(want).UnixMilli() {
			t.Fatalf("after a trip, want %v: %+v", want, b)
		}
		// Within the cooldown the worktree runs reduced; at its end the next hook tries full.
		f.at(time.UnixMilli(b.NextFullMS - 1))
		if !f.r.reducedDue(f.repo) {
			t.Fatalf("reduced was not chosen within %v", want)
		}
		f.at(time.UnixMilli(b.NextFullMS))
		if f.r.reducedDue(f.repo) {
			t.Fatalf("reduced was still chosen when %v had passed", want)
		}
	}
	if calls := f.hub.calls(); occurrences(calls, "status full") != len(calls) {
		t.Fatalf("an attempt was not a full one: %v", calls)
	}
	succeeded := f.now()
	f.run(f.quickGit(nil), time.Hour)
	if b, ok := f.breaker(); !ok || b != (breaker{V: 1, Worktree: f.repo.Worktree, Scope: scopeFull, LastFullOKMS: succeeded.UnixMilli()}) {
		t.Fatalf("after a full success: %+v", b)
	}
	f.at(succeeded.Add(time.Hour))
	f.run(f.slowGit(tripBudget), tripBudget)
	if b, _ := f.breaker(); b.CooldownMS != time.Minute.Milliseconds() || b.LastFullOKMS != succeeded.UnixMilli() {
		t.Fatalf("the trip after a success: %+v", b)
	}
}

// A worktree runs reduced only while its breaker says so and its cooldown runs, and only when
// both reads can be of the index gitinfo found. Anything else runs full, as before.
func TestReducedScopeIsChosenOnlyWhenItCanBeVouchedFor(t *testing.T) {
	write := func(raw string) func(*scopeFixture) {
		return func(f *scopeFixture) {
			if err := os.WriteFile(f.m.breakerFile(f.key()), []byte(raw), 0o600); err != nil {
				panic(err)
			}
		}
	}
	cases := []struct {
		name  string
		setup func(*scopeFixture)
		want  string
	}{
		{"a reduced breaker within its cooldown", func(f *scopeFixture) { f.reduced(time.Minute) }, "status reduced"},
		{"a reduced breaker at the end of a cooldown of the cap", func(f *scopeFixture) { f.reduced(breakerCap) }, "status reduced"},
		{"the cooldown over", func(f *scopeFixture) { f.reduced(0) }, "status full"},
		{"a next attempt past the cap, from a clock that moved", func(f *scopeFixture) { f.reduced(breakerCap + time.Millisecond) }, "status full"},
		{"a full breaker", func(f *scopeFixture) { f.m.SetBreaker(f.key(), breaker{V: 1, Scope: scopeFull}) }, "status full"},
		{"no breaker", func(*scopeFixture) {}, "status full"},
		{"a file cut short", write(`{"v":1,"scope":"redu`), "status full"},
		{"another version's file", write(fmt.Sprintf(`{"v":2,"scope":"reduced","next_full_ms":%d}`, time.Date(2026, 11, 10, 12, 1, 0, 0, time.UTC).UnixMilli())), "status full"},
		{"an unknown scope", write(fmt.Sprintf(`{"v":1,"scope":"partial","next_full_ms":%d}`, time.Date(2026, 11, 10, 12, 1, 0, 0, time.UTC).UnixMilli())), "status full"},
		{"GIT_DIR set", func(f *scopeFixture) { f.reduced(time.Minute); f.env["GIT_DIR"] = "/elsewhere" }, "status full"},
		{"GIT_WORK_TREE set", func(f *scopeFixture) { f.reduced(time.Minute); f.env["GIT_WORK_TREE"] = "/elsewhere" }, "status full"},
		{"GIT_COMMON_DIR set", func(f *scopeFixture) { f.reduced(time.Minute); f.env["GIT_COMMON_DIR"] = "/elsewhere" }, "status full"},
		{"GIT_INDEX_FILE set", func(f *scopeFixture) { f.reduced(time.Minute); f.env["GIT_INDEX_FILE"] = "/elsewhere" }, "status full"},
		{"no index file", func(f *scopeFixture) {
			f.reduced(time.Minute)
			if err := os.Remove(filepath.Join(f.repo.GitDir, "index")); err != nil {
				panic(err)
			}
		}, "status full"},
		{"no git directory", func(f *scopeFixture) { f.reduced(time.Minute); f.repo.GitDir = "" }, "status full"},
		{"a hub without the scoped route", func(f *scopeFixture) { f.reduced(time.Minute); f.r.Hub = reconcileHub{} }, "status full"},
		{"a memory that keeps no breaker", func(f *scopeFixture) { f.reduced(time.Minute); f.r.Memory = plainMemory{} }, "status full"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newScopeFixture(t)
			f.warm()
			tc.setup(f)
			if out, _ := f.run(f.quickGit(map[string]string{"a.txt": "??"}), time.Hour); out.Exit != 0 {
				t.Fatalf("%s", out.Stdout)
			}
			calls := f.hub.calls()
			if occurrences(calls, tc.want) != 1 || len(f.hub.scopedOneCall) != occurrences(calls, "status reduced") {
				t.Fatalf("calls %v", calls)
			}
		})
	}
}

// A reduced snapshot is sent only when the index read before the gitlinks and after the status
// is one generation: the same device, inode, size, times and last 32 bytes. Anything else, or a
// read that fails, sends nothing and records why (ADR 018).
func TestAReducedSnapshotNeedsOneIndexGeneration(t *testing.T) {
	one := gitinfo.IndexID{Device: 1, Inode: 2, Size: 100, Modified: 3, Changed: 4, Tail: [32]byte{9}}
	replaced, rewritten, skipHash := one, one, one
	replaced.Inode = 5
	rewritten.Tail[0] = 8
	skipHash.Tail = [32]byte{}
	unreadable := errors.New("unreadable")
	cases := []struct {
		name          string
		first, second gitinfo.IndexID
		errs          [2]error
		linksErr      error
		sent          bool
	}{
		{name: "one generation", first: one, second: one, sent: true},
		{name: "one generation without a checksum, by its five fields", first: skipHash, second: skipHash, sent: true},
		{name: "replaced by another file", first: one, second: replaced},
		{name: "rewritten with the same five fields", first: one, second: rewritten},
		{name: "unreadable before", errs: [2]error{unreadable, nil}},
		{name: "unreadable after", first: one, errs: [2]error{nil, unreadable}},
		{name: "gitlinks that cannot be read whole", first: one, second: one, linksErr: unreadable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newScopeFixture(t)
			f.reduced(time.Minute)
			f.warm()
			var reads atomic.Int32
			f.r.IndexID = func(path string) (gitinfo.IndexID, error) {
				if path != filepath.Join(f.repo.GitDir, "index") {
					t.Errorf("read %s", path)
				}
				if reads.Add(1) == 1 {
					return tc.first, tc.errs[0]
				}
				return tc.second, tc.errs[1]
			}
			if tc.linksErr != nil {
				f.r.Gitlinks = func(context.Context, string) ([]string, error) { return nil, tc.linksErr }
			}
			out, state := f.run(f.quickGit(map[string]string{"a.txt": "??"}), time.Hour)
			if !isAllow(out) {
				t.Fatalf("%s", out.Stdout)
			}
			stage, err := state.snapshot()
			if !tc.sent {
				if len(f.hub.scopedOneCall) != 0 || stage != "shell scope" || failureText(err) != shellScopeUnknown || !(Failure{Stage: stage, Error: failureText(err)}).Degraded() {
					t.Fatalf("sent %d, stage %q: %v", len(f.hub.scopedOneCall), stage, err)
				}
				return
			}
			if len(f.hub.scopedOneCall) != 1 || err != nil {
				t.Fatalf("sent %d: %v", len(f.hub.scopedOneCall), err)
			}
			got := f.hub.scopedOneCall[0]
			if got.Kind != "before" || got.Status["a.txt"] != "??" || got.Scope.Mode != "dirty" || !slices.Equal(got.Scope.Submodules, []string{"lib"}) {
				t.Fatalf("sent %+v", got)
			}
		})
	}
}

// A reduced status past its budget sends nothing and leaves the breaker as it was: only a full
// status is evidence for a trip (ADR 018).
func TestAReducedStatusPastItsBudgetChangesNothing(t *testing.T) {
	f := newScopeFixture(t)
	f.reduced(time.Minute)
	f.warm()
	before, err := os.ReadFile(f.m.breakerFile(f.key()))
	if err != nil {
		t.Fatal(err)
	}
	out, state := f.run(f.slowGit(tripBudget), tripBudget)
	after, err := os.ReadFile(f.m.breakerFile(f.key()))
	if err != nil || string(after) != string(before) {
		t.Fatalf("the breaker changed: %s, then %s: %v", before, after, err)
	}
	if calls := f.hub.calls(); !isAllow(out) || !slices.Equal(calls, []string{"status reduced"}) {
		t.Fatalf("calls %v: %s", calls, out.Stdout)
	}
	if stage, err := state.snapshot(); stage != "shell status" || failureText(err) != shellOverBudget {
		t.Fatalf("stage %q: %v", stage, err)
	}
}

// With the breaker reduced and nothing known of the hub, the hook registers first and lets the
// answer decide: a hub that takes reduced snapshots gets one, an older hub a full one, and a
// scoped request never goes to a hub that has not said it takes one (ADR 018).
func TestAColdCacheRegistersBeforeChoosingTheScope(t *testing.T) {
	f := newScopeFixture(t)
	f.reduced(time.Minute)
	f.run(f.quickGit(map[string]string{"a.txt": "??"}), time.Hour)
	if calls := f.hub.calls(); !slices.Equal(calls, []string{"register", "status reduced", "scoped"}) {
		t.Fatalf("calls %v", calls)
	}
	if got := f.hub.scoped[0]; got.ExecutionID != "ex" || got.Generation != 2 || got.Kind != "before" || got.Scope.Mode != "dirty" || !f.m.Capable(scopeHubURL) {
		t.Fatalf("sent %+v, capable %t", got, f.m.Capable(scopeHubURL))
	}

	older := newScopeFixture(t)
	older.reduced(time.Minute)
	older.hub.incapable = true
	older.run(older.quickGit(map[string]string{"a.txt": "??"}), time.Hour)
	if calls := older.hub.calls(); !slices.Equal(calls, []string{"register", "status full", "reconcile"}) || older.m.Capable(scopeHubURL) {
		t.Fatalf("calls %v", calls)
	}
	// That full status finished, so the worktree runs full again.
	if b, _ := older.breaker(); b.Scope != scopeFull || b.LastFullOKMS != older.now().UnixMilli() {
		t.Fatalf("breaker %+v", b)
	}
	// And one past its budget trips as any full status does, doubling the cooldown.
	slowOlder := newScopeFixture(t)
	slowOlder.reduced(time.Minute)
	slowOlder.hub.incapable = true
	out, state := slowOlder.run(slowOlder.slowGit(tripBudget), tripBudget)
	if calls := slowOlder.hub.calls(); !isAllow(out) || !slices.Equal(calls, []string{"register", "status full"}) {
		t.Fatalf("calls %v", calls)
	}
	if b, _ := slowOlder.breaker(); b.Scope != scopeReduced || b.CooldownMS != (2*time.Minute).Milliseconds() {
		t.Fatalf("breaker %+v", b)
	}
	if stage, err := state.snapshot(); stage != "shell status" || failureText(err) != shellOverBudget {
		t.Fatalf("stage %q: %v", stage, err)
	}
	// An older hub refusing the request in time is only learned at the next registration.
	failing := newScopeFixture(t)
	failing.reduced(time.Minute)
	failing.hub.scopedErrs = []error{&hubclient.Error{Code: "not_found"}}
	failing.run(failing.quickGit(nil), time.Hour)
	if calls := failing.hub.calls(); !slices.Equal(calls, []string{"register", "status reduced", "scoped"}) || failing.m.Capable(scopeHubURL) {
		t.Fatalf("calls %v, capable %t", calls, failing.m.Capable(scopeHubURL))
	}

	refused := newScopeFixture(t)
	refused.reduced(time.Minute)
	refused.hub.registerErrs = []error{errors.New("connection refused")}
	out, state = refused.run(refused.quickGit(nil), time.Hour)
	if calls := refused.hub.calls(); !isAllow(out) || !slices.Equal(calls, []string{"register"}) {
		t.Fatalf("calls %v", calls)
	}
	if stage, err := state.snapshot(); stage != "shell session" || err == nil {
		t.Fatalf("stage %q: %v", stage, err)
	}
}

// A scoped request that fails is never sent again on a plain route, which would read the reduced
// map as a full one. A refusal like an older hub's forgets the capability; any other failure
// keeps it (ADR 018).
func TestAFailedScopedRequestIsNeverResentAsFull(t *testing.T) {
	cases := []struct {
		name    string
		err     error
		wait    bool
		forgets bool
	}{
		{name: "not_found", err: &hubclient.Error{Code: "not_found"}, forgets: true},
		{name: "a bare 404", err: &hubclient.StatusError{Path: "/v1/shell/scoped", Code: 404, Status: "404 Not Found"}, forgets: true},
		{name: "bad_request", err: &hubclient.Error{Code: "bad_request"}, forgets: true},
		{name: "another failure", err: errors.New("connection refused")},
		{name: "past the budget", wait: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newScopeFixture(t)
			f.reduced(time.Minute)
			f.warm()
			f.hub.scopedErrs, f.hub.waitScoped = []error{tc.err}, tc.wait
			budget := time.Hour
			if tc.wait {
				budget = tripBudget
			}
			out, state := f.run(f.quickGit(map[string]string{"a.txt": "??"}), budget)
			if calls := f.hub.calls(); !isAllow(out) || !slices.Equal(calls, []string{"status reduced", "scoped one-call"}) {
				t.Fatalf("calls %v: %s", calls, out.Stdout)
			}
			if f.m.Capable(scopeHubURL) == tc.forgets {
				t.Fatalf("capable %t", f.m.Capable(scopeHubURL))
			}
			if stage, err := state.snapshot(); stage != "shell session" || err == nil {
				t.Fatalf("stage %q: %v", stage, err)
			}
		})
	}
}

// A scoped request answered lost repairs the session's proof as a registration does and drops
// that snapshot: nothing goes on a plain route, and the next hook's snapshot carries the new
// proof. A repair that is lost too refuses the command, or warns after it (ADR 018).
func TestALostScopedRequestRepairsTheProof(t *testing.T) {
	lost := &hubclient.Error{Code: "lost"}
	f := newScopeFixture(t)
	f.reduced(time.Minute)
	f.warm()
	f.m.Remember(string(f.ev.Client), f.ev.SessionID, 3)
	f.hub.generation = 4
	f.hub.scopedErrs = []error{lost}
	f.hub.registerErrs = []error{lost}
	if out, _ := f.run(f.quickGit(nil), time.Hour); !isAllow(out) {
		t.Fatalf("%s", out.Stdout)
	}
	if calls := f.hub.calls(); !slices.Equal(calls, []string{"status reduced", "scoped one-call", "register", "register"}) {
		t.Fatalf("calls %v", calls)
	}
	if sent, retry := f.hub.registers[1], f.hub.scopedOneCall[0]; retry.Generation != 3 || sent.Generation != 0 || sent.RetryGeneration != 3 {
		t.Fatalf("proofs %d, then %d retrying %d", retry.Generation, sent.Generation, sent.RetryGeneration)
	}
	f.run(f.quickGit(nil), time.Hour)
	if got := f.hub.scopedOneCall[1]; got.Generation != 4 || !f.m.Capable(scopeHubURL) {
		t.Fatalf("the next snapshot's proof %d", got.Generation)
	}

	for _, kind := range []Kind{KindPreTool, KindPostTool} {
		g := newScopeFixture(t)
		g.reduced(time.Minute)
		g.warm()
		g.m.Remember(string(g.ev.Client), g.ev.SessionID, 3)
		g.hub.scopedErrs = []error{lost}
		g.hub.registerErrs = []error{lost, lost}
		g.ev.Kind = kind
		out, _ := g.run(g.quickGit(nil), time.Hour)
		if kind == KindPreTool && !strings.Contains(string(out.Stdout), "refused rather than run unchecked") {
			t.Fatalf("before: %s", out.Stdout)
		}
		if kind == KindPostTool && !strings.Contains(string(out.Stdout), "were not checked or claimed") {
			t.Fatalf("after: %s", out.Stdout)
		}
		if calls := g.hub.calls(); occurrences(calls, "reconcile")+occurrences(calls, "one-call") != 0 {
			t.Fatalf("calls %v", calls)
		}
	}
}

// The scope is chosen once, before the status runs. A breaker another hook replaces meanwhile
// changes the next hook's choice, not this request (ADR 018).
func TestABreakerReplacedDuringTheStatusDoesNotChangeTheRequest(t *testing.T) {
	f := newScopeFixture(t)
	f.reduced(time.Minute)
	f.warm()
	f.run(func(ctx context.Context, root string, scope gitinfo.Scope, a *gitinfo.Attempt) (map[string]string, error) {
		f.m.SetBreaker(f.key(), breaker{V: 1, Worktree: f.repo.Worktree, Scope: scopeFull})
		return f.quickGit(map[string]string{"a.txt": "??"})(ctx, root, scope, a)
	}, time.Hour)
	if len(f.hub.scopedOneCall) != 1 || len(f.hub.oneCall) != 0 || f.hub.scopedOneCall[0].Scope.Mode != "dirty" {
		t.Fatalf("calls %v", f.hub.calls())
	}

	g := newScopeFixture(t)
	g.warm()
	g.run(func(ctx context.Context, root string, scope gitinfo.Scope, a *gitinfo.Attempt) (map[string]string, error) {
		g.reduced(time.Minute)
		return g.quickGit(map[string]string{"a.txt": "??"})(ctx, root, scope, a)
	}, time.Hour)
	if len(g.hub.oneCall) != 1 || len(g.hub.scopedOneCall) != 0 {
		t.Fatalf("calls %v", g.hub.calls())
	}
}

// barrierMemory holds a hook at its second read of the breaker, which is the trip's, until the
// test lets it go, so two hooks can be made to read before either writes.
type barrierMemory struct {
	fileMemory
	reads   atomic.Int32
	arrived chan<- struct{}
	proceed <-chan struct{}
}

func (b *barrierMemory) Breaker(worktree string) (breaker, bool) {
	out, ok := b.fileMemory.Breaker(worktree)
	if b.reads.Add(1) == 2 {
		b.arrived <- struct{}{}
		<-b.proceed
	}
	return out, ok
}

// Hooks that run at once read and replace the breaker in no particular order (ADR 018). Two
// that both read it before either writes make one doubling between them, and whichever writes
// last decides. Either way the file is whole, the next hook follows it, and each snapshot
// states the scope it ran with.
func TestConcurrentHooksLeaveABreakerTheNextHookFollows(t *testing.T) {
	t.Run("two trips from one reading", func(t *testing.T) {
		f := newScopeFixture(t)
		now := f.now().UnixMilli()
		f.m.SetBreaker(f.key(), breaker{V: 1, Worktree: f.repo.Worktree, Scope: scopeReduced, NextFullMS: now, CooldownMS: time.Minute.Milliseconds()})
		arrived, proceed := make(chan struct{}), make(chan struct{})
		var wg sync.WaitGroup
		for range 2 {
			r := f.r
			r.Memory = &barrierMemory{fileMemory: f.m, arrived: arrived, proceed: proceed}
			r.Status, r.ShellDeadline = f.slowGit(tripBudget), tripBudget
			wg.Go(func() { r.reconcile(context.Background(), f.ev, &runState{}) })
		}
		<-arrived
		<-arrived
		close(proceed)
		wg.Wait()
		if b, _ := f.breaker(); b.Scope != scopeReduced || b.CooldownMS != (2*time.Minute).Milliseconds() || !f.r.reducedDue(f.repo) {
			t.Fatalf("breaker %+v", b)
		}
	})

	t.Run("a full success written after a trip", func(t *testing.T) {
		f := newScopeFixture(t)
		started, release := make(chan struct{}), make(chan struct{})
		late := f.r
		late.ShellDeadline = time.Hour
		late.Status = func(ctx context.Context, _ string, scope gitinfo.Scope, a *gitinfo.Attempt) (map[string]string, error) {
			f.hub.note(statusEntry(scope))
			a.Begin(ctx, time.Now())
			close(started)
			<-release
			return map[string]string{"a.txt": "??"}, nil
		}
		var wg sync.WaitGroup
		wg.Go(func() { late.reconcile(context.Background(), f.ev, &runState{}) })
		<-started
		f.run(f.slowGit(tripBudget), tripBudget)
		if b, _ := f.breaker(); b.Scope != scopeReduced {
			t.Fatalf("no trip: %+v", b)
		}
		close(release)
		wg.Wait()
		if b, _ := f.breaker(); b.Scope != scopeFull || b.LastFullOKMS != f.now().UnixMilli() || f.r.reducedDue(f.repo) {
			t.Fatalf("breaker %+v", b)
		}
		if len(f.hub.oneCall) != 1 || len(f.hub.scopedOneCall)+len(f.hub.scoped) != 0 {
			t.Fatalf("calls %v", f.hub.calls())
		}
	})

	t.Run("a reduced snapshot sent after a full success", func(t *testing.T) {
		f := newScopeFixture(t)
		f.reduced(time.Minute)
		f.warm()
		started, release := make(chan struct{}), make(chan struct{})
		slow := f.r
		slow.ShellDeadline = time.Hour
		slow.Status = func(ctx context.Context, _ string, scope gitinfo.Scope, a *gitinfo.Attempt) (map[string]string, error) {
			f.hub.note(statusEntry(scope))
			close(started)
			<-release
			return map[string]string{"a.txt": "??"}, nil
		}
		var wg sync.WaitGroup
		wg.Go(func() { slow.reconcile(context.Background(), f.ev, &runState{}) })
		<-started
		// A hook that starts once the cooldown is over tries full, and it finishes.
		due := f.r
		due.Now = func() time.Time { return f.now().Add(time.Minute) }
		due.Status, due.ShellDeadline = f.quickGit(map[string]string{"a.txt": "??"}), time.Hour
		due.reconcile(context.Background(), f.ev, &runState{})
		close(release)
		wg.Wait()
		if b, _ := f.breaker(); b.Scope != scopeFull || f.r.reducedDue(f.repo) {
			t.Fatalf("breaker %+v", b)
		}
		if len(f.hub.oneCall) != 1 || len(f.hub.scopedOneCall) != 1 || f.hub.scopedOneCall[0].Scope.Mode != "dirty" {
			t.Fatalf("calls %v", f.hub.calls())
		}
	})
}

// Every client's output stays valid on the reduced path: a warning the hub gives reaches the
// model, and a snapshot not taken, or a trip, fails open (ADR 018, D3).
func TestEveryClientsOutputIsValidInReducedScope(t *testing.T) {
	for _, client := range Clients {
		t.Run(string(client), func(t *testing.T) {
			f := newScopeFixture(t)
			f.reduced(time.Minute)
			f.warm()
			f.ev.Client, f.ev.Kind = client, KindPostTool
			f.hub.message = "Shell edit detected. x.go is held. Iris detects these changes and cannot undo them."
			if out, _ := f.run(f.quickGit(map[string]string{"x.go": " M"}), time.Hour); out.Exit != 0 || !strings.Contains(string(out.Stdout), "cannot undo") {
				t.Fatalf("warning: %#v", out)
			}
			allow := Encode(f.ev, Decision{Action: ActionAllow})
			f.r.IndexID = func(string) (gitinfo.IndexID, error) { return gitinfo.IndexID{}, errors.New("unreadable") }
			if out, _ := f.run(f.quickGit(nil), time.Hour); string(out.Stdout) != string(allow.Stdout) || out.Exit != allow.Exit {
				t.Fatalf("no snapshot: %#v", out)
			}
			f.reduced(0)
			if out, _ := f.run(f.slowGit(tripBudget), tripBudget); string(out.Stdout) != string(allow.Stdout) || out.Exit != allow.Exit {
				t.Fatalf("a trip: %#v", out)
			}
			if b, _ := f.breaker(); b.Scope != scopeReduced || b.CooldownMS != time.Minute.Milliseconds() {
				t.Fatalf("breaker %+v", b)
			}
		})
	}
}

// What a hub said about reduced snapshots is kept for that hub only, and an answer that does
// not name the capability clears it again (ADR 018).
func TestTheCapabilityIsKeptPerHub(t *testing.T) {
	f := newScopeFixture(t)
	const other = "https://iris.example"
	f.m.SetCapable(other, true)
	if f.m.Capable(scopeHubURL) || !f.m.Capable(other) {
		t.Fatal("one hub's capability was read as another's")
	}
	if _, err := f.r.register(context.Background(), f.ev, f.repo); err != nil || !f.m.Capable(scopeHubURL) {
		t.Fatalf("a registration answer was not kept: %v", err)
	}
	f.hub.incapable = true
	if _, err := f.r.register(context.Background(), f.ev, f.repo); err != nil || f.m.Capable(scopeHubURL) || !f.m.Capable(other) {
		t.Fatalf("an answer without the capability: %v", err)
	}
	// A one-call answer is kept as a registration's is.
	f.hub.incapable = false
	f.run(f.quickGit(nil), time.Hour)
	if !f.m.Capable(scopeHubURL) {
		t.Fatal("a one-call answer was not kept")
	}
}

// A worktree has one breaker whichever path reaches it, because it is named by device and
// inode. Where those could not be read, its path names it.
func TestABreakerBelongsToTheWorktreeNotThePath(t *testing.T) {
	f := newScopeFixture(t)
	f.run(f.slowGit(tripBudget), tripBudget)
	through := f.repo
	through.Worktree = filepath.Join(t.TempDir(), "link")
	if !f.r.reducedDue(through) {
		t.Fatal("the worktree reached through another path has no breaker")
	}
	g := newScopeFixture(t)
	g.repo.WorktreeID = ""
	g.run(g.slowGit(tripBudget), tripBudget)
	if b, ok := g.m.Breaker(g.repo.Worktree); !ok || b.Scope != scopeReduced {
		t.Fatalf("a worktree without an identity: %+v", b)
	}
}

// stallMemory never finishes writing a breaker.
type stallMemory struct {
	fileMemory
	entered chan<- time.Time
	release <-chan struct{}
}

func (s stallMemory) SetBreaker(string, breaker) {
	s.entered <- time.Now()
	<-s.release
}

// A breaker write that stalls cannot hold the hook past its own deadline: the output is still
// valid, and only the trip is lost (ADR 018, D3).
func TestAStalledBreakerWriteStillReturnsValidOutput(t *testing.T) {
	f := newScopeFixture(t)
	entered, release := make(chan time.Time), make(chan struct{})
	defer close(release)
	r := f.r
	r.Memory = stallMemory{fileMemory: f.m, entered: entered, release: release}
	r.ClientName, r.EventName = "claude", "PreToolUse"
	r.Stdin = strings.NewReader(fmt.Sprintf(`{"session_id":"s","cwd":%q,"tool_name":"Bash","tool_input":{"command":"make"}}`, f.repo.Worktree))
	r.Status, r.ShellDeadline = f.slowGit(tripBudget), tripBudget
	// The hook's own deadline fires once the write has stalled.
	r.after = func(time.Duration) <-chan time.Time { return entered }
	r.logAfter = never
	assertAllow(t, ClientClaude, r.Run())
	if _, found := f.breaker(); found {
		t.Fatal("a stalled write was kept")
	}
}

// hookGit runs git with no user or system configuration.
func hookGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "protocol.file.allow=always"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// A reduced reconcile against a real hub and real git. The snapshot goes on the scoped route with
// the submodule set; a tracked edit inside the submodule is left out, as reduced scope says, and
// an edit beside it is still seen. A proof repaired after a lost answer keeps the Execution, so
// the next snapshot compares with the reduced baseline it already had (ADR 018).
func TestAReducedReconcileAgainstTheRealHub(t *testing.T) {
	const remote = "https://example.com/team/scope.git"
	base := t.TempDir()
	sub, super := filepath.Join(base, "sub"), filepath.Join(base, "super")
	for _, dir := range []string{sub, super} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		hookGit(t, dir, "init", "-q")
	}
	for path, body := range map[string]string{filepath.Join(sub, "inner.txt"): "inner\n", filepath.Join(super, "top.txt"): "top\n"} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	hookGit(t, sub, "add", "inner.txt")
	hookGit(t, sub, "commit", "-qm", "inner")
	hookGit(t, super, "add", "top.txt")
	hookGit(t, super, "submodule", "-q", "add", sub, "submod")
	hookGit(t, super, "commit", "-qm", "submod")
	hookGit(t, super, "remote", "add", "origin", remote)

	database, err := store.Open(filepath.Join(t.TempDir(), "hub.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	server := httptest.NewUnstartedServer(nil)
	server.Config.Handler = hub.New(database, "secret", []string{server.Listener.Addr().String()})
	server.Start()
	defer server.Close()
	client := &hubclient.Client{Config: hubURL(server.URL)}
	ctx := context.Background()
	holder, err := client.RegisterSession(ctx, hubclient.SessionRequest{Client: "codex", SessionID: "holder", Host: "testhost", CWD: "/other", Worktree: "/other", RemoteURL: remote})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = client.Touch(ctx, hubclient.TouchRequest{ExecutionID: holder.ExecutionID, Generation: holder.Generation, ToolCallID: "held", Paths: []string{"shared.txt", "submod", "later.txt"}}); err != nil {
		t.Fatal(err)
	}

	m := fileMemory{dir: t.TempDir(), now: time.Now, restrict: func(string) error { return nil }}
	r := Runner{Hub: client, Memory: m, ShellDeadline: time.Hour, Getenv: func(key string) string {
		if key == "IRIS_HUB_URL" {
			return server.URL
		}
		return ""
	}}
	repo, err := gitinfo.Find(super)
	if err != nil {
		t.Fatal(err)
	}
	m.SetBreaker(worktreeKey(repo), breaker{V: 1, Worktree: repo.Worktree, Scope: scopeReduced, NextFullMS: time.Now().Add(breakerCap / 2).UnixMilli(), CooldownMS: time.Minute.Milliseconds()})
	ev := Event{Client: ClientClaude, Kind: KindPreTool, SessionID: "shell", CWD: super, ToolClass: ClassShell}
	if out := r.reconcile(ctx, ev, &runState{}); !isAllow(out) || !m.Capable(server.URL) {
		t.Fatalf("before: capable %t: %s", m.Capable(server.URL), out.Stdout)
	}
	write := func(path, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(super, path), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("submod/inner.txt", "changed inside the submodule\n")
	write("shared.txt", "shell edit\n")
	ev.Kind = KindPostTool
	out := r.reconcile(ctx, ev, &runState{})
	if text := string(out.Stdout); !strings.Contains(text, "Shell edit detected") || !strings.Contains(text, "shared.txt") || strings.Contains(text, "submod") {
		t.Fatalf("after: %s", text)
	}

	// A stale proof: the hub answers lost, the hook reattaches and drops this snapshot.
	m.Remember(string(ev.Client), ev.SessionID, 99)
	write("later.txt", "made before the lost answer\n")
	if out = r.reconcile(ctx, ev, &runState{}); !isAllow(out) || m.Recall(string(ev.Client), ev.SessionID) == 99 {
		t.Fatalf("repair: proof %d: %s", m.Recall(string(ev.Client), ev.SessionID), out.Stdout)
	}
	// The Execution kept its reduced baseline, so the edit is still attributed.
	out = r.reconcile(ctx, ev, &runState{})
	if text := string(out.Stdout); !strings.Contains(text, "later.txt") || strings.Contains(text, "shared.txt") {
		t.Fatalf("after the repair: %s", text)
	}
}
