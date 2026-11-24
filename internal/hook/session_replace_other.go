//go:build !windows

package hook

import "os"

func replaceSessionFile(oldpath, newpath string) error {
	return os.Rename(oldpath, newpath)
}
