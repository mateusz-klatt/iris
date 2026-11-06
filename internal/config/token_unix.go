//go:build !windows

package config

import (
	"errors"
	"golang.org/x/sys/unix"
	"os"
)

// unixLstat is the ownership check. Tests report a foreign uid without chown.
var unixLstat = unix.Lstat

func secureToken(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("token must be a regular file")
	}
	var st unix.Stat_t
	if err = unixLstat(path, &st); err != nil {
		return err
	}
	if st.Uid != uint32(os.Geteuid()) {
		return errors.New("token is not owned by current user")
	}
	return os.Chmod(path, 0600)
}
