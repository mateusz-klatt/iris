//go:build !windows

package hubclient

import (
	"os"
	"syscall"
)

func openRegularConfig(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
}
