//go:build windows

package hook

import (
	"golang.org/x/sys/windows"
)

func lockFailureLog(f interface{ Fd() uintptr }) error {
	return windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &windows.Overlapped{})
}

// lockFailureLogShared waits for a writer. Windows locks are mandatory, so a read of a
// range that a writer holds fails instead of waiting.
func lockFailureLogShared(f interface{ Fd() uintptr }) error {
	return windows.LockFileEx(windows.Handle(f.Fd()), 0, 0, 1, 0, &windows.Overlapped{})
}
func unlockFailureLog(f interface{ Fd() uintptr }) {
	_ = windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &windows.Overlapped{})
}
