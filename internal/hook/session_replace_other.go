//go:build !windows

package hook

import (
	"os"
	"time"
)

func replaceSessionFileDeadline(oldpath, newpath string, deadline time.Time, _ bool, now func() time.Time, _ func(time.Duration)) error {
	if deadlineExpired(now, deadline) {
		return os.ErrDeadlineExceeded
	}
	return os.Rename(oldpath, newpath)
}
