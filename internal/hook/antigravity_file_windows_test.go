//go:build windows

package hook

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

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
	err = retryAntigravityStateRename(func() error {
		calls++
		return permanent
	}, func(time.Duration) { pauses++ })
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
