//go:build !windows

package hook

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestOpenRegularTranscriptUnix(t *testing.T) {
	if _, _, _, err := openRegularTranscript("relative.jsonl"); err == nil {
		t.Fatal("expected relative path rejection")
	}
	dir := t.TempDir()
	if _, _, _, err := openRegularTranscript(filepath.Join(dir, "missing.jsonl")); err == nil {
		t.Fatal("expected missing file error")
	}
	if _, _, _, err := openRegularTranscript(dir); err == nil {
		t.Fatal("expected directory rejection")
	}

	target := filepath.Join(dir, "real.jsonl")
	if err := os.WriteFile(target, []byte("{\"ok\":true}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.jsonl")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := openRegularTranscript(link); err == nil {
		t.Fatal("expected O_NOFOLLOW symlink rejection")
	}

	if _, _, _, err := openRegularFileUnix(target, os.OpenFile, func(*os.File) (os.FileInfo, error) {
		return nil, errors.New("fstat failed")
	}); err == nil {
		t.Fatal("expected fstat error")
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
