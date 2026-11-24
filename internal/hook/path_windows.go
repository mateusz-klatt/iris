package hook

import (
	"path/filepath"
	"strings"
)

var windowsPathAbs = filepath.Abs

// extendedWindowsPath normalizes separators but otherwise leaves device namespaces unchanged.
// A relative-path resolution failure returns the original input for the eventual open error.
func extendedWindowsPath(path string) string {
	original := path
	path = strings.ReplaceAll(path, "/", `\`)
	if strings.IndexByte(path, 0) >= 0 {
		return original
	}
	if strings.HasPrefix(path, `\\?\`) || strings.HasPrefix(path, `\\.\`) || strings.HasPrefix(path, `\??\`) {
		return path
	}
	if strings.HasPrefix(path, `\\`) {
		return `\\?\UNC\` + strings.TrimPrefix(filepath.Clean(path), `\\`)
	}
	if filepath.IsAbs(path) {
		return `\\?\` + filepath.Clean(path)
	}
	absolute, err := windowsPathAbs(path)
	if err != nil {
		return original
	}
	return extendedWindowsPath(absolute)
}
