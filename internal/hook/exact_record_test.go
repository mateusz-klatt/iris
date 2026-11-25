package hook

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/config"
	"github.com/mateusz-klatt/iris/internal/sessionproof"
)

func TestLockDeadlineChecksEveryStepAndCapsSleep(t *testing.T) {
	t.Run("expired before stat", func(t *testing.T) {
		clock := time.Unix(1_000, 0)
		m := fileMemory{
			dir:      filepath.Join(t.TempDir(), "sessions"),
			mono:     func() time.Time { return clock },
			restrict: config.OwnerOnly,
		}
		if _, err := m.lockDeadline(clock); !errors.Is(err, failSessionsLock) {
			t.Fatalf("lockDeadline = %v, want failSessionsLock", err)
		}
		if _, err := os.Stat(m.dir); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("expired lockDeadline created sessions dir: %v", err)
		}
	})

	t.Run("expired after directory creation before OpenFile", func(t *testing.T) {
		clock := time.Unix(1_000, 0)
		deadline := clock.Add(10 * time.Millisecond)
		m := fileMemory{
			dir:  filepath.Join(t.TempDir(), "sessions"),
			mono: func() time.Time { return clock },
			restrict: func(p string) error {
				clock = deadline
				return config.OwnerOnly(p)
			},
		}
		if _, err := m.lockDeadline(deadline); !errors.Is(err, failSessionsLock) {
			t.Fatalf("lockDeadline after directory = %v, want failSessionsLock", err)
		}
		if _, err := os.Stat(filepath.Join(m.dir, sessionsLock)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("expired lockDeadline created .lock: %v", err)
		}
	})

	t.Run("expired before protectSessionsLock", func(t *testing.T) {
		clock := time.Unix(1_000, 0)
		deadline := clock.Add(10 * time.Millisecond)
		m := fileMemory{
			dir:      filepath.Join(t.TempDir(), "sessions"),
			mono:     func() time.Time { return clock },
			restrict: config.OwnerOnly,
		}
		if err := m.directory(); err != nil {
			t.Fatal(err)
		}
		origProtect := protectSessionsLock
		t.Cleanup(func() { protectSessionsLock = origProtect })
		protected := false
		protectSessionsLock = func(f *os.File) error {
			protected = true
			return origProtect(f)
		}
		calls := 0
		m.mono = func() time.Time {
			calls++
			// Call 1: pre-stat, Call 2: post-stat/pre-open, Call 3: pre-protect.
			if calls == 3 {
				clock = deadline
			}
			return clock
		}
		if _, err := m.lockDeadline(deadline); !errors.Is(err, failSessionsLock) {
			t.Fatalf("lockDeadline before protect = %v, want failSessionsLock", err)
		}
		if protected {
			t.Fatal("protectSessionsLock ran past deadline")
		}
	})

	t.Run("expired after protect before tryLockFile", func(t *testing.T) {
		clock := time.Unix(1_000, 0)
		deadline := clock.Add(10 * time.Millisecond)
		m := fileMemory{
			dir:      filepath.Join(t.TempDir(), "sessions"),
			mono:     func() time.Time { return clock },
			restrict: config.OwnerOnly,
		}
		if err := m.directory(); err != nil {
			t.Fatal(err)
		}
		origProtect := protectSessionsLock
		t.Cleanup(func() { protectSessionsLock = origProtect })
		protectSessionsLock = func(f *os.File) error {
			clock = deadline
			return origProtect(f)
		}
		if _, err := m.lockDeadline(deadline); !errors.Is(err, failSessionsLock) {
			t.Fatalf("lockDeadline before tryLockFile = %v, want failSessionsLock", err)
		}
	})

	t.Run("caps retry sleep to remaining budget and stops on zero remainder", func(t *testing.T) {
		clock := time.Unix(1_000, 0)
		m := fileMemory{
			dir:      filepath.Join(t.TempDir(), "sessions"),
			restrict: config.OwnerOnly,
		}
		holder, err := m.lock(hookLockBudget)
		if err != nil {
			t.Fatal(err)
		}
		defer holder()

		var sleeps []time.Duration
		m.mono = func() time.Time { return clock }
		m.pause = func(d time.Duration) {
			sleeps = append(sleeps, d)
			clock = clock.Add(d)
		}
		// 2.5 ms budget: first pause is 2 ms, second pause is capped to 500 us, then stops.
		deadline := clock.Add(2*time.Millisecond + 500*time.Microsecond)
		if _, err := m.lockDeadline(deadline); !errors.Is(err, failSessionsLock) {
			t.Fatalf("lockDeadline = %v, want failSessionsLock", err)
		}
		if len(sleeps) != 2 || sleeps[0] != lockRetry || sleeps[1] != 500*time.Microsecond {
			t.Fatalf("sleeps = %v, want [%v, 500µs]", sleeps, lockRetry)
		}

		// Caller deadline smaller than hookLockBudget wins in lockBudgetDeadline.
		m.now = func() time.Time { return clock }
		short := clock.Add(15 * time.Millisecond)
		if got := m.lockBudgetDeadline(short); !got.Equal(short) {
			t.Fatalf("lockBudgetDeadline(short) = %v, want %v", got, short)
		}
		long := clock.Add(time.Second)
		if got := m.lockBudgetDeadline(long); !got.Equal(clock.Add(hookLockBudget)) {
			t.Fatalf("lockBudgetDeadline(long) = %v, want +50ms", got)
		}
	})

	t.Run("rem <= 0 immediately after failed tryLockFile skips sleep", func(t *testing.T) {
		clock := time.Unix(1_000, 0)
		m := fileMemory{
			dir:      filepath.Join(t.TempDir(), "sessions"),
			restrict: config.OwnerOnly,
		}
		holder, err := m.lock(hookLockBudget)
		if err != nil {
			t.Fatal(err)
		}
		defer holder()

		deadline := clock.Add(5 * time.Millisecond)
		calls := 0
		m.mono = func() time.Time {
			calls++
			// Call 5 is deadline.Sub(now()) immediately after first failed tryLockFile.
			if calls >= 5 {
				clock = deadline
			}
			return clock
		}
		m.pause = func(d time.Duration) {
			t.Fatalf("unexpected sleep %v when rem <= 0", d)
		}
		if _, err := m.lockDeadline(deadline); !errors.Is(err, failSessionsLock) {
			t.Fatalf("lockDeadline = %v, want failSessionsLock", err)
		}
	})
}

func TestSingleDeadlineAttentionAndWakeWriters(t *testing.T) {
	t.Run("consumeWake shares one deadline across both lock phases and discards temp after unlock", func(t *testing.T) {
		clock := time.Unix(1_800_000_000, 0)
		m := fileMemory{
			dir:      filepath.Join(t.TempDir(), "sessions"),
			now:      func() time.Time { return clock },
			mono:     func() time.Time { return clock },
			restrict: config.OwnerOnly,
		}
		writeWake(t, m, "codex", "s", `{"version":1,"attempts":[{"id":"a1","kind":"queue","slot":0,"token":"tok","message":"hello"}]}`)

		// Phase 1 lock barrier consumes 40ms of the 50ms budget; phase 2 pause at 50ms must fail
		// without getting a nested 50ms budget!
		step := 0
		m.pause = func(d time.Duration) { clock = clock.Add(d) }
		origRestrict := m.restrict
		m.restrict = func(path string) error {
			step++
			if step == 1 {
				// First restrict call (directory before 2nd lock): consume remaining 10ms past deadline!
				clock = clock.Add(50 * time.Millisecond)
			}
			return origRestrict(path)
		}
		err := m.consumeWakeDeadline("codex", "s", "hello", clock.Add(hookLockBudget))
		if !errors.Is(err, failSessionsLock) {
			t.Fatalf("consumeWakeDeadline past single deadline = %v, want failSessionsLock", err)
		}
		for _, info := range mustList(t, m.dir) {
			if strings.HasPrefix(info.Name(), ".tmp-") {
				t.Fatalf("leftover temp file after failed consumeWake: %s", info.Name())
			}
		}

		// Expiry during phase 1 lock check returns ErrDeadlineExceeded without protecting dir.
		clock = time.Unix(1_800_000_000, 0)
		calls := 0
		m2 := fileMemory{
			dir:  m.dir,
			now:  func() time.Time { return clock },
			mono: func() time.Time { return clock },
			restrict: func(string) error {
				t.Fatal("directory protected after phase 1 deadline expiry")
				return nil
			},
		}
		m2.mono = func() time.Time {
			calls++
			if calls == 6 {
				clock = clock.Add(hookLockBudget)
			}
			return clock
		}
		if err := m2.consumeWakeDeadline("codex", "s", "hello", clock.Add(hookLockBudget)); !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("consumeWake phase 1 expiry = %v, want ErrDeadlineExceeded", err)
		}

		// Directory error and temp-prepare error after phase 1 match.
		clock = time.Unix(1_800_000_000, 0)
		m3 := fileMemory{
			dir:      m.dir,
			now:      func() time.Time { return clock },
			mono:     func() time.Time { return clock },
			restrict: func(string) error { return errors.New("dir err") },
		}
		if err := m3.consumeWakeDeadline("codex", "s", "hello", clock.Add(hookLockBudget)); err == nil {
			t.Fatal("expected directory error")
		}
		restrictCalls := 0
		m3.restrict = func(p string) error {
			restrictCalls++
			if restrictCalls == 2 {
				return errors.New("temp restrict err")
			}
			return config.OwnerOnly(p)
		}
		if err := m3.consumeWakeDeadline("codex", "s", "hello", clock.Add(hookLockBudget)); err == nil {
			t.Fatal("expected temp restrict error")
		}

		// Expiry inside phase 2 lock before readWake.
		clock = time.Unix(1_800_000_000, 0)
		phase2Calls := 0
		var unlockedOnTemp bool
		m4 := fileMemory{
			dir:  m.dir,
			now:  func() time.Time { return clock },
			mono: func() time.Time { return clock },
			restrict: func(p string) error {
				phase2Calls++
				return config.OwnerOnly(p)
			},
		}
		// Advance clock right after phase 2 lock succeeds (mono call 11 at line 741).
		monoCalls := 0
		m4.mono = func() time.Time {
			monoCalls++
			if phase2Calls >= 2 && monoCalls >= 11 {
				clock = clock.Add(hookLockBudget)
			}
			return clock
		}
		_ = unlockedOnTemp
		if err := m4.consumeWakeDeadline("codex", "s", "hello", clock.Add(hookLockBudget)); !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("consumeWake phase 2 expiry = %v, want ErrDeadlineExceeded", err)
		}
		for _, info := range mustList(t, m.dir) {
			if strings.HasPrefix(info.Name(), ".tmp-") {
				t.Fatalf("leftover temp file after phase 2 expiry: %s", info.Name())
			}
		}
	})

	t.Run("updateAttention, toldRevision, forgetAttention respect single deadline and temp cleanup", func(t *testing.T) {
		clock := time.Unix(1_800_000_000, 0)
		m := fileMemory{
			dir:      filepath.Join(t.TempDir(), "sessions"),
			now:      func() time.Time { return clock },
			mono:     func() time.Time { return clock },
			restrict: config.OwnerOnly,
		}
		if err := m.directory(); err != nil {
			t.Fatal(err)
		}
		// Temp restrict error before lock.
		badTemp := m
		badTemp.restrict = func(p string) error {
			if p != m.dir {
				return errors.New("tmp acl")
			}
			return config.OwnerOnly(p)
		}
		if err := badTemp.updateAttentionDeadline("codex", "s", clock.Add(hookLockBudget), func(*toldState) bool { return true }); err == nil {
			t.Fatal("expected temp restrict failure")
		}

		// Locked contention cancels updateAttention before writing.
		holder, err := m.lock(hookLockBudget)
		if err != nil {
			t.Fatal(err)
		}
		mPause := m
		mPause.pause = func(d time.Duration) { clock = clock.Add(d) }
		if err := mPause.updateAttentionDeadline("codex", "s", clock.Add(5*time.Millisecond), func(*toldState) bool {
			t.Fatal("callback ran while lock missed")
			return true
		}); !errors.Is(err, failSessionsLock) {
			holder()
			t.Fatalf("contended updateAttention = %v, want failSessionsLock", err)
		}
		holder()
		for _, info := range mustList(t, m.dir) {
			if strings.HasPrefix(info.Name(), ".tmp-") {
				t.Fatalf("contended updateAttention left temp file: %s", info.Name())
			}
		}

		// Expiry inside updateAttention after lock acquisition.
		clock = time.Unix(1_800_000_000, 0)
		monoCalls := 0
		mExp := m
		mExp.mono = func() time.Time {
			monoCalls++
			if monoCalls == 6 {
				clock = clock.Add(hookLockBudget)
			}
			return clock
		}
		if err := mExp.updateAttentionDeadline("codex", "s", clock.Add(hookLockBudget), func(*toldState) bool { return true }); !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("updateAttention expiry = %v, want ErrDeadlineExceeded", err)
		}

		// Expiry inside replacePreparedDeadline right before writePreparedSessionFile.
		clock = time.Unix(1_800_000_000, 0)
		monoCalls = 0
		mExp2 := m
		mExp2.mono = func() time.Time {
			monoCalls++
			if monoCalls == 7 {
				clock = clock.Add(hookLockBudget)
			}
			return clock
		}
		if err := mExp2.updateAttentionDeadline("codex", "s", clock.Add(hookLockBudget), func(*toldState) bool { return true }); !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("replacePreparedDeadline expiry = %v, want ErrDeadlineExceeded", err)
		}

		// writePreparedSessionFile error when temp vanished.
		if err := writePreparedSessionFile(filepath.Join(m.dir, "nonexistent-tmp"), "x"); err == nil {
			t.Fatal("expected error writing missing temp file")
		}
		tmp, err := prepareSessionFile(m.dir, "seed", config.OwnerOnly)
		if err != nil {
			t.Fatal(err)
		}
		_ = os.Remove(tmp)
		if err := m.replacePreparedDeadline(&tmp, m.counts("codex", "s"), "1 0 0 0", clock.Add(hookLockBudget), false); err == nil {
			t.Fatal("expected replacePreparedDeadline error on missing temp")
		}

		// toldRevision and forgetAttention when lock is held past short deadline.
		holder, err = m.lock(hookLockBudget)
		if err != nil {
			t.Fatal(err)
		}
		defer holder()
		clock = time.Unix(1_800_000_000, 0)
		if rev, err := mPause.toldRevisionDeadline("codex", "s", clock.Add(5*time.Millisecond)); !errors.Is(err, failSessionsLock) || rev != fromReplay {
			t.Fatalf("toldRevision = (%d, %v), want (%d, failSessionsLock)", rev, err, fromReplay)
		}
		if err := mPause.forgetAttentionDeadline("codex", "s", clock.Add(5*time.Millisecond)); !errors.Is(err, failSessionsLock) {
			t.Fatalf("forgetAttention = %v, want failSessionsLock", err)
		}
	})
}

func TestDeadlineChecksInEvidenceWritersAndScanners(t *testing.T) {
	t.Run("readGenerationDeadline and readEndMarkerDeadline Lstat expiry", func(t *testing.T) {
		dir := t.TempDir()
		missing := filepath.Join(dir, "missing")
		clock := time.Unix(1_800_000_000, 0)
		deadline := clock.Add(10 * time.Millisecond)
		origOpen := generationFileOpen
		t.Cleanup(func() { generationFileOpen = origOpen })
		generationFileOpen = func(path string) (*os.File, error) {
			clock = deadline
			return nil, os.ErrNotExist
		}
		if _, err := readGenerationDeadline(missing, func() time.Time { return clock }, deadline); !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("readGenerationDeadline Lstat expiry = %v, want ErrDeadlineExceeded", err)
		}

		generationFileOpen = origOpen
		marker := filepath.Join(dir, "m.ended")
		if err := os.WriteFile(marker, []byte("2\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		clock = time.Unix(1_800_000_000, 0)
		deadline = clock.Add(10 * time.Millisecond)
		calls := 0
		m := fileMemory{
			dir: dir,
			now: func() time.Time { return clock },
			mono: func() time.Time {
				calls++
				// After readGenerationDeadline succeeds (3 checks), expire before os.Lstat.
				if calls == 4 {
					clock = deadline
				}
				return clock
			},
			restrict: config.OwnerOnly,
		}
		if _, _, err := m.readEndMarkerDeadline(marker, deadline); !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("readEndMarkerDeadline Lstat expiry = %v, want ErrDeadlineExceeded", err)
		}
	})

	t.Run("recallGenerationDeadline expiry on ended read", func(t *testing.T) {
		clock := time.Unix(1_800_000_000, 0)
		m := fileMemory{dir: filepath.Join(t.TempDir(), "sessions"), now: func() time.Time { return clock }, restrict: config.OwnerOnly}
		m.Remember("claude", "s", 3)
		calls := 0
		m.mono = func() time.Time {
			calls++
			// Expire right when reading .ended inside underLockDeadline.
			if calls == 12 {
				clock = clock.Add(hookLockBudget)
			}
			return clock
		}
		if _, err := m.recallGenerationDeadline("claude", "s", clock.Add(hookLockBudget)); !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("recallGenerationDeadline ended expiry = %v, want ErrDeadlineExceeded", err)
		}
	})

	t.Run("rememberDeadline and forgetDeadline and pruneDeadline expiry", func(t *testing.T) {
		clock := time.Unix(1_800_000_000, 0)
		m := fileMemory{dir: filepath.Join(t.TempDir(), "sessions"), now: func() time.Time { return clock }, restrict: config.OwnerOnly}
		m.Remember("claude", "s", 0) // early return on generation <= 0
		m.Remember("claude", "s", 2)
		marker := m.name("claude", "s") + ".ended"
		if err := os.WriteFile(marker, []byte("2\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		old := clock.Add(-memoryAge - time.Hour)
		if err := os.Chtimes(marker, old, old); err != nil {
			t.Fatal(err)
		}
		// Expire during readEndMarkerDeadline inside rememberDeadline.
		calls := 0
		mExp := m
		mExp.mono = func() time.Time {
			calls++
			if calls == 9 {
				clock = clock.Add(hookLockBudget)
			}
			return clock
		}
		mExp.rememberDeadline("claude", "s", 4, clock.Add(hookLockBudget))
		if m.Recall("claude", "s") != 2 {
			t.Fatalf("rememberDeadline modified proof past deadline")
		}

		// Expire right before removing current in expired rememberDeadline.
		clock = time.Unix(1_800_000_000, 0)
		calls = 0
		mExp2 := m
		mExp2.mono = func() time.Time {
			calls++
			if calls == 13 {
				clock = clock.Add(hookLockBudget)
			}
			return clock
		}
		mExp2.rememberDeadline("claude", "s", 4, clock.Add(hookLockBudget))
		if m.Recall("claude", "s") != 2 {
			t.Fatalf("rememberDeadline removed proof past deadline")
		}

		// Expire right before removing marker in expired rememberDeadline.
		clock = time.Unix(1_800_000_000, 0)
		mClean := fileMemory{dir: filepath.Join(t.TempDir(), "sessions"), now: func() time.Time { return clock }, restrict: config.OwnerOnly}
		if err := mClean.directory(); err != nil {
			t.Fatal(err)
		}
		mk2 := mClean.name("claude", "s") + ".ended"
		if err := os.WriteFile(mk2, []byte("2\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(mk2, old, old); err != nil {
			t.Fatal(err)
		}
		calls = 0
		mClean.mono = func() time.Time {
			calls++
			if calls == 12 {
				clock = clock.Add(hookLockBudget)
			}
			return clock
		}
		mClean.rememberDeadline("claude", "s", 4, clock.Add(hookLockBudget))
		if _, err := os.Lstat(mk2); err != nil {
			t.Fatalf("rememberDeadline removed marker past deadline: %v", err)
		}

		// forgetDeadline error on dir under file, and expiry before floor write.
		fileParent := filepath.Join(t.TempDir(), "file")
		if err := os.WriteFile(fileParent, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		badM := fileMemory{dir: filepath.Join(fileParent, "sessions"), restrict: config.OwnerOnly}
		if got := badM.forget("claude", "s", 2); got != badM.name("claude", "s")+".ended" {
			t.Fatalf("forget on bad dir = %q", got)
		}
		badTempForget := fileMemory{
			dir: mClean.dir,
			restrict: func(p string) error {
				if p != mClean.dir {
					return errors.New("temp restrict")
				}
				return config.OwnerOnly(p)
			},
		}
		if got := badTempForget.forget("claude", "s", 2); got != badTempForget.name("claude", "s")+".ended" {
			t.Fatalf("forget on temp restrict err = %q", got)
		}

		clock = time.Unix(1_800_000_000, 0)
		mForget := fileMemory{dir: filepath.Join(t.TempDir(), "sessions"), now: func() time.Time { return clock }, restrict: config.OwnerOnly}
		mForget.Remember("claude", "s", 3)
		// Expire right before writing .ended: forgetDeadline fails and preserves existing proof.
		calls = 0
		mForgetExp := mForget
		mForgetExp.mono = func() time.Time {
			calls++
			if calls == 8 {
				clock = clock.Add(hookLockBudget)
			}
			return clock
		}
		if got := mForgetExp.forgetDeadline("claude", "s", 3, clock.Add(hookLockBudget)); got == "" {
			t.Fatal("expected forgetDeadline to fail when deadline expired before .ended replace")
		}
		if mForget.Recall("claude", "s") != 3 {
			t.Fatal("forgetDeadline deleted proof even though .ended replace failed")
		}
		clock = time.Unix(1_800_000_000, 0)
		if got := mForget.forget("claude", "s", 3); got != "" {
			t.Fatal(got)
		}
		// Now marker=3; calling forget(1) with expiry before writePreparedSessionFile keeps marker=3.
		calls = 0
		mForget.mono = func() time.Time {
			calls++
			if calls == 9 {
				clock = clock.Add(hookLockBudget)
			}
			return clock
		}
		if got := mForget.forgetDeadline("claude", "s", 1, clock.Add(hookLockBudget)); got == "" {
			t.Fatal("expected forgetDeadline to fail on expired floor write")
		}

		// pruneDeadline expiry inside stale removal loop.
		clock = time.Unix(1_800_000_000, 0)
		mPrune := fileMemory{dir: filepath.Join(t.TempDir(), "sessions"), now: func() time.Time { return clock }, restrict: config.OwnerOnly}
		mPrune.Remember("claude", "old", 1)
		if err := os.Chtimes(mPrune.name("claude", "old"), old, old); err != nil {
			t.Fatal(err)
		}
		calls = 0
		mPrune.mono = func() time.Time {
			calls++
			if calls == 6 {
				clock = clock.Add(hookLockBudget)
			}
			return clock
		}
		if err := mPrune.pruneDeadline(clock.Add(hookLockBudget)); err != nil {
			t.Fatal(err)
		}
		if mPrune.Recall("claude", "old") != 1 {
			t.Fatal("prune removed entry after deadline expired")
		}
	})

	t.Run("replaceSessionFileDeadline and Antigravity deadline expiry", func(t *testing.T) {
		if runtime.GOOS != "windows" {
			dir := t.TempDir()
			src := filepath.Join(dir, "src")
			dst := filepath.Join(dir, "dst")
			if err := os.WriteFile(src, []byte("a"), 0o600); err != nil {
				t.Fatal(err)
			}
			now := time.Unix(1_000, 0)
			if err := replaceSessionFileDeadline(src, dst, now, false, func() time.Time { return now }, nil); !errors.Is(err, os.ErrDeadlineExceeded) {
				t.Fatalf("replaceSessionFileDeadline expired = %v, want ErrDeadlineExceeded", err)
			}
		}

		// Antigravity writeAntigravityJSONDeadline expired when renameAntigravityStateFile is hooked.
		dir := t.TempDir()
		origRename := renameAntigravityStateFile
		t.Cleanup(func() { renameAntigravityStateFile = origRename })
		renameAntigravityStateFile = func(string, string) error {
			t.Fatal("renameAntigravityStateFile ran past deadline")
			return nil
		}
		now := time.Unix(1_000, 0)
		if err := writeAntigravityJSONDeadline(dir, filepath.Join(dir, "out.json"), antigravityProcState{}, now, func() time.Time { return now }); !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("writeAntigravityJSONDeadline expired = %v, want ErrDeadlineExceeded", err)
		}

		// ScanAntigravityWakeSession temp-prepare error and post-lock deadline expiry.
		_, tokenPath, _ := agyTestEnv(t)
		tDir := t.TempDir()
		tr := filepath.Join(tDir, "t.jsonl")
		if err := os.WriteFile(tr, []byte(`{"step_index":1}`+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		const convID = "e28a6164-6409-4810-b8ee-df8202a16a0b"
		s := NewSessions(tokenPath)
		writeWakeTestDoc(t, s.WakePath("antigravity", convID), []map[string]any{{
			"id":      "att-1",
			"message": "msg",
			"scan":    map[string]any{"transcript_path": tr},
		}})
		s.memory.restrict = func(p string) error {
			if p != s.Dir() {
				return errors.New("tmp acl fail")
			}
			return config.OwnerOnly(p)
		}
		if _, _, err := ScanAntigravityWakeSession(s, convID, tr, time.Now(), time.Second); err == nil {
			t.Fatal("expected ScanAntigravityWakeSession prepare temp error")
		}

		clock := time.Unix(1_800_000_000, 0)
		calls := 0
		s.memory.restrict = config.OwnerOnly
		s.memory.mono = func() time.Time {
			calls++
			if calls >= 6 {
				clock = clock.Add(2 * time.Second)
			}
			return clock
		}
		if _, _, err := ScanAntigravityWakeSession(s, convID, tr, time.Now(), time.Second); !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("ScanAntigravityWakeSession expired = %v, want ErrDeadlineExceeded", err)
		}
	})
}

func testExecutionID(hexDigit string) string {
	return "ex_" + strings.Repeat(hexDigit, 32)
}

// writeTestExecutionRecord writes <hash>.execution strictly as a test helper;
// M2b never writes .execution in production paths.
func writeTestExecutionRecord(t *testing.T, m fileMemory, client, session string, rec sessionproof.Record) string {
	t.Helper()
	if err := m.directory(); err != nil {
		t.Fatal(err)
	}
	raw, err := sessionproof.EncodeRecord(rec)
	if err != nil {
		t.Fatal(err)
	}
	execPath := executionPath(m.name(client, session))
	if err := os.WriteFile(execPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return execPath
}

func denyExecutionOpen(t *testing.T, forbiddenPath string) {
	t.Helper()
	orig := generationFileOpen
	t.Cleanup(func() { generationFileOpen = orig })
	generationFileOpen = func(path string) (*os.File, error) {
		if path == forbiddenPath {
			t.Fatalf("OFF mode opened %s; .execution content must never be opened or parsed under OFF", path)
		}
		return orig(path)
	}
}

func TestEndRetirementLifecycleEnablePublishDisableExpireCleanReenable(t *testing.T) {
	clock := time.Unix(1_800_000_000, 0)
	token := filepath.Join(t.TempDir(), "hub.token")
	s := NewSessions(token)
	s.memory.now = func() time.Time { return clock }
	s.memory.mono = time.Now

	const (
		client  = "claude"
		session = "sess-lifecycle"
	)
	prefix := s.memory.name(client, session)
	execPath := executionPath(prefix)
	marker := prefix + ".ended"

	// Enable -> publish (Remember + test-helper .execution).
	s.memory.rememberWithSuccession(client, session, 2, time.Time{}, true)
	rec := sessionproof.Record{
		Version:     sessionproof.FormatVersion,
		Origin:      sessionproof.OriginHook,
		Generation:  2,
		ExecutionID: testExecutionID("a"),
	}
	writeTestExecutionRecord(t, s.memory, client, session, rec)

	snap, err := s.InspectEvidence(client, session, time.Time{}, true, false)
	if err != nil {
		t.Fatalf("InspectEvidence ON = %v", err)
	}
	if snap.GenerationState != sessionproof.FileValid || snap.Generation != 2 ||
		snap.RecordState != sessionproof.FileValid || snap.Record != rec ||
		snap.EndedState != sessionproof.FileAbsent {
		t.Fatalf("unexpected initial ON snapshot: %+v", snap)
	}

	// Simulate an ended state leaving both <hash> and .execution present beside .ended=2.
	if err := os.WriteFile(marker, []byte("2\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Disable (OFF): inspect does not open .execution and reports FileSkipped.
	t.Run("disable OFF skips execution open and cleans on expire", func(t *testing.T) {
		denyExecutionOpen(t, execPath)

		offSnap, err := s.InspectEvidence(client, session, time.Time{}, false, false)
		if err != nil || offSnap.RecordState != sessionproof.FileSkipped || offSnap.Ended != 2 {
			t.Fatalf("InspectEvidence OFF pre-expiry = (%+v, %v)", offSnap, err)
		}

		// Expire past memoryAge.
		clock = clock.Add(memoryAge + time.Hour)

		// Failure while deleting <hash>.execution MUST keep .ended effective!
		origRemove := removeSessionFile
		t.Cleanup(func() { removeSessionFile = origRemove })
		boom := errors.New("busy execution file")
		removeSessionFile = func(path string) error {
			if path == execPath {
				return boom
			}
			return origRemove(path)
		}

		_, err = s.InspectEvidence(client, session, time.Time{}, false, true)
		if err != nil {
			t.Fatalf("InspectEvidence OFF on cleanup failure returned error %v", err)
		}
		if _, err := os.Lstat(marker); err != nil {
			t.Fatalf(".ended was deleted despite .execution cleanup failure: %v", err)
		}
		s.memory.rememberWithSuccession(client, session, 2, time.Time{}, false)
		if g := s.memory.Recall(client, session); g != 0 {
			t.Fatalf("Recall returned G%d despite failed cleanup keeping .ended", g)
		}
		if _, ended, berr := s.RecallBindingGeneration(client, session); berr != nil || ended != 2 {
			t.Fatalf("veto broken: ended=%d berr=%v", ended, berr)
		}

		// Allow cleanup to finish: retires .execution via Lstat/Remove and removes .ended LAST.
		removeSessionFile = origRemove
		var removalOrder []string
		removeSessionFile = func(path string) error {
			removalOrder = append(removalOrder, filepath.Base(path))
			return origRemove(path)
		}
		cleanedSnap, err := s.InspectEvidence(client, session, time.Time{}, false, true)
		if err != nil {
			t.Fatalf("InspectEvidence OFF clean = %v", err)
		}
		if cleanedSnap.GenerationState != sessionproof.FileAbsent ||
			cleanedSnap.RecordState != sessionproof.FileSkipped ||
			cleanedSnap.EndedState != sessionproof.FileAbsent {
			t.Fatalf("unexpected cleaned OFF snapshot: %+v", cleanedSnap)
		}
		if len(removalOrder) == 0 || removalOrder[len(removalOrder)-1] != filepath.Base(marker) {
			t.Fatalf(".ended was not deleted LAST; order=%v", removalOrder)
		}
	})

	// Re-enable (ON): confirm everything is clean and absent, then publish G=3.
	reSnap, err := s.InspectEvidence(client, session, time.Time{}, true, false)
	if err != nil {
		t.Fatalf("InspectEvidence re-enabled = %v", err)
	}
	if reSnap.GenerationState != sessionproof.FileAbsent ||
		reSnap.RecordState != sessionproof.FileAbsent ||
		reSnap.EndedState != sessionproof.FileAbsent {
		t.Fatalf("re-enabled snapshot not clean: %+v", reSnap)
	}
	s.memory.rememberWithSuccession(client, session, 3, time.Time{}, true)
	rec3 := sessionproof.Record{
		Version:     sessionproof.FormatVersion,
		Origin:      sessionproof.OriginHook,
		Generation:  3,
		ExecutionID: testExecutionID("b"),
	}
	writeTestExecutionRecord(t, s.memory, client, session, rec3)
	finalSnap, err := s.InspectEvidence(client, session, time.Time{}, true, false)
	if err != nil || finalSnap.Generation != 3 || finalSnap.Record != rec3 {
		t.Fatalf("re-enabled publish snapshot = (%+v, %v)", finalSnap, err)
	}
}

func TestEndRetirementAbsentPermutationsAndCleanupFailures(t *testing.T) {
	for _, tc := range []struct {
		name              string
		hashGen           int
		execRec           *sessionproof.Record
		corruptExec       bool
		endedGen          int
		successionEnabled bool
		wantCleaned       bool
		wantHashKept      bool
		wantExecKept      bool
	}{
		{
			name:              "both hash and execution absent under OFF",
			endedGen:          2,
			successionEnabled: false,
			wantCleaned:       true,
		},
		{
			name:              "both hash and execution absent under ON",
			endedGen:          2,
			successionEnabled: true,
			wantCleaned:       true,
		},
		{
			name:              "only hash present under ON",
			hashGen:           2,
			endedGen:          2,
			successionEnabled: true,
			wantCleaned:       true,
		},
		{
			name:              "only execution present under OFF",
			execRec:           &sessionproof.Record{Version: 1, Origin: sessionproof.OriginHook, Generation: 2, ExecutionID: testExecutionID("1")},
			endedGen:          2,
			successionEnabled: false,
			wantCleaned:       true,
		},
		{
			name:              "only execution present under ON",
			execRec:           &sessionproof.Record{Version: 1, Origin: sessionproof.OriginHook, Generation: 2, ExecutionID: testExecutionID("2")},
			endedGen:          2,
			successionEnabled: true,
			wantCleaned:       true,
		},
		{
			name:              "shim prior<=ended retired under ON",
			hashGen:           3,
			execRec:           &sessionproof.Record{Version: 1, Origin: sessionproof.OriginShim, Generation: 3, ExecutionID: testExecutionID("3"), PriorGeneration: 2, PriorExecutionID: testExecutionID("2")},
			endedGen:          2,
			successionEnabled: true,
			wantCleaned:       true,
			wantHashKept:      true,
		},
		{
			name:              "newer hook execution kept under ON while ended retires",
			hashGen:           4,
			execRec:           &sessionproof.Record{Version: 1, Origin: sessionproof.OriginHook, Generation: 4, ExecutionID: testExecutionID("4")},
			endedGen:          2,
			successionEnabled: true,
			wantCleaned:       true,
			wantHashKept:      true,
			wantExecKept:      true,
		},
		{
			name:              "newer hash protects unknown execution under OFF",
			hashGen:           4,
			corruptExec:       true,
			endedGen:          2,
			successionEnabled: false,
			wantCleaned:       true,
			wantHashKept:      true,
			wantExecKept:      true,
		},
		{
			name:              "corrupt execution retired by name when hash absent under OFF",
			corruptExec:       true,
			endedGen:          2,
			successionEnabled: false,
			wantCleaned:       true,
		},
		{
			name:              "corrupt execution blocks end retirement under ON",
			hashGen:           2,
			corruptExec:       true,
			endedGen:          2,
			successionEnabled: true,
			wantCleaned:       false,
			wantHashKept:      true,
			wantExecKept:      true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock := time.Unix(1_800_000_000, 0)
			m := fileMemory{dir: filepath.Join(t.TempDir(), "sessions"), now: func() time.Time { return clock }, restrict: config.OwnerOnly}
			if err := m.directory(); err != nil {
				t.Fatal(err)
			}
			prefix := m.name("claude", "s")
			execPath := executionPath(prefix)
			marker := prefix + ".ended"
			if tc.hashGen > 0 {
				if err := os.WriteFile(prefix, []byte(strings.Join([]string{"", ""}, "")), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := writePreparedSessionFile(prefix, strings.TrimSpace(string(rune('0'+tc.hashGen)))); err != nil {
					t.Fatal(err)
				}
			}
			if tc.execRec != nil {
				writeTestExecutionRecord(t, m, "claude", "s", *tc.execRec)
			}
			if tc.corruptExec {
				if err := os.WriteFile(execPath, []byte("{corrupt"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(marker, []byte(string(rune('0'+tc.endedGen))), 0o600); err != nil {
				t.Fatal(err)
			}
			if !tc.successionEnabled {
				denyExecutionOpen(t, execPath)
			}

			cleaned, _ := m.cleanupExpiredEnd(prefix, tc.endedGen, clock.Add(hookLockBudget), tc.successionEnabled)
			if cleaned != tc.wantCleaned {
				t.Fatalf("cleaned = %v, want %v", cleaned, tc.wantCleaned)
			}
			_, markerErr := os.Lstat(marker)
			if tc.wantCleaned && !errors.Is(markerErr, os.ErrNotExist) {
				t.Fatalf(".ended survived cleanup: %v", markerErr)
			}
			if !tc.wantCleaned && markerErr != nil {
				t.Fatalf(".ended vanished when cleanup refused: %v", markerErr)
			}
			_, hashErr := os.Lstat(prefix)
			if tc.wantHashKept && hashErr != nil {
				t.Fatalf("<hash> missing when expected kept: %v", hashErr)
			}
			if !tc.wantHashKept && !errors.Is(hashErr, os.ErrNotExist) {
				t.Fatalf("<hash> survived cleanup: %v", hashErr)
			}
			_, execErr := os.Lstat(execPath)
			if tc.wantExecKept && execErr != nil {
				t.Fatalf(".execution missing when expected kept: %v", execErr)
			}
			if !tc.wantExecKept && !errors.Is(execErr, os.ErrNotExist) {
				t.Fatalf(".execution survived cleanup: %v", execErr)
			}
		})
	}

	t.Run("cleanup failures on hash, lstat, execution, and marker preserve .ended and order", func(t *testing.T) {
		clock := time.Unix(1_800_000_000, 0)
		m := fileMemory{dir: filepath.Join(t.TempDir(), "sessions"), now: func() time.Time { return clock }, mono: func() time.Time { return clock }, restrict: config.OwnerOnly}
		if err := m.directory(); err != nil {
			t.Fatal(err)
		}
		prefix := m.name("claude", "fail")
		execPath := executionPath(prefix)
		marker := prefix + ".ended"
		reset := func() {
			_ = os.WriteFile(prefix, []byte("2"), 0o600)
			_ = os.WriteFile(execPath, []byte("{bad"), 0o600)
			_ = os.WriteFile(marker, []byte("2"), 0o600)
			old := clock.Add(-memoryAge - time.Hour)
			_ = os.Chtimes(marker, old, old)
		}

		// Damaged <hash> rejects cleanupExpiredEnd.
		_ = os.WriteFile(prefix, []byte("bad"), 0o600)
		_ = os.WriteFile(marker, []byte("2"), 0o600)
		if ok, err := m.cleanupExpiredEnd(prefix, 2, clock.Add(hookLockBudget), false); ok || err == nil {
			t.Fatalf("expected damaged <hash> cleanup failure, got (%v, %v)", ok, err)
		}
		if _, err := os.Lstat(marker); err != nil {
			t.Fatalf(".ended removed despite damaged <hash>: %v", err)
		}

		// Non-ENOENT lstatSessionFile error on .execution keeps .ended.
		reset()
		origLstat := lstatSessionFile
		t.Cleanup(func() { lstatSessionFile = origLstat })
		lstatSessionFile = func(p string) (os.FileInfo, error) {
			if p == execPath {
				return nil, os.ErrPermission
			}
			return origLstat(p)
		}
		if ok, err := m.cleanupExpiredEnd(prefix, 2, clock.Add(hookLockBudget), false); ok || !errors.Is(err, os.ErrPermission) {
			t.Fatalf("lstat error = (%v, %v), want false/ErrPermission", ok, err)
		}
		if _, err := os.Lstat(marker); err != nil {
			t.Fatalf(".ended removed after lstat error: %v", err)
		}
		lstatSessionFile = origLstat

		// removeSessionFile fails on <hash>, then <hash>.ended; each keeps .ended intact.
		origRemove := removeSessionFile
		t.Cleanup(func() { removeSessionFile = origRemove })
		for _, failPath := range []string{prefix, marker} {
			reset()
			removeSessionFile = func(p string) error {
				if p == failPath {
					return errors.New("unlink failed")
				}
				return origRemove(p)
			}
			if ok, err := m.cleanupExpiredEnd(prefix, 2, clock.Add(hookLockBudget), false); ok || err == nil {
				t.Fatalf("remove fail on %s returned (%v, %v)", failPath, ok, err)
			}
			if _, err := os.Lstat(marker); err != nil {
				t.Fatalf(".ended vanished after remove error on %s: %v", failPath, err)
			}
		}
		removeSessionFile = origRemove

		// rememberWithSuccession refuses to overwrite expired .ended when cleanupExpiredEnd fails.
		reset()
		removeSessionFile = func(p string) error {
			if p == execPath {
				return errors.New("keep marker")
			}
			return origRemove(p)
		}
		m.rememberWithSuccession("claude", "fail", 1, clock.Add(hookLockBudget), false)
		if _, err := os.Lstat(marker); err != nil {
			t.Fatalf("rememberWithSuccession dropped .ended when cleanup failed: %v", err)
		}
		if g := m.Recall("claude", "fail"); g != 0 {
			t.Fatalf("rememberWithSuccession revived generation G%d despite cleanup failure", g)
		}
		removeSessionFile = origRemove

		// Every deadline check inside cleanupExpiredEnd.
		deadline := clock.Add(hookLockBudget)
		// 1) Before OFF lstat check: 4 calls (readGenerationDeadline on prefix takes 3 checks).
		reset()
		calls := 0
		mExp := m
		mExp.mono = func() time.Time {
			calls++
			if calls == 4 {
				return deadline
			}
			return clock
		}
		if ok, err := mExp.cleanupExpiredEnd(prefix, 2, deadline, false); ok || !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("OFF lstat deadline = (%v, %v)", ok, err)
		}
		// 2) Before ON readExactRecordDeadline: call 4.
		calls = 0
		writeTestExecutionRecord(t, m, "claude", "fail", sessionproof.Record{Version: 1, Origin: sessionproof.OriginHook, Generation: 2, ExecutionID: testExecutionID("c")})
		if ok, err := mExp.cleanupExpiredEnd(prefix, 2, deadline, true); ok || !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("ON readExactRecord deadline = (%v, %v)", ok, err)
		}
		// 3) Before removeSessionFile(prefix): call 5 under OFF.
		calls = 0
		mExp.mono = func() time.Time {
			calls++
			if calls == 5 {
				return deadline
			}
			return clock
		}
		if ok, err := mExp.cleanupExpiredEnd(prefix, 2, deadline, false); ok || !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("removeHash deadline = (%v, %v)", ok, err)
		}
		// 4) Before removeSessionFile(execPath): call 6 under OFF.
		reset()
		calls = 0
		mExp.mono = func() time.Time {
			calls++
			if calls == 6 {
				return deadline
			}
			return clock
		}
		if ok, err := mExp.cleanupExpiredEnd(prefix, 2, deadline, false); ok || !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("removeExec deadline = (%v, %v)", ok, err)
		}
		// 5) Before removeSessionFile(marker): call 7 under OFF.
		reset()
		calls = 0
		mExp.mono = func() time.Time {
			calls++
			if calls == 7 {
				return deadline
			}
			return clock
		}
		if ok, err := mExp.cleanupExpiredEnd(prefix, 2, deadline, false); ok || !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("removeMarker deadline = (%v, %v)", ok, err)
		}
	})
}

func TestOFFImmunityAndIndependentInspectAndForgetAndPrune(t *testing.T) {
	t.Run("readExactRecordDeadline states and deadline expiry after ErrNotExist", func(t *testing.T) {
		dir := t.TempDir()
		absent := filepath.Join(dir, "absent.execution")
		clock := time.Unix(1_800_000_000, 0)
		deadline := clock.Add(10 * time.Millisecond)

		origOpen := generationFileOpen
		t.Cleanup(func() { generationFileOpen = origOpen })
		generationFileOpen = func(string) (*os.File, error) {
			clock = deadline
			return nil, os.ErrNotExist
		}
		if _, _, err := readExactRecordDeadline(absent, func() time.Time { return clock }, deadline); !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("readExactRecordDeadline expired after ErrNotExist = %v", err)
		}
		generationFileOpen = origOpen

		// Directory at .execution is damaged, not absent.
		dirPath := filepath.Join(dir, "dir.execution")
		if err := os.MkdirAll(dirPath, 0o700); err != nil {
			t.Fatal(err)
		}
		if _, st, err := readExactRecordDeadline(dirPath, nil, time.Time{}); st != sessionproof.FileDamaged || !errors.Is(err, sessionproof.ErrInvalidRecord) {
			t.Fatalf("dir execution = (%v, %v), want FileDamaged/ErrInvalidRecord", st, err)
		}

		if runtime.GOOS != "windows" {
			dangling := filepath.Join(dir, "dangling.execution")
			if err := os.Symlink(filepath.Join(dir, "missing-target"), dangling); err != nil {
				t.Fatal(err)
			}
			if _, st, err := readExactRecordDeadline(dangling, nil, time.Time{}); st != sessionproof.FileDamaged || !errors.Is(err, sessionproof.ErrInvalidRecord) {
				t.Fatalf("dangling symlink = (%v, %v), want FileDamaged/ErrInvalidRecord", st, err)
			}
		}
	})

	t.Run("InspectEvidence independent reads, absent dir, OFF immunity, and deadline exits", func(t *testing.T) {
		token := filepath.Join(t.TempDir(), "hub.token")
		s := NewSessions(token)
		// Empty client/session and absent sessions dir have no side effects.
		if snap, err := s.InspectEvidence("", "s", time.Time{}, true, true); err != nil || snap != (sessionproof.Snapshot{}) {
			t.Fatalf("empty client snapshot = (%+v, %v)", snap, err)
		}
		for _, on := range []bool{false, true} {
			snap, err := s.InspectEvidence("claude", "s", time.Time{}, on, true)
			if err != nil {
				t.Fatalf("absent dir inspect(on=%v) = %v", on, err)
			}
			wantRec := sessionproof.FileSkipped
			if on {
				wantRec = sessionproof.FileAbsent
			}
			if snap.GenerationState != sessionproof.FileAbsent || snap.EndedState != sessionproof.FileAbsent || snap.RecordState != wantRec {
				t.Fatalf("absent dir snap(on=%v) = %+v", on, snap)
			}
			if _, statErr := os.Lstat(s.Dir()); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("InspectEvidence created sessions dir on absent session: %v", statErr)
			}
		}
		// Already-expired deadline fails even on absent dir.
		past := time.Now().Add(-time.Second)
		if _, err := s.InspectEvidence("claude", "s", past, false, false); !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("InspectEvidence expired = %v, want ErrDeadlineExceeded", err)
		}

		// Bad dir path returns lock error cleanly.
		fileParent := filepath.Join(t.TempDir(), "file")
		if err := os.WriteFile(fileParent, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		badS := NewSessions(filepath.Join(fileParent, "sessions", "hub.token"))
		if _, err := badS.InspectEvidence("claude", "s", time.Time{}, false, false); err == nil {
			t.Fatal("expected InspectEvidence error when sessions dir parent is a file")
		}

		// Damaged <hash> must not hide valid .ended (or valid .execution under ON).
		clock := time.Unix(1_800_000_000, 0)
		s.memory.now = func() time.Time { return clock }
		s.memory.mono = func() time.Time { return clock }
		if err := s.Protect(); err != nil {
			t.Fatal(err)
		}
		prefix := s.memory.name("claude", "independent")
		if err := os.WriteFile(prefix, []byte("not-a-gen"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(prefix+".ended", []byte("3\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		rec3 := sessionproof.Record{Version: 1, Origin: sessionproof.OriginHook, Generation: 3, ExecutionID: testExecutionID("d")}
		writeTestExecutionRecord(t, s.memory, "claude", "independent", rec3)

		snap, err := s.InspectEvidence("claude", "independent", clock.Add(hookLockBudget), true, false)
		if err != nil {
			t.Fatalf("InspectEvidence damaged hash = %v", err)
		}
		if snap.GenerationState != sessionproof.FileDamaged ||
			snap.EndedState != sessionproof.FileValid || snap.Ended != 3 ||
			snap.RecordState != sessionproof.FileValid || snap.Record != rec3 {
			t.Fatalf("damaged hash hid other states: %+v", snap)
		}

		// Damaged .ended must not hide valid <hash>, and damaged .execution must not hide valid .ended.
		if err := os.WriteFile(prefix, []byte("3\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(executionPath(prefix), []byte(`{"version":1}`), 0o600); err != nil {
			t.Fatal(err)
		}
		snap, err = s.InspectEvidence("claude", "independent", clock.Add(hookLockBudget), true, false)
		if err != nil {
			t.Fatalf("InspectEvidence damaged execution = %v", err)
		}
		if snap.GenerationState != sessionproof.FileValid || snap.Generation != 3 ||
			snap.RecordState != sessionproof.FileDamaged ||
			snap.EndedState != sessionproof.FileValid || snap.Ended != 3 {
			t.Fatalf("damaged execution hid states: %+v", snap)
		}
		if err := os.WriteFile(prefix+".ended", []byte("bad\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		snap, err = s.InspectEvidence("claude", "independent", clock.Add(hookLockBudget), true, false)
		if err != nil || snap.EndedState != sessionproof.FileDamaged || snap.Generation != 3 {
			t.Fatalf("damaged .ended snapshot = (%+v, %v)", snap, err)
		}

		// Internal deadline checks inside inspectEvidenceDeadline (under lock).
		prefix2 := s.memory.name("claude", "deadline-steps")
		old := clock.Add(-memoryAge - time.Hour)
		writeExpired := func() {
			_ = os.WriteFile(prefix2, []byte("2\n"), 0o600)
			writeTestExecutionRecord(t, s.memory, "claude", "deadline-steps", sessionproof.Record{Version: 1, Origin: sessionproof.OriginHook, Generation: 2, ExecutionID: testExecutionID("e")})
			_ = os.WriteFile(prefix2+".ended", []byte("2\n"), 0o600)
			_ = os.Chtimes(prefix2+".ended", old, old)
		}
		deadline := clock.Add(hookLockBudget)
		// 1) Initial readEndMarkerDeadline expiry (call 6 inside underLockDeadline).
		// 2) cleanupExpiredEnd expiry (call 10).
		// 3) Re-read marker expiry after cleanupExpiredEnd (call 18).
		// 4) readGenerationDeadline expiry (call 11 when cleanExpired=false).
		// 5) readExactRecordDeadline expiry (call 14 when cleanExpired=false).
		for _, tc := range []struct {
			name         string
			cleanExpired bool
			expireAtCall int
		}{
			{"initial marker", true, 7},
			{"during cleanup", true, 11},
			{"post-cleanup reread", true, 20},
			{"generation read", false, 11},
			{"execution read", false, 14},
		} {
			writeExpired()
			calls := 0
			m := s.memory
			m.mono = func() time.Time {
				calls++
				if calls == tc.expireAtCall {
					return deadline
				}
				return clock
			}
			if _, err := m.inspectEvidenceDeadline("claude", "deadline-steps", deadline, true, tc.cleanExpired); !errors.Is(err, os.ErrDeadlineExceeded) {
				t.Fatalf("%s inspect expiry = %v, want ErrDeadlineExceeded (calls=%d)", tc.name, err, calls)
			}
		}
	})

	t.Run("forgetEvidence and pruneWithSuccession ON and OFF", func(t *testing.T) {
		clock := time.Now()
		m := fileMemory{dir: filepath.Join(t.TempDir(), "sessions"), now: func() time.Time { return clock }, mono: func() time.Time { return clock }, restrict: config.OwnerOnly}

		// OFF: forgetEvidence cleans <= floor .execution by name without opening, keeps > floor.
		m.Remember("claude", "off-end", 2)
		execOff := writeTestExecutionRecord(t, m, "claude", "off-end", sessionproof.Record{Version: 1, Origin: sessionproof.OriginHook, Generation: 2, ExecutionID: testExecutionID("1")})
		_ = os.WriteFile(execOff, []byte("{corrupt-off"), 0o600) // corrupt bytes must still be unlinked without open under OFF
		denyExecutionOpen(t, execOff)
		if got := m.forgetEvidence("claude", "off-end", 2, time.Time{}, false); got != "" {
			t.Fatalf("forgetEvidence OFF = %q", got)
		}
		if _, err := os.Lstat(execOff); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("forgetEvidence OFF kept corrupt .execution: %v", err)
		}
		if got := m.forget("claude", "off-end", 0); got != "" {
			t.Fatalf("forget(0) = %q", got)
		}
		// Newer proof (> floor) protects both <hash> and .execution under OFF.
		m.Remember("claude", "off-keep", 4)
		execKeep := writeTestExecutionRecord(t, m, "claude", "off-keep", sessionproof.Record{Version: 1, Origin: sessionproof.OriginHook, Generation: 4, ExecutionID: testExecutionID("2")})
		denyExecutionOpen(t, execKeep)
		if got := m.forgetEvidence("claude", "off-keep", 2, time.Time{}, false); got != "" {
			t.Fatalf("forgetEvidence OFF keep = %q", got)
		}
		if _, err := os.Lstat(execKeep); err != nil {
			t.Fatalf("forgetEvidence OFF deleted newer .execution: %v", err)
		}

		// Damaged <hash> returns "" after recording .ended without touching .execution.
		pDmg := m.name("claude", "dmg-hash")
		_ = os.WriteFile(pDmg, []byte("bad"), 0o600)
		if got := m.forgetEvidence("claude", "dmg-hash", 2, time.Time{}, false); got != "" {
			t.Fatalf("forgetEvidence on damaged hash = %q", got)
		}

		// ON: forgetEvidence deletes matching valid .execution, preserves damaged or newer .execution.
		m.rememberWithSuccession("claude", "on-end", 2, time.Time{}, true)
		execOn := writeTestExecutionRecord(t, m, "claude", "on-end", sessionproof.Record{Version: 1, Origin: sessionproof.OriginHook, Generation: 2, ExecutionID: testExecutionID("3")})
		if got := m.forgetEvidence("claude", "on-end", 2, time.Time{}, true); got != "" {
			t.Fatalf("forgetEvidence ON = %q", got)
		}
		if _, err := os.Lstat(execOn); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("forgetEvidence ON left .execution behind: %v", err)
		}

		m.rememberWithSuccession("claude", "on-dmg", 2, time.Time{}, true)
		execOnDmg := executionPath(m.name("claude", "on-dmg"))
		_ = os.WriteFile(execOnDmg, []byte("{corrupt-on"), 0o600)
		if got := m.forgetEvidence("claude", "on-dmg", 2, time.Time{}, true); got != "" {
			t.Fatalf("forgetEvidence ON damaged = %q", got)
		}
		if _, err := os.Lstat(execOnDmg); err != nil {
			t.Fatalf("forgetEvidence ON removed damaged .execution instead of preserving damage: %v", err)
		}

		// pruneWithSuccession: aged .ended expires cleanly under OFF/ON; young <hash> or existing .ended protects .execution;
		// orphaned aged .execution is pruned; non-expired .ended or readEndMarkerDeadline deadline expiry exits cleanly.
		mPrune := fileMemory{dir: filepath.Join(t.TempDir(), "sessions"), now: func() time.Time { return clock }, mono: func() time.Time { return clock }, restrict: config.OwnerOnly}
		mPrune.Remember("claude", "active", 4)
		execActive := writeTestExecutionRecord(t, mPrune, "claude", "active", sessionproof.Record{Version: 1, Origin: sessionproof.OriginHook, Generation: 4, ExecutionID: testExecutionID("4")})
		old := clock.Add(-memoryAge - time.Hour)
		_ = os.Chtimes(execActive, old, old) // young <hash> must protect aged .execution

		mPrune.Remember("claude", "ended-young", 2)
		mPrune.Forget("claude", "ended-young", 2)
		execEnded := writeTestExecutionRecord(t, mPrune, "claude", "ended-young", sessionproof.Record{Version: 1, Origin: sessionproof.OriginHook, Generation: 2, ExecutionID: testExecutionID("5")})
		_ = os.Chtimes(execEnded, old, old) // young .ended protects aged .execution even when <hash> is absent until .ended expires

		execOrphan := writeTestExecutionRecord(t, mPrune, "claude", "orphan", sessionproof.Record{Version: 1, Origin: sessionproof.OriginHook, Generation: 1, ExecutionID: testExecutionID("6")})
		_ = os.Chtimes(execOrphan, old, old)

		// Corrupt aged .ended falls through to raw stale removal; expired valid .ended cleans via cleanupExpiredEnd.
		dmgEnded := mPrune.name("claude", "dmg-ended") + ".ended"
		_ = os.WriteFile(dmgEnded, []byte("bad"), 0o600)
		_ = os.Chtimes(dmgEnded, old, old)

		mPrune.Remember("claude", "aged-ended", 2)
		_ = os.WriteFile(mPrune.name("claude", "aged-ended")+".ended", []byte("2"), 0o600)
		execAgedEnd := writeTestExecutionRecord(t, mPrune, "claude", "aged-ended", sessionproof.Record{Version: 1, Origin: sessionproof.OriginHook, Generation: 2, ExecutionID: testExecutionID("7")})
		_ = os.Chtimes(mPrune.name("claude", "aged-ended"), old, old)
		_ = os.Chtimes(mPrune.name("claude", "aged-ended")+".ended", old, old)
		_ = os.Chtimes(execAgedEnd, old, old)

		if err := mPrune.pruneWithSuccession(time.Time{}, true); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Lstat(execActive); err != nil {
			t.Fatalf("prune deleted active session's .execution: %v", err)
		}
		if _, err := os.Lstat(execEnded); err != nil {
			t.Fatalf("prune bypassed fresh .ended and deleted .execution: %v", err)
		}
		if _, err := os.Lstat(execOrphan); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("prune left orphaned stale .execution: %v", err)
		}
		if _, err := os.Lstat(dmgEnded); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("prune kept damaged aged .ended: %v", err)
		}
		if _, err := os.Lstat(execAgedEnd); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("prune left expired ended .execution: %v", err)
		}
		if _, err := os.Lstat(mPrune.name("claude", "aged-ended") + ".ended"); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("prune left expired .ended: %v", err)
		}

		// Valid .ended touched fresh after ReadDir is skipped during prune, and readEndMarkerDeadline expiry exits prune cleanly.
		mRace := fileMemory{dir: filepath.Join(t.TempDir(), "sessions"), now: func() time.Time { return clock }, mono: func() time.Time { return clock }, restrict: config.OwnerOnly}
		_ = mRace.directory()
		raceEnded := mRace.name("claude", "fresh-before-lock") + ".ended"
		_ = os.WriteFile(raceEnded, []byte("2"), 0o600)
		_ = os.Chtimes(raceEnded, old, old)
		calls := 0
		mRace.mono = func() time.Time {
			calls++
			// Refresh file right before lock loop checks readEndMarkerDeadline (call 6).
			if calls == 2 {
				_ = os.Chtimes(raceEnded, clock, clock)
			}
			return clock
		}
		if err := mRace.pruneWithSuccession(clock.Add(hookLockBudget), true); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Lstat(raceEnded); err != nil {
			t.Fatalf("prune removed non-expired .ended: %v", err)
		}

		_ = os.Chtimes(raceEnded, old, old)
		calls = 0
		deadline := clock.Add(hookLockBudget)
		mRace.mono = func() time.Time {
			calls++
			if calls == 7 {
				return deadline
			}
			return clock
		}
		if err := mRace.pruneWithSuccession(deadline, true); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Lstat(raceEnded); err != nil {
			t.Fatalf("prune deleted .ended after deadline expired in readEndMarkerDeadline: %v", err)
		}
	})
}
