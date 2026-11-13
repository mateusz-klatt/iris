//go:build !windows

package kimiwake

import (
	"os"

	"golang.org/x/sys/unix"
)

// tryLock takes f's exclusive lock if nobody holds it, and fails at once otherwise.
func tryLock(f *os.File) error { return unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB) }

func unlock(f *os.File) { _ = unix.Flock(int(f.Fd()), unix.LOCK_UN) }
