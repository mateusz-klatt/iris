package kimiwake

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/proc"
)

const (
	kimiPID   = 4242
	kimiStart = int64(1_700_000_000_000)
	sessionA  = "session_114df8a5-1263-4744-bd4a-17c39274ffd6"
	sessionB  = "session_638c5a91-9064-450e-8a7a-6f48c85d9186"
)

// processes is a process table of (pid, start time) pairs that are running.
type processes map[[2]int64]bool

func (p processes) set(pid int, start int64, running bool) { p[[2]int64{int64(pid), start}] = running }

// newStore is a state directory whose Kimi process (kimiPID, kimiStart) is running, as it is for
// every hook it starts.
func newStore(t *testing.T) (*Store, *time.Time, processes) {
	t.Helper()
	now := time.UnixMilli(1_763_000_000_000)
	procs := processes{}
	procs.set(kimiPID, kimiStart, true)
	s := &Store{
		Root:  Root(t.TempDir()),
		Now:   func() time.Time { return now },
		Alive: func(pid int, start int64) bool { return procs[[2]int64{int64(pid), start}] },
	}
	return s, &now, procs
}

func keyFor(session string) Key { return Key{PID: kimiPID, StartMS: kimiStart, Session: session} }

func recordOf(t *testing.T, s *Store, k Key) (Record, bool) {
	t.Helper()
	r, exists, err := readRecord(s.recordPath(k))
	if err != nil {
		t.Fatal(err)
	}
	return r, exists
}

func put(t *testing.T, s *Store, k Key, r Record) []byte {
	t.Helper()
	if err := s.prepare(); err != nil {
		t.Fatal(err)
	}
	if err := replace(s.recordPath(k), r); err != nil {
		t.Fatal(err)
	}
	return raw(t, s.recordPath(k))
}

func raw(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// mapped reads the process's map as a reader does, under the lock.
func mapped(t *testing.T, s *Store) (string, bool) {
	t.Helper()
	id, ok, err := s.Session(kimiPID, kimiStart, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return id, ok
}

func mapSession(t *testing.T, s *Store, session string) {
	t.Helper()
	if err := s.MapSession(kimiPID, kimiStart, session, time.Second); err != nil {
		t.Fatal(err)
	}
}

func absent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("%s exists: %v", path, err)
	}
}

// noTemporaries asserts that no write left its temporary file behind.
func noTemporaries(t *testing.T, s *Store) {
	t.Helper()
	for _, dir := range []string{s.Root, filepath.Join(s.Root, sessionsName), filepath.Join(s.Root, watchersName)} {
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), ".tmp-") {
				t.Fatalf("a temporary file was left in %s: %s", dir, e.Name())
			}
		}
	}
}

// Only the trimmed text "on" enables wake; anything else, including no file, is the default, off.
func TestOnlyAnExplicitOnEnablesWake(t *testing.T) {
	pinned := t.TempDir()
	root := Root(pinned)
	if root != filepath.Join(pinned, "kimi-wake") {
		t.Fatalf("root %s", root)
	}
	if Enabled(root) {
		t.Fatal("a missing preference enabled wake")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	for text, want := range map[string]bool{"on": true, " on\n": true, "off": false, "ON": false, "": false, "on off": false} {
		if err := os.WriteFile(filepath.Join(root, preferenceName), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := Enabled(root); got != want {
			t.Fatalf("%q: enabled %v, want %v", text, got, want)
		}
	}
	if err := os.Remove(filepath.Join(root, preferenceName)); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, preferenceName), 0o700); err != nil {
		t.Fatal(err)
	}
	if Enabled(root) {
		t.Fatal("an unreadable preference enabled wake")
	}
}

// A session id becomes part of a file name, so only Kimi's own shape is accepted.
func TestOnlyKimiSessionIdsNameARecord(t *testing.T) {
	for _, id := range []string{sessionA, "a", "A-z_0", strings.Repeat("s", 128)} {
		if !ValidSession(id) {
			t.Fatalf("%q was refused", id)
		}
	}
	for _, id := range []string{"", strings.Repeat("s", 129), "../session", `session\x`, "session/x", "C:session", "session.json", "session x", "sessión"} {
		if ValidSession(id) {
			t.Fatalf("%q was accepted", id)
		}
	}
	for _, k := range []Key{{0, kimiStart, sessionA}, {kimiPID, 0, sessionA}, {kimiPID, kimiStart, "../x"}} {
		if ask, err := (&Store{Root: t.TempDir()}).RequestArm(k, time.Second); ask || !errors.Is(err, errKey) {
			t.Fatalf("%+v: %v %v", k, ask, err)
		}
	}
	if err := (&Store{Root: t.TempDir()}).MapSession(kimiPID, kimiStart, "session x", time.Second); !errors.Is(err, errKey) {
		t.Fatal(err)
	}
}

// A Stop asks for a watcher once. Inside the grace it asks nothing more, because the watcher may
// not have published itself yet; after it, a request no live watcher answered latches failed
// instead of asking at the end of every turn, and the latch keeps every later Stop quiet.
func TestAStopAsksOnceThenWaitsForItsWatcherThenLatches(t *testing.T) {
	s, now, _ := newStore(t)
	k := keyFor(sessionA)
	mapSession(t, s, sessionA)
	ask, err := s.RequestArm(k, time.Second)
	if err != nil || !ask {
		t.Fatalf("first stop: %v %v", ask, err)
	}
	r, _ := recordOf(t, s, k)
	if r.Version != recordVersion || r.ArmRequestedMS != now.UnixMilli() || r.Latch != nil || r.Attempt != nil || r.Checkpoint.Initialized {
		t.Fatalf("after the request: %+v", r)
	}
	*now = now.Add(Grace - time.Millisecond)
	if ask, err := s.RequestArm(k, time.Second); err != nil || ask {
		t.Fatalf("inside the grace: %v %v", ask, err)
	}
	*now = now.Add(time.Millisecond)
	ask, err = s.RequestArm(k, time.Second)
	r, _ = recordOf(t, s, k)
	if err != nil || ask || r.Latch == nil || *r.Latch != (Latch{State: LatchFailed, AtMS: now.UnixMilli()}) || r.ArmRequestedMS != 0 {
		t.Fatalf("after the grace: %v %v %+v", ask, err, r)
	}
	*now = now.Add(time.Hour)
	if ask, err := s.RequestArm(k, time.Second); err != nil || ask {
		t.Fatalf("latched: %v %v", ask, err)
	}
}

// A request dated in the future, after the clock went back, cannot be judged young, so it latches
// rather than suppressing every request until the clock catches up.
func TestARequestFromAClockThatWentBackLatches(t *testing.T) {
	s, now, _ := newStore(t)
	k := keyFor(sessionA)
	mapSession(t, s, sessionA)
	put(t, s, k, Record{Version: recordVersion, ArmRequestedMS: now.UnixMilli() + 1})
	ask, err := s.RequestArm(k, time.Second)
	r, _ := recordOf(t, s, k)
	if err != nil || ask || r.Latch == nil || r.Latch.State != LatchFailed {
		t.Fatalf("%v %v %+v", ask, err, r)
	}
}

// A live watcher owns its record, so a Stop leaves it untouched. Once the watcher's process ends
// without cleaning up, its stale claim no longer counts and a Stop asks again.
func TestALiveWatcherIsLeftAloneAndADeadOneIsReplaced(t *testing.T) {
	s, now, procs := newStore(t)
	k := keyFor(sessionA)
	mapSession(t, s, sessionA)
	watcher := Attempt{PID: 9001, StartMS: 9002, ID: "attempt-1"}
	procs.set(watcher.PID, watcher.StartMS, true)
	before := put(t, s, k, Record{Version: recordVersion, Attempt: &watcher, Checkpoint: Checkpoint{Stream: "s1", Cursor: 7, Initialized: true}})
	if ask, err := s.RequestArm(k, time.Second); err != nil || ask {
		t.Fatalf("live watcher: %v %v", ask, err)
	}
	if after := raw(t, s.recordPath(k)); !bytes.Equal(before, after) {
		t.Fatalf("a Stop rewrote a live watcher's record:\n%s\n%s", before, after)
	}
	procs.set(watcher.PID, watcher.StartMS, false)
	ask, err := s.RequestArm(k, time.Second)
	r, _ := recordOf(t, s, k)
	if err != nil || !ask || r.ArmRequestedMS != now.UnixMilli() || r.Checkpoint != (Checkpoint{Stream: "s1", Cursor: 7, Initialized: true}) {
		t.Fatalf("dead watcher: %v %v %+v", ask, err, r)
	}
}

// A Stop asks only for the session its process runs now, as SessionStart published it. Without a
// map, or with one naming another session, it asks nothing and creates no record.
func TestAStopAsksOnlyForTheSessionItsProcessRuns(t *testing.T) {
	s, _, _ := newStore(t)
	a, b := keyFor(sessionA), keyFor(sessionB)
	// Before any SessionStart there is no state directory, and a read creates none.
	if id, ok := mapped(t, s); ok || id != "" {
		t.Fatalf("mapped %q before any SessionStart", id)
	}
	absent(t, s.Root)
	if ask, err := s.RequestArm(a, time.Second); err != nil || ask {
		t.Fatalf("no map: %v %v", ask, err)
	}
	absent(t, s.recordPath(a))
	mapSession(t, s, sessionB)
	if ask, err := s.RequestArm(a, time.Second); err != nil || ask {
		t.Fatalf("other session: %v %v", ask, err)
	}
	absent(t, s.recordPath(a))
	if ask, err := s.RequestArm(b, time.Second); err != nil || !ask {
		t.Fatalf("mapped session: %v %v", ask, err)
	}
	// A map that is not a session map names no session.
	for _, body := range []string{"{", `{"session_id":"../x"}`, `{}`} {
		if err := os.WriteFile(s.sessionPath(kimiPID, kimiStart), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if id, ok := mapped(t, s); ok || id != "" {
			t.Fatalf("%q mapped %q", body, id)
		}
	}
}

// After /clear the process runs a new session: the map moves to it, and the old session's record
// keeps its latch. A SessionStart of a session that has a record clears its latch and request, the
// re-arm point of ADR 030, and keeps its cursor, identities and coalescing. A record with nothing
// to clear is not rewritten, and a session without a record gets none.
func TestSessionStartRepointsTheProcessAndClearsOnlyTheNewKeysLatch(t *testing.T) {
	s, _, _ := newStore(t)
	a, b := keyFor(sessionA), keyFor(sessionB)
	mapSession(t, s, sessionA)
	oldRecord := put(t, s, a, Record{Version: recordVersion, Latch: &Latch{State: LatchFailed, AtMS: 1}})
	mapSession(t, s, sessionB)
	if id, ok := mapped(t, s); !ok || id != sessionB {
		t.Fatalf("map after /clear: %q %v", id, ok)
	}
	if !bytes.Equal(oldRecord, raw(t, s.recordPath(a))) {
		t.Fatal("a SessionStart of the new session changed the old session's record")
	}
	absent(t, s.recordPath(b))
	durable := Record{
		Version:        recordVersion,
		ArmRequestedMS: 5,
		Attempt:        &Attempt{PID: 9001, StartMS: 9002, ID: "attempt-1"},
		Latch:          &Latch{State: LatchExpired, AtMS: 6},
		Checkpoint:     Checkpoint{Stream: "s1", Cursor: 7, Initialized: true},
		Emitted:        map[string]Identity{"inbox_changed": {Stream: "s1", Cursor: 7}},
		Coalesce:       map[string]int64{"inbox_changed": 8},
	}
	put(t, s, b, durable)
	mapSession(t, s, sessionB)
	got, _ := recordOf(t, s, b)
	want := durable
	want.ArmRequestedMS, want.Latch = 0, nil
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("after SessionStart:\n%+v\nwant\n%+v", got, want)
	}
	quiet := put(t, s, b, Record{Version: recordVersion, Checkpoint: Checkpoint{Stream: "s1", Cursor: 9, Initialized: true}})
	if err := os.Chtimes(s.recordPath(b), time.Unix(1, 0), time.Unix(1, 0)); err != nil {
		t.Fatal(err)
	}
	mapSession(t, s, sessionB)
	info, err := os.Stat(s.recordPath(b))
	if err != nil || !info.ModTime().Equal(time.Unix(1, 0)) || !bytes.Equal(quiet, raw(t, s.recordPath(b))) {
		t.Fatalf("a record with nothing to clear was rewritten: %v", err)
	}
}

// SessionStart removes the maps and records of processes that have ended, and leaves running
// processes' files and names that are not state files alone.
func TestSessionStartRemovesTheStateOfEndedProcesses(t *testing.T) {
	s, _, procs := newStore(t)
	procs.set(kimiPID, kimiStart, true)
	procs.set(5000, 5001, true)
	if err := s.prepare(); err != nil {
		t.Fatal(err)
	}
	files := map[string]bool{
		// name -> kept
		"sessions/5000-5001.json":                   true,
		"sessions/6000-6001.json":                   false,
		"watchers/5000-5001-" + sessionA + ".json":  true,
		"watchers/6000-6001-" + sessionA + ".json":  false,
		"watchers/6000-6001-" + sessionB + ".json":  false,
		"sessions/notes.txt":                        true,
		"sessions/6000.json":                        true,
		"sessions/x-6001.json":                      true,
		"sessions/6000-y.json":                      true,
		"sessions/0-6001.json":                      true,
		"watchers/6000-0-" + sessionA + ".json":     true,
		"watchers/.tmp-123":                         true,
		"watchers/6000-6001-" + sessionA + ".jsonx": true,
	}
	for name := range files {
		if err := os.WriteFile(filepath.Join(s.Root, filepath.FromSlash(name)), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	mapSession(t, s, sessionA)
	for name, kept := range files {
		_, err := os.Stat(filepath.Join(s.Root, filepath.FromSlash(name)))
		if kept != (err == nil) {
			t.Fatalf("%s: kept %v, stat %v", name, kept, err)
		}
	}
	if id, ok := mapped(t, s); !ok || id != sessionA {
		t.Fatal("pruning removed the running process's own map")
	}
}

// The process whose SessionStart prunes is running, so its own map and records stay even when the
// process table cannot see it.
func TestSessionStartKeepsItsOwnProcessState(t *testing.T) {
	s, _, procs := newStore(t)
	procs.set(kimiPID, kimiStart, false)
	put(t, s, keyFor(sessionA), Record{Version: recordVersion, Checkpoint: Checkpoint{Stream: "s1", Cursor: 3, Initialized: true}})
	mapSession(t, s, sessionB)
	if id, ok := mapped(t, s); !ok || id != sessionB {
		t.Fatalf("map %q %v", id, ok)
	}
	if _, exists := recordOf(t, s, keyFor(sessionA)); !exists {
		t.Fatal("pruning removed a record of the process that is pruning")
	}
}

// pinPruneStart makes the pruning passes begin at the given places, the last one repeating.
func pinPruneStart(t *testing.T, starts ...int) {
	t.Helper()
	original, pass := pruneStart, 0
	pruneStart = func(int) int {
		start := starts[min(pass, len(starts)-1)]
		pass++
		return start
	}
	t.Cleanup(func() { pruneStart = original })
}

func count(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	return len(entries)
}

// Live entries early in the listing cannot keep the ended ones after them forever: a pass that
// starts at the top spends its limit on live entries, and a pass that starts elsewhere reaches the
// ended ones.
func TestPruningReachesEndedEntriesBehindLiveOnes(t *testing.T) {
	s, _, procs := newStore(t)
	if err := s.prepare(); err != nil {
		t.Fatal(err)
	}
	live := pruneLimit + 6
	for pid := 1; pid <= live; pid++ {
		procs.set(pid, 1, true)
		if err := os.WriteFile(s.sessionPath(pid, 1), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// "9001-1.json" and the rest sort after every live name, "9-1.json" included.
	for pid := 9001; pid <= 9005; pid++ {
		if err := os.WriteFile(s.sessionPath(pid, 1), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	sessions := filepath.Join(s.Root, sessionsName)
	pinPruneStart(t, 0, live)
	s.prune(0, 0)
	if n := count(t, sessions); n != live+5 {
		t.Fatalf("a pass from the top left %d entries, want all %d", n, live+5)
	}
	s.prune(0, 0)
	if n := count(t, sessions); n != live {
		t.Fatalf("a pass from the ended entries left %d entries, want the %d live ones", n, live)
	}
}

// One SessionStart checks a bounded number of entries, so a directory full of ended processes
// never holds the lock for long; the rest go at later SessionStarts.
func TestPruningChecksABoundedNumberOfEntries(t *testing.T) {
	s, _, _ := newStore(t)
	if err := s.prepare(); err != nil {
		t.Fatal(err)
	}
	pinPruneStart(t, 0)
	for i := 1; i <= pruneLimit+5; i++ {
		if err := os.WriteFile(s.sessionPath(i, 1), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(s.Root, watchersName, "1-1-"+sessionA+".json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	s.prune(0, 0)
	sessions, _ := os.ReadDir(filepath.Join(s.Root, sessionsName))
	watchers, _ := os.ReadDir(filepath.Join(s.Root, watchersName))
	if len(sessions) != 5 || len(watchers) != 1 {
		t.Fatalf("after one pruning: %d maps and %d records left", len(sessions), len(watchers))
	}
	s.prune(0, 0)
	sessions, _ = os.ReadDir(filepath.Join(s.Root, sessionsName))
	watchers, _ = os.ReadDir(filepath.Join(s.Root, watchersName))
	if len(sessions) != 0 || len(watchers) != 0 {
		t.Fatalf("after two prunings: %d maps and %d records left", len(sessions), len(watchers))
	}
}

// Every directory and every file is protected before it is used or gets its name, and on Unix the
// modes are the owner's alone.
func TestTheStateIsPrivate(t *testing.T) {
	s, _, _ := newStore(t)
	var protected []string
	protect = func(path string) error {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("protected %s before it existed: %v", path, err)
		}
		protected = append(protected, filepath.Base(path))
		return nil
	}
	t.Cleanup(func() { protect = defaultProtect })
	mapSession(t, s, sessionA)
	if ask, err := s.RequestArm(keyFor(sessionA), time.Second); err != nil || !ask {
		t.Fatalf("%v %v", ask, err)
	}
	// Each operation protects the three directories, then the file it writes before naming it.
	want := []string{"kimi-wake", sessionsName, watchersName, ".tmp-", "kimi-wake", sessionsName, watchersName, ".tmp-"}
	if len(protected) != len(want) {
		t.Fatalf("protected %v", protected)
	}
	for i := range want {
		if !strings.HasPrefix(protected[i], want[i]) {
			t.Fatalf("protected %v", protected)
		}
	}
	if runtime.GOOS == "windows" {
		return // config.OwnerOnly's DACL is the protection there, and the calls above show it applied
	}
	for path, mode := range map[string]os.FileMode{
		s.Root: 0o700, filepath.Join(s.Root, sessionsName): 0o700, filepath.Join(s.Root, watchersName): 0o700,
		s.sessionPath(kimiPID, kimiStart): 0o600, s.recordPath(keyFor(sessionA)): 0o600, filepath.Join(s.Root, lockName): 0o600,
	} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm()&^mode != 0 {
			t.Fatalf("%s: %v %v", path, info.Mode(), err)
		}
	}
}

var defaultProtect = protect

// A lock another process holds past the budget refuses the operation; nothing is asked or written.
func TestALockHeldPastTheBudgetRefuses(t *testing.T) {
	s, _, _ := newStore(t)
	mapSession(t, s, sessionA)
	release, err := s.lock(time.Second)
	if err != nil {
		t.Fatal(err)
	}
	ask, err := s.RequestArm(keyFor(sessionA), 20*time.Millisecond)
	if ask || !errors.Is(err, ErrLocked) {
		t.Fatalf("%v %v", ask, err)
	}
	if err := s.MapSession(kimiPID, kimiStart, sessionB, 20*time.Millisecond); !errors.Is(err, ErrLocked) {
		t.Fatal(err)
	}
	// A read waits for the lock too, and is refused past its budget.
	if id, ok, err := s.Session(kimiPID, kimiStart, 20*time.Millisecond); ok || id != "" || !errors.Is(err, ErrLocked) {
		t.Fatalf("a read past the budget: %q %v %v", id, ok, err)
	}
	release()
	absent(t, s.recordPath(keyFor(sessionA)))
	if id, _ := mapped(t, s); id != sessionA {
		t.Fatalf("a refused SessionStart re-pointed the map to %q", id)
	}
	if ask, err := s.RequestArm(keyFor(sessionA), time.Second); err != nil || !ask {
		t.Fatalf("after release: %v %v", ask, err)
	}
	// The lock file itself cannot be opened when the state directory is gone.
	if _, err := (&Store{Root: filepath.Join(t.TempDir(), "gone")}).lock(time.Second); err == nil {
		t.Fatal("locked a missing directory")
	}
}

// A record that cannot be read, or that another format wrote, is never replaced or asked over.
func TestAnUnreadableRecordIsLeftAsItIs(t *testing.T) {
	for name, setup := range map[string]func(t *testing.T, path string){
		"malformed": func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"another version": func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte(`{"version":2}`), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"not a file": func(t *testing.T, path string) {
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			s, _, _ := newStore(t)
			mapSession(t, s, sessionA)
			path := s.recordPath(keyFor(sessionA))
			setup(t, path)
			before, _ := os.ReadFile(path)
			if ask, err := s.RequestArm(keyFor(sessionA), time.Second); ask || err == nil {
				t.Fatalf("Stop: %v %v", ask, err)
			}
			// SessionStart still publishes the map, then reports the record it could not clear.
			if err := s.MapSession(kimiPID, kimiStart, sessionA, time.Second); err == nil {
				t.Fatal("SessionStart cleared an unreadable record")
			}
			if after, _ := os.ReadFile(path); !bytes.Equal(before, after) {
				t.Fatal("the unreadable record was changed")
			}
		})
	}
}

// A failure at any step of making the directories or writing a file asks nothing, records no
// request and leaves no temporary file behind.
func TestAFailedWriteAsksNothingAndLeavesNoTemporaryFile(t *testing.T) {
	failure := errors.New("injected")
	for name, inject := range map[string]func(t *testing.T, s *Store) func(){
		"directory": func(t *testing.T, s *Store) func() {
			blocker := filepath.Join(filepath.Dir(s.Root), "blocker")
			if err := os.WriteFile(blocker, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			s.Root = filepath.Join(blocker, "kimi-wake")
			return func() {}
		},
		"directory protection": func(*testing.T, *Store) func() {
			protect = func(path string) error {
				if filepath.Base(path) == watchersName {
					return failure
				}
				return nil
			}
			return func() { protect = defaultProtect }
		},
		"temporary file": func(*testing.T, *Store) func() {
			createTemp = func(string, string) (*os.File, error) { return nil, failure }
			return func() { createTemp = os.CreateTemp }
		},
		"write": func(*testing.T, *Store) func() {
			createTemp = func(dir, pattern string) (*os.File, error) {
				f, err := os.CreateTemp(dir, pattern)
				if err == nil {
					err = f.Close() // a closed file fails the write
				}
				return f, err
			}
			return func() { createTemp = os.CreateTemp }
		},
		"file protection": func(*testing.T, *Store) func() {
			protect = func(path string) error {
				if strings.HasPrefix(filepath.Base(path), ".tmp-") {
					return failure
				}
				return nil
			}
			return func() { protect = defaultProtect }
		},
		"rename": func(*testing.T, *Store) func() {
			rename = func(string, string) error { return failure }
			return func() { rename = os.Rename }
		},
	} {
		t.Run(name, func(t *testing.T) {
			s, _, _ := newStore(t)
			if name != "directory" && name != "directory protection" {
				mapSession(t, s, sessionA)
			}
			t.Cleanup(inject(t, s))
			ask, err := s.RequestArm(keyFor(sessionA), time.Second)
			if ask || err == nil {
				t.Fatalf("Stop: %v %v", ask, err)
			}
			if err := s.MapSession(kimiPID, kimiStart, sessionB, time.Second); err == nil {
				t.Fatal("SessionStart reported no failure")
			}
			if _, exists, _ := readRecord(s.recordPath(keyFor(sessionA))); exists {
				t.Fatal("a failed write recorded a request")
			}
			noTemporaries(t, s)
		})
	}
}

// Process evidence is the pid and the start time together: a reused pid is another process.
// Without a stand-in, the store reads the process table and the clock.
func TestTheStoreDecidesByTheProcessTableAndTheClock(t *testing.T) {
	self, ok := proc.Lookup(os.Getpid())
	if !ok || self.StartMS <= 0 {
		t.Fatalf("the process table gave no start time for the test process: %+v %v", self, ok)
	}
	if !running(os.Getpid(), self.StartMS) || running(os.Getpid(), self.StartMS+1) || running(0, self.StartMS) || running(os.Getpid(), 0) {
		t.Fatal("running did not match the pid and start time together")
	}
	s := &Store{Root: Root(t.TempDir())}
	k := Key{PID: os.Getpid(), StartMS: self.StartMS, Session: sessionA}
	if err := s.MapSession(k.PID, k.StartMS, sessionA, time.Second); err != nil {
		t.Fatal(err)
	}
	put(t, s, k, Record{Version: recordVersion, Attempt: &Attempt{PID: os.Getpid(), StartMS: self.StartMS, ID: "self"}})
	if ask, err := s.RequestArm(k, time.Second); err != nil || ask {
		t.Fatalf("a running watcher was asked over: %v %v", ask, err)
	}
	put(t, s, k, Record{Version: recordVersion})
	before := time.Now().UnixMilli()
	ask, err := s.RequestArm(k, time.Second)
	r, _ := recordOf(t, s, k)
	if err != nil || !ask || r.ArmRequestedMS < before || r.ArmRequestedMS > time.Now().UnixMilli() {
		t.Fatalf("%v %v %+v", ask, err, r)
	}
}

// entryProcess reads only names a state file can have.
func TestStateFileNamesNameTheirProcess(t *testing.T) {
	for name, want := range map[string][3]int64{
		"1-2.json":                       {1, 2, 1},
		"1-2-" + sessionA + ".json":      {1, 2, 1},
		"1-2-x-y.json":                   {1, 2, 1},
		"1-2":                            {0, 0, 0},
		"1.json":                         {0, 0, 0},
		"a-2.json":                       {0, 0, 0},
		"1-b.json":                       {0, 0, 0},
		"-1-2.json":                      {0, 0, 0},
		"1--2.json":                      {0, 0, 0},
		"0-2.json":                       {0, 0, 0},
		"1-0.json":                       {0, 0, 0},
		"99999999999999999999-2.json":    {0, 0, 0},
		"1-99999999999999999999.json":    {0, 0, 0},
		".tmp-1-2.json":                  {0, 0, 0},
		"1-2.json.tmp":                   {0, 0, 0},
		"1-2-" + sessionA + ".json.json": {1, 2, 1},
	} {
		pid, start, ok := entryProcess(name)
		got := [3]int64{int64(pid), start, 0}
		if ok {
			got[2] = 1
		}
		if !slices.Equal(got[:], want[:]) {
			t.Fatalf("%s: %v, want %v", name, got, want)
		}
	}
}
