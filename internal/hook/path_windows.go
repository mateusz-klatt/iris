package hook

import (
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

var windowsFullPathName = windows.GetFullPathName

// extendedWindowsPath mirrors Go 1.27.4 src/os/path_windows.go:107-201. Short paths keep Win32's
// own normalization; longer paths use GetFullPathName before the extended prefix is added. Unlike
// addExtendedPrefix, namespace selection uses the resolved name so relative paths under UNC working
// directories stay valid.
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

	p, err := windows.UTF16FromString(path)
	if err != nil {
		return original
	}
	n := uint32(pathLength) + 1
	var buf []uint16
	for {
		buf = make([]uint16, n)
		n, err = windowsFullPathName(&p[0], n, &buf[0], nil)
		if err != nil {
			return original
		}
		if n <= uint32(len(buf)) {
			buf = buf[:n]
			break
		}
	}
	resolved := windows.UTF16ToString(buf)
	if strings.HasPrefix(resolved, `\\`) && !strings.HasPrefix(resolved, `\\?\`) && !strings.HasPrefix(resolved, `\\.\`) {
		return `\\?\UNC\` + strings.TrimPrefix(resolved, `\\`)
	}
	return `\\?\` + resolved
}
