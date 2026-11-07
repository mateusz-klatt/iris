//go:build !windows

package hubclient

import (
	"errors"
	"os"
	"syscall"
)

// dirOwner reads the owner of a directory. Tests replace it to model someone else's.
var dirOwner = func(info os.FileInfo) int { return int(info.Sys().(*syscall.Stat_t).Uid) }

// privateDir keeps the Iris configuration directory owner-only. MkdirAll leaves an
// existing directory as it is, and one made by hand with `mkdir -p` has the umask's 0755,
// which lets other users list it. A directory that belongs to someone else is left alone.
func privateDir(dir string) error {
	info, err := os.Stat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return os.MkdirAll(dir, 0o700)
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return &os.PathError{Op: "mkdir", Path: dir, Err: syscall.ENOTDIR}
	}
	if dirOwner(info) != os.Geteuid() || info.Mode().Perm()&0o077 == 0 {
		return nil
	}
	return os.Chmod(dir, info.Mode().Perm()&^0o077)
}
