//go:build !windows

package hook

import (
	"golang.org/x/sys/unix"
	"os"
)

func lockFailureLog(f *os.File) error { return unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB) }
func unlockFailureLog(f *os.File)     { _ = unix.Flock(int(f.Fd()), unix.LOCK_UN) }
