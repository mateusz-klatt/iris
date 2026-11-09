package store

import (
	"fmt"
	"golang.org/x/sys/windows"
	"os"
)

func lockDatabase(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	err = windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &windows.Overlapped{})
	if err != nil {
		// Nothing was written or locked; preserve the lock failure if cleanup also fails.
		_ = f.Close()
		return nil, fmt.Errorf("database already owned by another hub: %w", err)
	}
	return f, nil
}
func unlockDatabase(f *os.File) {
	if f != nil {
		_ = windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &windows.Overlapped{})
		_ = f.Close()
	}
}
