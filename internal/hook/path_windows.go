package hook

import (
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

var windowsFullPathName = windows.GetFullPathName

// extendedWindowsPath mirrors Go 1.27.4 src/os/path_windows.go:107-201. Short paths keep Win32's
// own normalization; longer paths use GetFullPathName before the extended prefix is added.
func extendedWindowsPath(path string) string {
	original := path
	path = strings.ReplaceAll(path, "/", `\`)
	if strings.HasPrefix(path, `\\?\`) || strings.HasPrefix(path, `\\.\`) || strings.HasPrefix(path, `\??\`) {
		return path
	}

	pathLength := len(path)
	if !filepath.IsAbs(path) {
		cwd, _ := os.Getwd()
		pathLength += len(cwd) + 1
	}
	if pathLength < 248 {
		return path
	}

	isUNC := strings.HasPrefix(path, `\\`)
	prefix := []uint16{'\\', '\\', '?', '\\'}
	if isUNC {
		prefix = []uint16{'\\', '\\', '?', '\\', 'U', 'N', 'C', '\\'}
	}
	p, err := windows.UTF16FromString(path)
	if err != nil {
		return original
	}
	n := uint32(pathLength) + 1
	var buf []uint16
	for {
		buf = make([]uint16, n+uint32(len(prefix)))
		n, err = windowsFullPathName(&p[0], n, &buf[len(prefix)], nil)
		if err != nil {
			return original
		}
		if n <= uint32(len(buf)-len(prefix)) {
			buf = buf[:n+uint32(len(prefix))]
			break
		}
	}
	if isUNC {
		buf = buf[2:]
	}
	copy(buf, prefix)
	return windows.UTF16ToString(buf)
}
