//go:build !windows

package hook

import (
	"errors"
	"os"
	"syscall"

	"github.com/mateusz-klatt/iris/internal/sessionproof"
)

func sessionPathAbsent(_ string, err error) bool {
	return errors.Is(err, os.ErrNotExist)
}

type exactRecordID struct {
	dev uint64
	ino uint64
}

var unixExactRecordOpen = os.OpenFile

func statExactRecordFile(path string) (os.FileInfo, exactRecordID, error) {
	info, err := lstatSessionFile(path)
	if err != nil {
		return nil, exactRecordID{}, err
	}
	if !info.Mode().IsRegular() {
		return info, exactRecordID{}, nil
	}
	st := info.Sys().(*syscall.Stat_t)
	return info, exactRecordID{dev: uint64(st.Dev), ino: uint64(st.Ino)}, nil
}

func openExactRecordFile(path string, expected exactRecordID) (*os.File, error) {
	f, err := unixExactRecordOpen(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, sessionproof.ErrInvalidRecord
	}
	st := info.Sys().(*syscall.Stat_t)
	if uint64(st.Dev) != expected.dev || uint64(st.Ino) != expected.ino {
		_ = f.Close()
		return nil, sessionproof.ErrInvalidRecord
	}
	return f, nil
}
