package kimiwake

import (
	"bytes"
	"errors"
	"io/fs"
	"maps"
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

// A prompt asks for a watcher once. A request still pending at the next prompt was not answered by
// the turn that received it, so it latches failed instead of asking at every turn, and the latch
// keeps every later prompt quiet.
func TestAPromptAsksOnceThenLatchesAnUnansweredRequest(t *testing.T) {
	s, now, _ := newStore(t)
	k := keyFor(sessionA)
	mapSession(t, s, sessionA)
	ask, err := s.RequestArm(k, time.Second)
	if err != nil || !ask {
		t.Fatalf("first prompt: %v %v", ask, err)
	}
	r, _ := recordOf(t, s, k)
	if r.Version != recordVersion || r.ArmRequestedMS != now.UnixMilli() || r.Latch != nil || r.Attempt != nil || r.Checkpoint.Initialized {
		t.Fatalf("after the request: %+v", r)
	}
	*now = now.Add(time.Second)
	ask, err = s.RequestArm(k, time.Second)
	r, _ = recordOf(t, s, k)
	if err != nil || ask || r.Latch == nil || *r.Latch != (Latch{State: LatchFailed, AtMS: now.UnixMilli()}) || r.ArmRequestedMS != 0 {
		t.Fatalf("next prompt: %v %v %+v", ask, err, r)
	}
	*now = now.Add(time.Hour)
	if ask, err := s.RequestArm(k, time.Second); err != nil || ask {
		t.Fatalf("latched: %v %v", ask, err)
	}
}

// Read gives a key's record under the lock, makes nothing, and refuses an invalid key and a lock
// held past its budget.
func TestARecordIsReadUnderTheLock(t *testing.T) {
	s, _, _ := newStore(t)
	k := keyFor(sessionA)
	if r, exists, err := s.Read(k, time.Second); err != nil || exists || r.Version != recordVersion {
		t.Fatalf("before any state: %+v %v %v", r, exists, err)
	}
	absent(t, s.Root)
	if _, _, err := s.Read(Key{}, time.Second); !errors.Is(err, errKey) {
		t.Fatal(err)
	}
	mapSession(t, s, sessionA)
	want := Record{Version: recordVersion, Latch: &Latch{State: LatchExpired, AtMS: 5}}
	put(t, s, k, want)
	if r, exists, err := s.Read(k, time.Second); err != nil || !exists || !reflect.DeepEqual(r, want) {
		t.Fatalf("%+v %v %v", r, exists, err)
	}
	release, err := s.lock(time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if _, _, err := s.Read(k, 20*time.Millisecond); !errors.Is(err, ErrLocked) {
		t.Fatal(err)
	}
}

// A live watcher owns its record, so a prompt leaves it untouched. Once the watcher's process ends
// without cleaning up, its stale claim no longer counts and a prompt asks again.
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
		t.Fatalf("a prompt rewrote a live watcher's record:\n%s\n%s", before, after)
	}
	procs.set(watcher.PID, watcher.StartMS, false)
	ask, err := s.RequestArm(k, time.Second)
	r, _ := recordOf(t, s, k)
	if err != nil || !ask || r.ArmRequestedMS != now.UnixMilli() || r.Checkpoint != (Checkpoint{Stream: "s1", Cursor: 7, Initialized: true}) {
		t.Fatalf("dead watcher: %v %v %+v", ask, err, r)
	}
}

// A prompt asks only for the session its process runs now, as SessionStart published it. Without a
// map, or with one naming another session, it asks nothing and creates no record.
func TestAPromptAsksOnlyForTheSessionItsProcessRuns(t *testing.T) {
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
				t.Fatalf("request: %v %v", ask, err)
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
				t.Fatalf("request: %v %v", ask, err)
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

// A watcher owns its record first: the request it answers and any latch go at once, and the
// durable state stays. A second live watcher is a duplicate and changes nothing; a dead holder's
// claim is taken over, and owning again is the same claim.
func TestAWatcherOwnsItsRecordAndAnotherLiveOneIsADuplicate(t *testing.T) {
	s, _, procs := newStore(t)
	k := keyFor(sessionA)
	mapSession(t, s, sessionA)
	put(t, s, k, Record{Version: recordVersion, ArmRequestedMS: 5, Latch: &Latch{State: LatchFailed, AtMS: 6}, Checkpoint: Checkpoint{Stream: "s1", Cursor: 7, Initialized: true}})
	first, second := Attempt{PID: 9001, StartMS: 9002, ID: "first"}, Attempt{PID: 9003, StartMS: 9004, ID: "second"}
	procs.set(first.PID, first.StartMS, true)
	if owned, err := s.Own(k, first, time.Second); err != nil || !owned {
		t.Fatalf("first: %v %v", owned, err)
	}
	r, _ := recordOf(t, s, k)
	if r.Attempt == nil || *r.Attempt != first || r.ArmRequestedMS != 0 || r.Latch != nil || r.Checkpoint != (Checkpoint{Stream: "s1", Cursor: 7, Initialized: true}) {
		t.Fatalf("owned: %+v", r)
	}
	before := raw(t, s.recordPath(k))
	if owned, err := s.Own(k, second, time.Second); err != nil || owned {
		t.Fatalf("duplicate: %v %v", owned, err)
	}
	if !bytes.Equal(before, raw(t, s.recordPath(k))) {
		t.Fatal("a duplicate changed the record")
	}
	if owned, err := s.Own(k, first, time.Second); err != nil || !owned {
		t.Fatalf("the same watcher again: %v %v", owned, err)
	}
	procs.set(first.PID, first.StartMS, false)
	if owned, err := s.Own(k, second, time.Second); err != nil || !owned {
		t.Fatalf("after the holder ended: %v %v", owned, err)
	}
	for _, bad := range []Attempt{{PID: 0, StartMS: 1, ID: "x"}, {PID: 1, StartMS: 0, ID: "x"}, {PID: 1, StartMS: 1}} {
		if _, err := s.Own(k, bad, time.Second); !errors.Is(err, errKey) {
			t.Fatalf("%+v: %v", bad, err)
		}
	}
	if _, err := s.Own(Key{}, first, time.Second); !errors.Is(err, errKey) {
		t.Fatal(err)
	}
}

// A prompt judged before the requested watcher publishes itself latches, and the publication then
// clears the latch. A watcher published first keeps the prompt quiet. Either order ends armed.
func TestARacingPromptAndTheWatchersPublicationEndArmed(t *testing.T) {
	for name, promptFirst := range map[string]bool{"prompt first": true, "watcher first": false} {
		t.Run(name, func(t *testing.T) {
			s, _, procs := newStore(t)
			k := keyFor(sessionA)
			mapSession(t, s, sessionA)
			if ask, err := s.RequestArm(k, time.Second); err != nil || !ask {
				t.Fatalf("request: %v %v", ask, err)
			}
			w := Attempt{PID: 9001, StartMS: 9002, ID: "w"}
			procs.set(w.PID, w.StartMS, true)
			if promptFirst {
				if ask, err := s.RequestArm(k, time.Second); err != nil || ask {
					t.Fatalf("racing prompt: %v %v", ask, err)
				}
			}
			if owned, err := s.Own(k, w, time.Second); err != nil || !owned {
				t.Fatalf("publication: %v %v", owned, err)
			}
			if !promptFirst {
				if ask, err := s.RequestArm(k, time.Second); err != nil || ask {
					t.Fatalf("later prompt: %v %v", ask, err)
				}
			}
			if r, _ := recordOf(t, s, k); r.Latch != nil || r.ArmRequestedMS != 0 || r.Attempt == nil || r.Attempt.ID != "w" {
				t.Fatalf("not armed: %+v", r)
			}
		})
	}
}

// Only the watcher holding a record changes it. Once it has released the record, or another holds
// it, nothing is written, and the durable state stays.
func TestOnlyTheHoldingWatcherUpdatesItsRecord(t *testing.T) {
	s, _, procs := newStore(t)
	k := keyFor(sessionA)
	mapSession(t, s, sessionA)
	w := Attempt{PID: 9001, StartMS: 9002, ID: "w"}
	procs.set(w.PID, w.StartMS, true)
	if owned, err := s.Own(k, w, time.Second); err != nil || !owned {
		t.Fatalf("%v %v", owned, err)
	}
	baseline := Checkpoint{Stream: "s1", Cursor: 3, Initialized: true}
	if owned, err := s.UpdateOwned(k, "w", time.Second, func(r *Record) { r.Checkpoint = baseline }); err != nil || !owned {
		t.Fatalf("holder: %v %v", owned, err)
	}
	if owned, err := s.UpdateOwned(k, "other", time.Second, func(*Record) { t.Fatal("changed for another watcher") }); err != nil || owned {
		t.Fatalf("another: %v %v", owned, err)
	}
	if owned, err := s.UpdateOwned(k, "w", time.Second, func(r *Record) { r.Attempt = nil }); err != nil || !owned {
		t.Fatalf("release: %v %v", owned, err)
	}
	if owned, err := s.UpdateOwned(k, "w", time.Second, func(*Record) { t.Fatal("changed after release") }); err != nil || owned {
		t.Fatalf("after release: %v %v", owned, err)
	}
	if r, _ := recordOf(t, s, k); r.Attempt != nil || r.Checkpoint != baseline {
		t.Fatalf("after release: %+v", r)
	}
	if _, err := s.UpdateOwned(k, "", time.Second, func(*Record) {}); !errors.Is(err, errKey) {
		t.Fatal(err)
	}
	if _, err := s.UpdateOwned(Key{}, "w", time.Second, func(*Record) {}); !errors.Is(err, errKey) {
		t.Fatal(err)
	}
}

// Coalescing data only delays a wake, so a malformed deadline, or a coalescing value that is not an
// object, decodes as no delay. The record stays usable: a watcher can own and update it, and its
// checkpoint survives.
func TestMalformedCoalescingDataIsNoDelay(t *testing.T) {
	for body, want := range map[string]Delays{
		`{"version":1,"checkpoint":{"stream":"s1","cursor":4,"initialized":true},"coalesce":{"inbox_changed":9,"claim_available":"bad","x":1.5}}`: {"inbox_changed": 9},
		`{"version":1,"checkpoint":{"stream":"s1","cursor":4,"initialized":true},"coalesce":"bad"}`:                                               {},
		`{"version":1,"checkpoint":{"stream":"s1","cursor":4,"initialized":true},"coalesce":[1,2]}`:                                               {},
	} {
		s, _, procs := newStore(t)
		k := keyFor(sessionA)
		mapSession(t, s, sessionA)
		if err := os.WriteFile(s.recordPath(k), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		w := Attempt{PID: 9001, StartMS: 9002, ID: "w"}
		procs.set(w.PID, w.StartMS, true)
		if owned, err := s.Own(k, w, time.Second); err != nil || !owned {
			t.Fatalf("%s: own %v %v", body, owned, err)
		}
		var seen Delays
		if owned, err := s.UpdateOwned(k, "w", time.Second, func(r *Record) { seen = r.Coalesce }); err != nil || !owned {
			t.Fatalf("%s: update %v %v", body, owned, err)
		}
		r, _ := recordOf(t, s, k)
		if !maps.Equal(seen, want) || r.Checkpoint != (Checkpoint{Stream: "s1", Cursor: 4, Initialized: true}) {
			t.Fatalf("%s: coalesce %v, record %+v", body, seen, r)
		}
	}
}

// The preference records the choice: none before integration or in an emptied file, then default,
// on or off. Only on enables wake for now; anything else in the file is refused rather than guessed.
func TestThePreferenceRecordsTheChoice(t *testing.T) {
	s, _, _ := newStore(t)
	path := PreferencePath(s.Root)
	if got, err := Preference(s.Root); err != nil || got != "" || path != filepath.Join(s.Root, preferenceName) {
		t.Fatalf("before integration: %q %v at %s", got, err, path)
	}
	if err := os.MkdirAll(s.Root, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"", PreferenceDefault, PreferenceOff, PreferenceOn} {
		if err := os.WriteFile(path, []byte(value+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if got, err := Preference(s.Root); err != nil || got != value || Enabled(s.Root) != On(value) || On(value) != (value == PreferenceOn) {
			t.Fatalf("%q: %q %v, enabled %v", value, got, err, Enabled(s.Root))
		}
	}
	if err := os.WriteFile(path, []byte("maybe\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Preference(s.Root); !errors.Is(err, ErrPreference) || Enabled(s.Root) {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Preference(s.Root); err == nil || errors.Is(err, ErrPreference) {
		t.Fatalf("an unreadable preference: %v", err)
	}
}

// Stopping watchers where none ever ran makes no state: removing an integration that never used
// wake leaves the pinned directory as it was.
func TestStopWatchersWithoutStateMakesNone(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kimi-wake")
	terminate = func(int, int64) error { t.Fatal("stopped a process without a record"); return nil }
	t.Cleanup(func() { terminate = defaultTerminate })
	if err := (&Store{Root: root}).StopWatchers(time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(root); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the state directory was made: %v", err)
	}
}

// Turning wake off stops every watcher a record shows alive, by its pid and start time, and latches
// its record disabled once it no longer runs. A watcher already gone is skipped. A stop that fails
// is reported and leaves its record unlatched, without keeping the others from stopping.
func TestStopWatchersStopsTheLiveOnesAndLatchesThem(t *testing.T) {
	s, _, procs := newStore(t)
	mapSession(t, s, sessionA)
	live, gone, stubborn := keyFor(sessionA), keyFor(sessionB), Key{PID: kimiPID, StartMS: kimiStart, Session: "session_c"}
	put(t, s, live, Record{Version: recordVersion, Attempt: &Attempt{PID: 9001, StartMS: 9002, ID: "live"}})
	put(t, s, gone, Record{Version: recordVersion, Attempt: &Attempt{PID: 9003, StartMS: 9004, ID: "gone"}})
	put(t, s, stubborn, Record{Version: recordVersion, Attempt: &Attempt{PID: 9005, StartMS: 9006, ID: "stubborn"}})
	procs.set(9001, 9002, true)
	procs.set(9005, 9006, true)
	for _, name := range []string{"notes.json", "1-2.json", "1-2-bad id.json", ".tmp-123"} {
		if err := os.WriteFile(filepath.Join(s.Root, watchersName, name), []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(s.recordPath(Key{PID: kimiPID, StartMS: kimiStart, Session: "session_broken"}), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stopped []int
	terminate = func(pid int, start int64) error {
		stopped = append(stopped, pid)
		if pid == 9005 {
			return errors.New("access denied")
		}
		procs.set(pid, start, false)
		return nil
	}
	t.Cleanup(func() { terminate = defaultTerminate })
	err := s.StopWatchers(time.Second)
	if err == nil || !strings.Contains(err.Error(), "watcher 9005") || !slices.Equal(stopped, []int{9001, 9005}) && !slices.Equal(stopped, []int{9005, 9001}) {
		t.Fatalf("%v %v", err, stopped)
	}
	if r, _ := recordOf(t, s, live); r.Latch == nil || r.Latch.State != LatchDisabled {
		t.Fatalf("the stopped watcher's record: %+v", r)
	}
	if r, _ := recordOf(t, s, gone); r.Latch != nil {
		t.Fatalf("a record without a live watcher was latched: %+v", r)
	}
	if r, _ := recordOf(t, s, stubborn); r.Latch != nil {
		t.Fatalf("a watcher that could not be stopped was latched: %+v", r)
	}
	// A watcher that ended on its own before its stop counts as stopped.
	procs.set(9005, 9006, true)
	terminate = func(int, int64) error { return proc.ErrNotFound }
	if err := s.StopWatchers(time.Second); err != nil {
		t.Fatal(err)
	}
	release, err := s.lock(time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if err := s.StopWatchers(20 * time.Millisecond); !errors.Is(err, ErrLocked) {
		t.Fatal(err)
	}
}

var defaultTerminate = terminate

// A latch never replaces a live watcher. Without one it latches and supersedes a pending request.
func TestALatchNeverReplacesALiveWatcher(t *testing.T) {
	s, now, procs := newStore(t)
	k := keyFor(sessionA)
	mapSession(t, s, sessionA)
	w := Attempt{PID: 9001, StartMS: 9002, ID: "w"}
	procs.set(w.PID, w.StartMS, true)
	before := put(t, s, k, Record{Version: recordVersion, ArmRequestedMS: 5, Attempt: &w})
	if err := s.LatchIfFree(k, LatchDisabled, time.Second); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, raw(t, s.recordPath(k))) {
		t.Fatal("a latch replaced a live watcher")
	}
	procs.set(w.PID, w.StartMS, false)
	if err := s.LatchIfFree(k, LatchDisabled, time.Second); err != nil {
		t.Fatal(err)
	}
	if r, _ := recordOf(t, s, k); r.Latch == nil || *r.Latch != (Latch{State: LatchDisabled, AtMS: now.UnixMilli()}) || r.ArmRequestedMS != 0 {
		t.Fatalf("%+v", r)
	}
	if err := s.LatchIfFree(Key{}, LatchFailed, time.Second); !errors.Is(err, errKey) {
		t.Fatal(err)
	}
}
