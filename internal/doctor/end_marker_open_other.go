//go:build !windows

package doctor

import "os"

func openEndMarkerFile(path string) (*os.File, error) {
	return os.Open(path)
}
