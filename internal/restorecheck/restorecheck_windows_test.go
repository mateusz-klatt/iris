package restorecheck

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestRestoreRefusesALockedBackupWithoutChangingIt(t *testing.T) {
	src := backup(t, false)
	before, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(src, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	handle := windows.Handle(f.Fd())
	// Closing the owning file releases the mandatory byte-range lock as well.
	t.Cleanup(func() {
		if err := f.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := windows.LockFileEx(handle, windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &windows.Overlapped{}); err != nil {
		t.Fatal(err)
	}
	scratch := filepath.Join(t.TempDir(), "scratch")
	var out bytes.Buffer
	if err := Run(context.Background(), src, scratch, &out); !errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		t.Fatalf("locked backup: %v\n%s", err, &out)
	}
	if _, err := os.Stat(filepath.Join(scratch, "hub.db")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed copy left a database: %v", err)
	}
	if err := windows.UnlockFileEx(handle, 0, 1, 0, &windows.Overlapped{}); err != nil {
		t.Fatal(err)
	}
	if after, err := os.ReadFile(src); err != nil || !bytes.Equal(before, after) {
		t.Fatalf("backup changed: %v", err)
	}
}
