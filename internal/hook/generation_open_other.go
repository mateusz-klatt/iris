//go:build !windows

package hook

import "os"

// OpenGenerationFile opens a generation or end-marker file for a bounded read.
func OpenGenerationFile(path string) (*os.File, error) {
	return os.Open(path)
}
