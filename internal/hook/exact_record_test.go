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
