//go:build !windows

package gitinfo

import (
	"os"

	"golang.org/x/sys/unix"
)

// fstat and readAt are what ReadIndexID reads with. Tests replace them to see a read fail.
var (
	fstat  = unix.Fstat
	readAt = (*os.File).ReadAt
)

// ReadIndexID reads the IndexID of the index file at path. The fields and the tail come from
// one open file, so they belong to one generation even if the path is replaced meanwhile. A
// missing file, or one that cannot be read whole, is an error.
func ReadIndexID(path string) (IndexID, error) {
	f, err := os.Open(path)
	if err != nil {
		return IndexID{}, err
	}
	defer func() { _ = f.Close() }()
	var st unix.Stat_t
	if err = fstat(int(f.Fd()), &st); err != nil {
		return IndexID{}, err
	}
	// Dev is a uint64 on Linux and an int32 on macOS.
	id := IndexID{Device: uint64(st.Dev), Inode: st.Ino, Size: st.Size, Modified: st.Mtim.Nano(), Changed: st.Ctim.Nano()}
	n := min(int64(len(id.Tail)), st.Size)
	if _, err = readAt(f, id.Tail[:n], st.Size-n); err != nil {
		return IndexID{}, err
	}
	return id, nil
}
