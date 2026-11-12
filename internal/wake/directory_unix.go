//go:build !windows

package wake

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

var directoryOwner = unix.Lstat
var directoryStat = os.Lstat
var directoryMake = os.MkdirAll
var directoryChmod = os.Chmod

type syncFile interface {
	Sync() error
	Close() error
}

var directoryOpen = func(path string) (syncFile, error) { return os.Open(path) }

func privateDirectory(path string) error {
	if err := directoryMake(path, 0700); err != nil {
		return err
	}
	info, err := directoryStat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("wake directory is not a directory")
	}
	var stat unix.Stat_t
	if err = directoryOwner(path, &stat); err != nil {
		return err
	}
	if stat.Uid != uint32(os.Geteuid()) {
		return errors.New("wake directory has another owner")
	}
	// Exact equality matters: 0500 and restrictive-umask 0000 cannot publish a reservation.
	if info.Mode().Perm() != 0700 || info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		if err := directoryChmod(path, 0700); err != nil {
			return err
		}
		info, err = directoryStat(path)
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode().Perm() != 0700 || info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
			return errors.New("wake directory is not private")
		}
	}
	return nil
}

func syncDirectory(path string) error {
	f, err := directoryOpen(path)
	if err != nil {
		return err
	}
	err = f.Sync()
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	return err
}
