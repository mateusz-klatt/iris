//go:build !windows

package hook

import (
	"golang.org/x/sys/unix"
)

func lockFailureLog(f interface{ Fd() uintptr }) error {
	return unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
}
func lockFailureLogShared(f interface{ Fd() uintptr }) error {
	return unix.Flock(int(f.Fd()), unix.LOCK_SH)
}
func unlockFailureLog(f interface{ Fd() uintptr }) { _ = unix.Flock(int(f.Fd()), unix.LOCK_UN) }
