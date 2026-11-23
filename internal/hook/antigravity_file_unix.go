//go:build !windows

package hook

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// renameAntigravityStateFile is the final replace of a state file. Tests set it
// to the error a replace returns after its own retries are finished.
var renameAntigravityStateFile = func(oldpath, newpath string) error {
	return os.Rename(oldpath, newpath)
}

func openAntigravityStateFile(path string) (*os.File, error) {
	return os.Open(path)
}

func openRegularTranscript(path string) (*os.File, string, int64, error) {
	return openRegularFileUnix(path, os.OpenFile, func(f *os.File) (os.FileInfo, error) { return f.Stat() })
}

func openRegularFileUnix(
	path string,
	open func(string, int, os.FileMode) (*os.File, error),
	stat func(*os.File) (os.FileInfo, error),
) (*os.File, string, int64, error) {
	if !filepath.IsAbs(path) {
		return nil, "", 0, errors.New("transcript path is not absolute")
	}
	f, err := open(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, "", 0, err
	}
	info, err := stat(f)
	if err != nil {
		_ = f.Close()
		return nil, "", 0, err
	}
	if !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, "", 0, errors.New("transcript is not a regular file")
	}
	st := info.Sys().(*syscall.Stat_t)
	return f, fmt.Sprintf("%d:%d", uint64(st.Dev), uint64(st.Ino)), info.Size(), nil
}
