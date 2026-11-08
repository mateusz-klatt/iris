//go:build !windows

package gitinfo

import (
	"fmt"
	"os"
	"syscall"
)

// directoryID names a directory by its device and inode, which every path to it shares: a
// bind mount or a symlinked parent gives the same directory another path string, and a hub
// that keyed worktrees on the path alone made two agents of one session (D6). It is "" when
// the directory cannot be read.
func directoryID(path string) string {
	info, err := os.Stat(path)
	if err != nil {
		return ""
	}
	st := info.Sys().(*syscall.Stat_t)
	return fmt.Sprintf("%d:%d", st.Dev, st.Ino)
}
