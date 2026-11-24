//go:build !windows

package hook

import (
	"os"
	"syscall"
)

// OpenGenerationFile opens a generation or end-marker file for a bounded read without waiting
// for a FIFO writer. The shared reader accepts only regular files, for which O_NONBLOCK has no effect.
func OpenGenerationFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}
