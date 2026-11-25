//go:build !windows

package hook

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/mateusz-klatt/iris/internal/config"
	"github.com/mateusz-klatt/iris/internal/sessionproof"
)

func TestExactRecordRejectsSymlinkAndSwapOnUnix(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "other.execution")
	rec := sessionproof.Record{
		Version:     sessionproof.FormatVersion,
		Origin:      sessionproof.OriginHook,
		Generation:  4,
		ExecutionID: testExecutionID("f"),
	}
	raw, err := sessionproof.EncodeRecord(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "session.execution")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, st, err := readExactRecordDeadline(link, nil, timeZero); st != sessionproof.FileDamaged || !errors.Is(err, sessionproof.ErrInvalidRecord) {
		t.Fatalf("symlink .execution = (%v, %v), want FileDamaged/ErrInvalidRecord", st, err)
	}

	// Regular-file swap between Lstat and open (different dev/ino), open error, Stat error, and dir fd.
	realExec := filepath.Join(dir, "real.execution")
	otherExec := filepath.Join(dir, "other2.execution")
	if err := os.WriteFile(realExec, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(otherExec, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	origUnixOpen := unixExactRecordOpen
	t.Cleanup(func() { unixExactRecordOpen = origUnixOpen })

	swapped := false
	unixExactRecordOpen = func(p string, flag int, perm os.FileMode) (*os.File, error) {
		if !swapped && p == realExec {
			swapped = true
			if err := os.Rename(otherExec, realExec); err != nil {
				t.Fatal(err)
			}
		}
		return origUnixOpen(p, flag, perm)
	}
	if _, st, err := readExactRecordDeadline(realExec, nil, timeZero); st != sessionproof.FileDamaged || !errors.Is(err, sessionproof.ErrInvalidRecord) {
		t.Fatalf("swapped inode = (%v, %v), want FileDamaged", st, err)
	}

	// Symlink swapped between Lstat and O_NOFOLLOW open -> ELOOP -> FileDamaged.
	if err := os.WriteFile(otherExec, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(realExec)
	if err := os.WriteFile(realExec, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	unixExactRecordOpen = func(p string, flag int, perm os.FileMode) (*os.File, error) {
		_ = os.Remove(realExec)
		if err := os.Symlink(target, realExec); err != nil {
			t.Fatal(err)
		}
		return origUnixOpen(p, flag, perm)
	}
	if _, st, err := readExactRecordDeadline(realExec, nil, timeZero); st != sessionproof.FileDamaged || !errors.Is(err, sessionproof.ErrInvalidRecord) {
		t.Fatalf("symlink swapped before O_NOFOLLOW = (%v, %v), want FileDamaged", st, err)
	}
	_ = os.Remove(realExec)
	if err := os.WriteFile(realExec, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	// Closed fd (f.Stat error) and FIFO/directory fd inside openExactRecordFile.
	unixExactRecordOpen = func(p string, flag int, perm os.FileMode) (*os.File, error) {
		f, err := origUnixOpen(p, flag, perm)
		if err == nil {
			_ = f.Close()
		}
		return f, err
	}
	if _, st, err := readExactRecordDeadline(realExec, nil, timeZero); st != sessionproof.FileDamaged || !errors.Is(err, sessionproof.ErrInvalidRecord) {
		t.Fatalf("closed fd = (%v, %v), want FileDamaged", st, err)
	}
	unixExactRecordOpen = func(string, int, os.FileMode) (*os.File, error) {
		return os.Open(dir)
	}
	if _, st, err := readExactRecordDeadline(realExec, nil, timeZero); st != sessionproof.FileDamaged || !errors.Is(err, sessionproof.ErrInvalidRecord) {
		t.Fatalf("dir fd = (%v, %v), want FileDamaged", st, err)
	}
	// Open returns O_NONBLOCK error on nonexistent FIFO test (normal FIFO caught by statExactRecordFile).
	fifo := filepath.Join(dir, "fifo.execution")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	unixExactRecordOpen = origUnixOpen
	if _, st, err := readExactRecordDeadline(fifo, nil, timeZero); st != sessionproof.FileDamaged || !errors.Is(err, sessionproof.ErrInvalidRecord) {
		t.Fatalf("fifo .execution = (%v, %v), want FileDamaged", st, err)
	}
	_ = config.OwnerOnly
}
