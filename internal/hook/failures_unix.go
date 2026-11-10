//go:build !windows

package hook

import (
	"golang.org/x/sys/unix"
)

// tryLockFile takes f's exclusive lock if nobody holds it, and fails at once otherwise. The
// failure log and the sessions directory's lock use it.
func tryLockFile(f interface{ Fd() uintptr }) error {
	return unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
}
func lockFileShared(f interface{ Fd() uintptr }) error {
	return unix.Flock(int(f.Fd()), unix.LOCK_SH)
}
func unlockFile(f interface{ Fd() uintptr }) { _ = unix.Flock(int(f.Fd()), unix.LOCK_UN) }
