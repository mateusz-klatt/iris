//go:build !windows

package hubclient

import (
	"errors"
	"os"
	"syscall"
)

type configFileID struct {
	dev uint64
	ino uint64
}

func sameConfigFileID(a, b configFileID) bool {
	return a.dev == b.dev && a.ino == b.ino
}

var (
	unixConfigLstat = os.Lstat
	unixConfigOpen  = os.OpenFile
)

func statRegularConfig(path string) (os.FileInfo, configFileID, error) {
	info, err := unixConfigLstat(path)
	if err != nil {
		return nil, configFileID{}, err
	}
	if !info.Mode().IsRegular() {
		return info, configFileID{}, nil
	}
	st := info.Sys().(*syscall.Stat_t)
	return info, configFileID{dev: uint64(st.Dev), ino: uint64(st.Ino)}, nil
}

func openRegularConfig(path string, expected configFileID) (*os.File, configFileID, error) {
	f, err := unixConfigOpen(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, configFileID{}, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, configFileID{}, err
	}
	if !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, configFileID{}, errors.New("client config is not a bounded regular file")
	}
	st := info.Sys().(*syscall.Stat_t)
	actual := configFileID{dev: uint64(st.Dev), ino: uint64(st.Ino)}
	if !sameConfigFileID(expected, actual) {
		_ = f.Close()
		return nil, configFileID{}, errors.New("client config replaced concurrently")
	}
	return f, actual, nil
}

func lockConfigFile(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
}

func unlockConfigFile(f *os.File) {
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
