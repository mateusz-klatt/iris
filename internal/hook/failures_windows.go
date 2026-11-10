//go:build windows

package hook

import (
	"golang.org/x/sys/windows"
)

// tryLockFile takes f's exclusive lock if nobody holds it, and fails at once otherwise. The
// failure log and the sessions directory's lock use it.
func tryLockFile(f interface{ Fd() uintptr }) error {
	return windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &windows.Overlapped{})
}

// lockFileShared waits for a writer. Windows locks are mandatory, so a read of a range that a
// writer holds fails instead of waiting.
func lockFileShared(f interface{ Fd() uintptr }) error {
	return windows.LockFileEx(windows.Handle(f.Fd()), 0, 0, 1, 0, &windows.Overlapped{})
}
func unlockFile(f interface{ Fd() uintptr }) {
	_ = windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &windows.Overlapped{})
}
