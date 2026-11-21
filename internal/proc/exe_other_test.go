//go:build !linux

package proc

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestOpenExecutableRefusesDeletedPathOnNonLinux(t *testing.T) {
	dir := t.TempDir()
	reg := filepath.Join(dir, "agy")
	if err := os.WriteFile(reg, []byte("bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	live := func(int) (Snapshot, bool) { return Snapshot{PID: 10, StartMS: 100}, true }
	deleted := func(int, Snapshot) (string, error) { return reg + " (deleted)", nil }
	if _, err := openExecutableFrom(10, 100, live, deleted, os.Stat, openDeletedExeOS); !errors.Is(err, ErrExecutableDeleted) {
		t.Fatalf("non-Linux deleted exe = %v, want ErrExecutableDeleted", err)
	}

	img := &ProcessExecutable{Path: reg}
	cmd := img.Command(context.Background(), "agentapi", "send-message")
	if cmd.Path != reg || len(cmd.ExtraFiles) != 0 {
		t.Fatalf("non-Linux Command = path %q extraFiles %d", cmd.Path, len(cmd.ExtraFiles))
	}
	if err := img.Close(); err != nil {
		t.Fatalf("non-Linux Close = %v", err)
	}
}
