package hook

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/config"
	"github.com/mateusz-klatt/iris/internal/hubclient"
)

func TestSessionsUsesTheHooksPathsWithoutReadingTheToken(t *testing.T) {
	parent := t.TempDir()
	// A directory in place of a token cannot be read as a credential. Construction only uses
	// its path, so it works equally for a token that has not been written yet.
	token := filepath.Join(parent, "token")
	if err := os.Mkdir(token, 0o700); err != nil {
		t.Fatal(err)
	}
	s := NewSessions(token)
	if got, want := s.Dir(), filepath.Join(parent, "sessions"); got != want {
		t.Fatalf("session directory = %q, want %q", got, want)
	}
	if _, err := os.Stat(s.Dir()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("construction created session files: %v", err)
	}
	want := filepath.Join(s.Dir(), "wake", "11f83d32883e99de36088c8bc97e00eb")
	if got := s.WakePath("codex", "session"); got != want || got != s.memory.wakePath("codex", "session") {
		t.Fatalf("wake path = %q, want %q", got, want)
	}
	if s.WakePath("claude", "session") == want || s.WakePath("codex", "another") == want {
		t.Fatal("different clients or sessions share a wake file")
	}
	if WakeVersion != 1 || SecondNoteAfter != 10*time.Minute {
		t.Fatal("the shared constants diverge from the hook format or second-note wait")
	}
}

func newSessions(t *testing.T) *Sessions {
	t.Helper()
	s := NewSessions(filepath.Join(t.TempDir(), "hub.token"))
	if err := s.Protect(); err != nil {
		t.Fatal(err)
	}
	return s
}

func sessionTold(t *testing.T, s *Sessions) ToldSnapshot {
	t.Helper()
	unlock, err := s.Lock(hookLockBudget)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	snapshot, err := s.Told("codex", "session")
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func writeSessionTold(t *testing.T, s *Sessions, raw string) {
	t.Helper()
	unlock, err := s.Lock(hookLockBudget)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if err := os.WriteFile(s.memory.counts("codex", "session"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestSessionsToldPreservesTheCompleteReadAndCurrentParsing(t *testing.T) {
	for name, tc := range map[string]struct {
		raw      string
		identity hubclient.Identity
		at, rev  int64
		twice    bool
	}{
		"empty":         {identity: hubclient.Identity{Known: true}},
		"legacy counts": {raw: "3 4 1 2\n", identity: hubclient.Identity{Known: true}},
		"no identity":   {raw: "3 4 1 2 - 0 0 0 8\n", identity: hubclient.Identity{Known: true}, rev: 8},
		"first line only": {
			raw:      " 1 2 3 4 73747265616d 10 20 1 30 \r\n0 0 0 0 6f74686572 100 200 0 300\n",
			identity: hubclient.Identity{Known: true, Pending: true, Stream: "stream", Cursor: 10}, at: 20, rev: 30, twice: true,
		},
		"damage is per field": {
			raw:      "bad 4 no 2 zz 10 junk 1 9\n",
			identity: hubclient.Identity{Known: true, Cursor: 10}, rev: 9, twice: true,
		},
		"partial hex and negative values": {
			raw:      "0 0 0 0 7367zz -2 -3 2 -4 ignored extra fields",
			identity: hubclient.Identity{Known: true, Pending: true, Stream: "sg", Cursor: -2}, at: -3, rev: -4,
		},
		"a later identity is not the first line": {
			raw: "\n0 0 0 0 73747265616d 10 20 1 30\n", identity: hubclient.Identity{Known: true},
		},
	} {
		t.Run(name, func(t *testing.T) {
			s := newSessions(t)
			writeSessionTold(t, s, tc.raw)
			got := sessionTold(t, s)
			want := ToldSnapshot{Present: true, Raw: tc.raw, Identity: tc.identity, ToldMS: tc.at, Twice: tc.twice, Revision: tc.rev}
			if got != want {
				t.Fatalf("snapshot = %#v, want %#v", got, want)
			}
			if raw, err := os.ReadFile(s.memory.counts("codex", "session")); err != nil || string(raw) != tc.raw {
				t.Fatalf("reading changed the told state: %q, %v", raw, err)
			}
		})
	}
}

func TestSessionsToldObservesPresenceAndSameRevisionChanges(t *testing.T) {
	s := newSessions(t)
	missing := sessionTold(t, s)
	if missing != (ToldSnapshot{}) {
		t.Fatalf("a missing file has told state: %#v", missing)
	}
	writeSessionTold(t, s, "")
	empty := sessionTold(t, s)
	if !empty.Present || empty.Raw != missing.Raw || empty == missing {
		t.Fatalf("empty and missing files cannot be distinguished: %#v, %#v", empty, missing)
	}
	first := "0 0 0 0 73747265616d 9 100 0 7\n"
	writeSessionTold(t, s, first)
	before := sessionTold(t, s)
	writeSessionTold(t, s, "0 0 0 0 73747265616d 9 100 1 7\n")
	twice := sessionTold(t, s)
	if before == twice || before.Revision != twice.Revision || !twice.Twice {
		t.Fatalf("a second telling at the same revision was missed: %#v, %#v", before, twice)
	}
	unlock, err := s.Lock(hookLockBudget)
	if err != nil {
		t.Fatal(err)
	}
	err = os.Remove(s.memory.counts("codex", "session"))
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	if pruned := sessionTold(t, s); pruned != missing {
		t.Fatalf("a pruned file still has told state: %#v", pruned)
	}
	writeSessionTold(t, s, "0 0 0 0 73747265616d 9 200 0 7\n")
	recreated := sessionTold(t, s)
	if before == recreated || before.Revision != recreated.Revision || recreated.ToldMS != 200 {
		t.Fatalf("a recreated file reusing the revision was missed: %#v, %#v", before, recreated)
	}
	// Details after the first line are not parsed, but still participate in snapshot equality.
	writeSessionTold(t, s, first+"additional detail\x00\n")
	detail := sessionTold(t, s)
	if before == detail || before.Identity != detail.Identity || before.Revision != detail.Revision || before.ToldMS != detail.ToldMS {
		t.Fatalf("complete bytes did not retain unparsed changes: %#v, %#v", before, detail)
	}
}

func TestSessionsToldReturnsReadErrorsWithoutFreshAllowance(t *testing.T) {
	s := newSessions(t)
	path := s.memory.counts("codex", "session")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	unlock, err := s.Lock(hookLockBudget)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	snapshot, err := s.Told("codex", "session")
	var readErr *os.PathError
	if !errors.As(err, &readErr) || readErr.Path != path || snapshot != (ToldSnapshot{}) {
		t.Fatalf("an unreadable told file became fresh state: %#v, %v", snapshot, err)
	}
	if info, err := os.Stat(path); err != nil || !info.IsDir() {
		t.Fatalf("the unreadable told path changed: %v, %v", info, err)
	}
}

func TestSessionsProtectAndLockKeepDirectoryProtectionOutsideTheLock(t *testing.T) {
	s := NewSessions(filepath.Join(t.TempDir(), "hub.token"))
	protected := 0
	s.memory.restrict = func(path string) error {
		if path != s.Dir() {
			t.Fatalf("protected %q, want sessions directory", path)
		}
		protected++
		// Taking the same lock while protection runs proves Protect does not hold it.
		unlock, err := s.Lock(0)
		if err != nil {
			return err
		}
		unlock()
		return config.OwnerOnly(path)
	}
	if err := s.Protect(); err != nil || protected != 1 {
		t.Fatalf("directory protection held the shared lock: %v, calls %d", err, protected)
	}
	if runtime.GOOS != "windows" {
		if info, err := os.Stat(s.Dir()); err != nil || info.Mode().Perm() != 0o700 {
			t.Fatalf("session directory permissions: %v, %v", info, err)
		}
	}
	unlock, err := s.Lock(hookLockBudget)
	if err != nil {
		t.Fatal(err)
	}
	unlock()
	if protected != 1 {
		t.Fatal("locking an existing directory reapplied its recursive protection")
	}
}

func TestSessionsLockSharesTheStableHookLockAndBudget(t *testing.T) {
	s := newSessions(t)
	unlock, err := s.Lock(hookLockBudget)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(filepath.Join(s.Dir(), sessionsLock))
	if err != nil {
		unlock()
		t.Fatal(err)
	}
	started := time.Now()
	const budget = 10 * time.Millisecond
	other, err := s.memory.lock(budget)
	if err == nil {
		other()
	}
	unlock()
	if !errors.Is(err, ErrSessionsLocked) || time.Since(started) < budget {
		t.Fatalf("a hook ignored the shared lock or its budget: %v", err)
	}
	// Reverse the callers: an existing hook lock must also hold the exported API off.
	held, err := s.memory.lock(hookLockBudget)
	if err != nil {
		t.Fatal(err)
	}
	blocked, err := s.Lock(0)
	if err == nil {
		blocked()
	}
	held()
	if !errors.Is(err, failSessionsLock) {
		t.Fatalf("the exported API ignored a hook's lock: %v", err)
	}
	again, err := s.Lock(0)
	if err != nil {
		t.Fatalf("a released lock could not be retaken: %v", err)
	}
	again()
	after, err := os.Stat(filepath.Join(s.Dir(), sessionsLock))
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("locking replaced the stable lock file: %v", err)
	}
}

func TestSessionsReturnsProtectionAndLockErrors(t *testing.T) {
	s := NewSessions(filepath.Join(t.TempDir(), "hub.token"))
	refused := errors.New("protection refused")
	s.memory.restrict = func(string) error { return refused }
	if err := s.Protect(); !errors.Is(err, refused) {
		t.Fatalf("protection failure was hidden: %v", err)
	}
	// Lock also protects a directory when it must create one; this remains best effort within
	// the existing hook contract rather than creating an unprotected lock directory.
	s = NewSessions(filepath.Join(t.TempDir(), "hub.token"))
	s.memory.restrict = func(string) error { return refused }
	if unlock, err := s.Lock(hookLockBudget); !errors.Is(err, refused) || unlock != nil {
		t.Fatalf("lock creation hid a protection failure: %v", err)
	}
	s = newSessions(t)
	if err := os.Mkdir(filepath.Join(s.Dir(), sessionsLock), 0o700); err != nil {
		t.Fatal(err)
	}
	if unlock, err := s.Lock(hookLockBudget); err == nil || unlock != nil || errors.Is(err, failSessionsLock) {
		t.Fatalf("an unusable lock path became a timeout or lock: %v", err)
	}
	parent := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(parent, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	s = NewSessions(filepath.Join(parent, "hub.token"))
	if err := s.Protect(); err == nil {
		t.Fatal("a sessions directory under a regular file was protected")
	}
}

func TestSessionsLockRefusesProtectionFailureAndClosesTheFile(t *testing.T) {
	s := newSessions(t)
	original := protectSessionsLock
	t.Cleanup(func() { protectSessionsLock = original })
	refused := errors.New("lock permissions refused")
	var opened *os.File
	protectSessionsLock = func(f *os.File) error {
		opened = f
		return refused
	}
	if unlock, err := s.Lock(hookLockBudget); !errors.Is(err, refused) || unlock != nil {
		t.Fatalf("failed lock protection was hidden: %v", err)
	}
	if opened == nil {
		t.Fatal("the lock file was not opened before protection")
	}
	// Fd returns this sentinel after Close on every platform. Stat's failure varies: Windows
	// can report an invalid handle instead of wrapping os.ErrClosed.
	if fd := opened.Fd(); fd != ^uintptr(0) {
		t.Fatalf("the refused lock descriptor leaked: %v", fd)
	}
	if _, err := opened.Stat(); err == nil {
		t.Fatal("the refused lock file remained usable after protection failed")
	}
	protectSessionsLock = original
	unlock, err := s.Lock(hookLockBudget)
	if err != nil {
		t.Fatalf("the refused lock did not recover when protection recovered: %v", err)
	}
	unlock()
}

func TestSessionsWakePathKeepsReservationFieldsThroughHookConsumption(t *testing.T) {
	s := newSessions(t)
	const body = `{"version":1,"mark":{"stream":"stream","cursor":5},"attempts":[` +
		`{"id":"old","kind":"queue","slot":0,"token":"old-token","message":"old prompt","answer":"submitted","consumed_ms":1,"extra":{"detail":true}},` +
		`{"id":"current","kind":"queue","slot":1,"token":"current-token","message":"current prompt","written_ms":200,"extra":[1,2,3]}],"extra":"envelope"}`
	writeWake(t, s.memory, "codex", "session", body)
	s.memory.now = func() time.Time { return time.Unix(1_800_000_000, 0) }
	if err := s.memory.consumeWake("codex", "session", "current prompt"); err != nil {
		t.Fatal(err)
	}
	doc := wakeOf(t, s.memory, "session")
	attempts := doc["attempts"].([]any)
	old, current := attempts[0].(map[string]any), attempts[1].(map[string]any)
	if old["id"] != "old" || old["kind"] != "queue" || old["slot"] != float64(0) || old["consumed_ms"] != float64(1) || old["extra"] == nil {
		t.Fatalf("compaction lost the old reservation's identity or quota: %v", old)
	}
	if old["token"] != nil || old["message"] != nil || old["answer"] != nil {
		t.Fatalf("long-consumed prompt details were not compacted: %v", old)
	}
	if current["id"] != "current" || current["kind"] != "queue" || current["token"] != "current-token" || current["message"] != "current prompt" || current["extra"] == nil || current["consumed_ms"] != float64(s.memory.now().UnixMilli()) {
		t.Fatalf("consumption lost current reservation fields: %v", current)
	}
	if doc["extra"] != "envelope" || doc["mark"] == nil {
		t.Fatalf("consumption lost envelope fields: %v", doc)
	}
}
