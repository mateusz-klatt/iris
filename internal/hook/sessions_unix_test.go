//go:build !windows

package hook

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestSessionsLockRemainsUsableUnderRestrictiveUmask(t *testing.T) {
	if os.Getenv("IRIS_TEST_SESSION_LOCK_UMASK") != "1" {
		// Umask is process-wide. Keep it in a helper process so unrelated tests and background
		// hook observation writes retain their normal creation permissions.
		bin, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		child := exec.CommandContext(ctx, bin, "-test.run=^TestSessionsLockRemainsUsableUnderRestrictiveUmask$", "-test.count=1")
		child.Env = append(os.Environ(), "IRIS_TEST_SESSION_LOCK_UMASK=1")
		if out, err := child.CombinedOutput(); err != nil {
			t.Fatalf("restrictive-umask lock regression: %v\n%s", err, out)
		}
		return
	}
	s := newSessions(t)
	old := unix.Umask(0o777)
	defer unix.Umask(old)
	first, err := s.Lock(hookLockBudget)
	if err != nil {
		t.Fatal(err)
	}
	first()
	path := filepath.Join(s.Dir(), sessionsLock)
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("the first lock retained unusable umask permissions: %v, %v", info, err)
	}
	// A fresh caller must reopen the same stable inode while the restrictive umask remains.
	reopened := NewSessions(filepath.Join(filepath.Dir(s.Dir()), "hub.token"))
	second, err := reopened.Lock(hookLockBudget)
	if err != nil {
		t.Fatalf("a subsequent caller could not reopen the lock: %v", err)
	}
	second()
}
