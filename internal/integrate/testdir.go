package integrate

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// CleanupTestDir removes a temporary directory created by the calling process.
// The caller must retain its own MkdirTemp result, never discover old directories.
// A short retry lets Windows release child-process files. Failure is reported
// without changing the test process's exit status.
func CleanupTestDir(dir, prefix string) {
	if err := removeTestDir(dir, prefix, os.RemoveAll, time.Sleep); err != nil {
		fmt.Fprintf(os.Stderr, "test cleanup %q: %v\n", dir, err)
	}
}

func removeTestDir(dir, prefix string, remove func(string) error, sleep func(time.Duration)) error {
	if dir == "" || !filepath.IsAbs(dir) || filepath.Dir(dir) != filepath.Clean(os.TempDir()) ||
		prefix == "" || !strings.HasPrefix(filepath.Base(dir), prefix) {
		return fmt.Errorf("refusing to remove directory outside temporary prefix %q", prefix)
	}
	var err error
	for attempt := range 21 {
		if attempt > 0 {
			sleep(100 * time.Millisecond)
		}
		err = remove(dir)
		if err == nil {
			return nil
		}
	}
	return err
}
