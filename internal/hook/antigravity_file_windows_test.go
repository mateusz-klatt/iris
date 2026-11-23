//go:build windows

package hook

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// slowRenameClock advances by less than hookLockBudget on every pause, so the
// attempt cap is what stops a retry. The budget test moves the clock by the
// whole budget instead. Neither clock reads the wall time.
func slowRenameClock() (advance func(), now func() time.Time) {
	current := time.Unix(1_000, 0)
	step := hookLockBudget / time.Duration(antigravityRenameAttempts)
	return func() { current = current.Add(step) }, func() time.Time { return current }
}

func countRenamePause(t *testing.T, pauses *int, advance func()) func(time.Duration) {
	t.Helper()
	return func(delay time.Duration) {
		if delay != antigravityRenamePause {
			t.Errorf("retry delay = %v, want %v", delay, antigravityRenamePause)
		}
		*pauses++
		advance()
	}
}

// retryFallbackUntil is the unsupported-then-classic latch in replaceAntigravityState,
// run on a clock the test controls.
func retryFallbackUntil(posix, classic func(string, string) error, pause func(time.Duration), now func() time.Time) error {
	unsupported := false
	return retryAntigravityStateRenameUntil(func() error {
		if !unsupported {
			err := posix("old", "new")
			if err == nil || !posixRenameUnsupported(err) {
				return err
			}
			unsupported = true
		}
		return classic("old", "new")
	}, pause, now)
}

func TestRetryAntigravityStateRename(t *testing.T) {
	calls, pauses := 0, 0
	err := retryAntigravityStateRename(func() error {
		calls++
		if calls < 3 {
			return &os.LinkError{Op: "rename", Err: os.ErrPermission}
		}
		return nil
	}, func(delay time.Duration) {
		if delay != antigravityRenamePause {
			t.Errorf("retry delay = %v, want %v", delay, antigravityRenamePause)
		}
		pauses++
	})
	if err != nil || calls != 3 || pauses != 2 {
		t.Fatalf("transient replacement = %v after %d calls and %d pauses", err, calls, pauses)
	}

	permanent := &os.LinkError{Op: "rename", Err: os.ErrPermission}
	calls, pauses = 0, 0
	advance, now := slowRenameClock()
	err = retryAntigravityStateRenameUntil(func() error {
		calls++
		return permanent
	}, countRenamePause(t, &pauses, advance), now)
	if !errors.Is(err, os.ErrPermission) || calls != antigravityRenameAttempts || pauses != antigravityRenameAttempts-1 {
		t.Fatalf("exhausted replacement = %v after %d calls and %d pauses", err, calls, pauses)
	}

	nonRetryable := errors.New("invalid replacement")
	calls = 0
	err = retryAntigravityStateRename(func() error {
		calls++
		return nonRetryable
	}, func(time.Duration) { t.Fatal("non-retryable error paused") })
	if !errors.Is(err, nonRetryable) || calls != 1 {
		t.Fatalf("non-retryable replacement = %v after %d calls", err, calls)
	}

	calls = 0
	err = retryAntigravityStateRename(func() error {
		calls++
		if calls == 1 {
			return &os.LinkError{Op: "rename", Err: windows.ERROR_SHARING_VIOLATION}
		}
		return nil
	}, func(time.Duration) {})
	if err != nil || calls != 2 {
		t.Fatalf("sharing-violation replacement = %v after %d calls", err, calls)
	}
}

func TestRetryAntigravityStateRenameStopsAtTheBudget(t *testing.T) {
	start := time.Unix(1_000, 0)
	now := start
	calls, pauses := 0, 0
	err := retryAntigravityStateRenameUntil(func() error {
		calls++
		return &os.LinkError{Op: "rename", Err: os.ErrPermission}
	}, func(delay time.Duration) {
		if delay != antigravityRenamePause {
			t.Errorf("retry delay = %v", delay)
		}
		pauses++
		now = now.Add(hookLockBudget)
	}, func() time.Time { return now })
	if !errors.Is(err, os.ErrPermission) || calls != 2 || pauses != 1 {
		t.Fatalf("budget stopped the retry = %v after %d calls and %d pauses", err, calls, pauses)
	}
}

func TestOpenRegularTranscriptWindows(t *testing.T) {
	if _, _, _, err := openRegularTranscript("relative.jsonl"); err == nil {
		t.Fatal("expected relative path rejection")
	}
	dir := t.TempDir()
	if _, err := openAntigravityStateFile(filepath.Join(dir, "bad\x00name.json")); err == nil {
		t.Fatal("expected state NUL path error")
	}
	if _, _, _, err := openRegularTranscript(filepath.Join(dir, "bad\x00name.jsonl")); err == nil {
		t.Fatal("expected NUL path error")
	}
	if _, _, _, err := openRegularTranscript(filepath.Join(dir, "missing.jsonl")); err == nil {
		t.Fatal("expected missing file error")
	}

	target := filepath.Join(dir, "real.jsonl")
	if err := os.WriteFile(target, []byte("{\"ok\":true}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := openRegularFileWindows(target, func(windows.Handle, *windows.ByHandleFileInformation) error {
		return errors.New("info failed")
	}, windows.GetFileType); err == nil {
		t.Fatal("expected fileInfo error")
	}
	if _, _, _, err := openRegularFileWindows(target, windows.GetFileInformationByHandle, func(windows.Handle) (uint32, error) {
		return windows.FILE_TYPE_PIPE, nil
	}); err == nil {
		t.Fatal("expected non-disk file rejection")
	}

	f, id, size, err := openRegularTranscript(target)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if id == "" || size != 12 {
		t.Fatalf("got id=%q size=%d", id, size)
	}
}

func TestAntigravityRenameInfoMatchesWin32Layout(t *testing.T) {
	var info antigravityRenameInfo
	handle := unsafe.Sizeof(windows.Handle(0))
	if unsafe.Offsetof(info.Flags) != 0 || unsafe.Sizeof(info.Flags) != 4 {
		t.Fatalf("Flags offset %d size %d", unsafe.Offsetof(info.Flags), unsafe.Sizeof(info.Flags))
	}
	if unsafe.Offsetof(info.RootDirectory) != handle {
		t.Fatalf("RootDirectory offset %d, want %d", unsafe.Offsetof(info.RootDirectory), handle)
	}
	if unsafe.Offsetof(info.FileNameLength) != handle+handle {
		t.Fatalf("FileNameLength offset %d, want %d", unsafe.Offsetof(info.FileNameLength), handle+handle)
	}
	if unsafe.Offsetof(info.FileName) != unsafe.Offsetof(info.FileNameLength)+4 {
		t.Fatalf("FileName offset %d", unsafe.Offsetof(info.FileName))
	}
}

func TestPOSIXRenameUnsupported(t *testing.T) {
	for _, err := range []error{
		windows.ERROR_INVALID_PARAMETER,
		windows.ERROR_NOT_SUPPORTED,
		windows.ERROR_INVALID_FUNCTION,
		fmt.Errorf("rename: %w", windows.ERROR_NOT_SUPPORTED),
	} {
		if !posixRenameUnsupported(err) {
			t.Fatalf("%v did not select the MoveFileEx fallback", err)
		}
	}
	if posixRenameUnsupported(windows.ERROR_ACCESS_DENIED) || posixRenameUnsupported(nil) {
		t.Fatal("a supported failure selected the MoveFileEx fallback")
	}
}

func TestReplaceAntigravityState(t *testing.T) {
	noop := func(time.Duration) { t.Fatal("rename paused") }
	classicUnused := func(string, string) error { t.Fatal("classic rename ran"); return nil }

	posixCalls := 0
	err := replaceAntigravityState("old", "new", func(old, new string) error {
		posixCalls++
		if old != "old" || new != "new" {
			t.Fatalf("paths = %s -> %s", old, new)
		}
		return nil
	}, classicUnused, noop)
	if err != nil || posixCalls != 1 {
		t.Fatalf("POSIX success = %v after %d calls", err, posixCalls)
	}

	posixCalls = 0
	err = replaceAntigravityState("old", "new", func(string, string) error {
		posixCalls++
		if posixCalls == 1 {
			return &os.LinkError{Op: "rename", Err: os.ErrPermission}
		}
		return nil
	}, classicUnused, func(time.Duration) {})
	if err != nil || posixCalls != 2 {
		t.Fatalf("retried POSIX rename = %v after %d calls", err, posixCalls)
	}

	posixCalls = 0
	err = replaceAntigravityState("old", "new", func(string, string) error {
		posixCalls++
		if posixCalls == 1 {
			return &os.LinkError{Op: "rename", Err: windows.ERROR_SHARING_VIOLATION}
		}
		return nil
	}, classicUnused, func(time.Duration) {})
	if err != nil || posixCalls != 2 {
		t.Fatalf("retried sharing violation = %v after %d calls", err, posixCalls)
	}

	permanent := errors.New("invalid replacement")
	posixCalls = 0
	err = replaceAntigravityState("old", "new", func(string, string) error {
		posixCalls++
		return permanent
	}, classicUnused, noop)
	if !errors.Is(err, permanent) || posixCalls != 1 {
		t.Fatalf("permanent POSIX failure = %v after %d calls", err, posixCalls)
	}

	for _, unsupported := range []error{windows.ERROR_INVALID_PARAMETER, windows.ERROR_NOT_SUPPORTED, windows.ERROR_INVALID_FUNCTION} {
		posixCalls, classicCalls := 0, 0
		err = replaceAntigravityState("old", "new", func(string, string) error {
			posixCalls++
			return unsupported
		}, func(old, new string) error {
			classicCalls++
			if old != "old" || new != "new" {
				t.Fatalf("classic paths = %s -> %s", old, new)
			}
			return nil
		}, noop)
		if err != nil || posixCalls != 1 || classicCalls != 1 {
			t.Fatalf("%v fallback = %v after posix %d classic %d", unsupported, err, posixCalls, classicCalls)
		}
	}

	posixCalls, classicCalls, pauses := 0, 0, 0
	err = replaceAntigravityState("old", "new", func(string, string) error {
		posixCalls++
		return windows.ERROR_NOT_SUPPORTED
	}, func(string, string) error {
		classicCalls++
		if classicCalls < 3 {
			return &os.LinkError{Op: "rename", Err: os.ErrPermission}
		}
		return nil
	}, func(time.Duration) { pauses++ })
	if err != nil || posixCalls != 1 || classicCalls != 3 || pauses != 2 {
		t.Fatalf("retried fallback = %v after posix %d classic %d pauses %d", err, posixCalls, classicCalls, pauses)
	}

	posixCalls, classicCalls, pauses = 0, 0, 0
	advance, now := slowRenameClock()
	err = retryFallbackUntil(func(string, string) error {
		posixCalls++
		return windows.ERROR_INVALID_PARAMETER
	}, func(string, string) error {
		classicCalls++
		return &os.LinkError{Op: "rename", Err: os.ErrPermission}
	}, countRenamePause(t, &pauses, advance), now)
	if !errors.Is(err, os.ErrPermission) || posixCalls != 1 || classicCalls != antigravityRenameAttempts || pauses != antigravityRenameAttempts-1 {
		t.Fatalf("exhausted fallback = %v after posix %d classic %d pauses %d", err, posixCalls, classicCalls, pauses)
	}

	posixCalls, classicCalls, pauses = 0, 0, 0
	advance, now = slowRenameClock()
	err = retryFallbackUntil(func(string, string) error {
		posixCalls++
		return &os.LinkError{Op: "rename", Err: os.ErrPermission}
	}, func(string, string) error {
		classicCalls++
		return nil
	}, countRenamePause(t, &pauses, advance), now)
	if !errors.Is(err, os.ErrPermission) || posixCalls != antigravityRenameAttempts || classicCalls != 0 || pauses != antigravityRenameAttempts-1 {
		t.Fatalf("exhausted POSIX rename = %v after posix %d classic %d pauses %d", err, posixCalls, classicCalls, pauses)
	}
}

func TestRenameAntigravityStatePOSIX(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.json")
	if err := os.WriteFile(src, []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := renameAntigravityStatePOSIX("bad\x00old", filepath.Join(dir, "dst.json")); !errors.Is(err, syscall.EINVAL) {
		t.Fatalf("NUL source = %v", err)
	}
	if err := renameAntigravityStatePOSIX(src, "bad\x00new"); !errors.Is(err, syscall.EINVAL) {
		t.Fatalf("NUL destination = %v", err)
	}
	if _, statErr := os.Stat(src); statErr != nil {
		t.Fatalf("source disappeared after a rejected name: %v", statErr)
	}
	if err := renameAntigravityStatePOSIX(filepath.Join(dir, "missing.json"), filepath.Join(dir, "dst.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing source = %v", err)
	}
	if err := renameAntigravityStatePOSIX(src, filepath.Join(dir, "no-such-dir", "dst.json")); err == nil {
		t.Fatal("rename into a missing directory succeeded")
	} else if _, statErr := os.Stat(src); statErr != nil {
		t.Fatalf("source disappeared after a failed rename: %v (rename %v)", statErr, err)
	}

	dst := filepath.Join(dir, "dst.json")
	if err := os.WriteFile(dst, []byte("held\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	held, err := openAntigravityStateFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Close() }()
	if err := renameAntigravityStatePOSIX(src, dst); err != nil {
		t.Fatalf("replace with a reader open: %v", err)
	}
	if _, statErr := os.Stat(src); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("source remained after rename: %v", statErr)
	}
	got, err := os.ReadFile(dst)
	if err != nil || string(got) != "old\n" {
		t.Fatalf("replacement contents = %q, %v", got, err)
	}
	kept, err := io.ReadAll(held)
	if err != nil || string(kept) != "held\n" {
		t.Fatalf("open reader lost the file it held: %q, %v", kept, err)
	}
}
