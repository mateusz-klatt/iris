//go:build windows

package hook

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestOpenRegularTranscriptWindows(t *testing.T) {
	if _, _, _, err := openRegularTranscript("relative.jsonl"); err == nil {
		t.Fatal("expected relative path rejection")
	}
	dir := t.TempDir()
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
