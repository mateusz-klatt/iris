package hook

import (
	"path/filepath"
	"strings"
)

func extendedWindowsPath(path string) string {
	if strings.IndexByte(path, 0) >= 0 {
		return path
	}
	if strings.HasPrefix(path, `\\?\`) || strings.HasPrefix(path, `\\.\`) || strings.HasPrefix(path, `\??\`) {
		return path
	}
	absolute, _ := filepath.Abs(path)
	absolute = filepath.Clean(absolute)
	if strings.HasPrefix(absolute, `\\`) {
		return `\\?\UNC\` + strings.TrimPrefix(absolute, `\\`)
	}
	return `\\?\` + absolute
}
