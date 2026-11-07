// Package gitinfo reads worktree identity from git files.
// It never runs the git binary: a hook cannot afford that, especially on Windows.
package gitinfo

import (
	"errors"
	"strings"
	"unicode"
)

// ErrEscape means path is outside the worktree or walks out with "..".
var ErrEscape = errors.New("path escapes the worktree")

// Relative returns path relative to the worktree root, using forward slashes.
// Relative inputs are resolved against root. ".." that leaves the worktree is rejected.
// Windows drive letters and backslashes are accepted on every OS.
func Relative(root, path string) (string, error) {
	rp := parsePath(root)
	if !rp.abs && rp.vol == "" {
		return "", ErrEscape
	}
	pp := parsePath(path)
	if !pp.abs {
		// A relative path is resolved from the worktree root, after the caller
		// has joined it to the event's cwd when that cwd is not the root.
		pp.vol = rp.vol
		pp.abs = true
		pp.parts = clean(append(append([]string{}, rp.parts...), pp.parts...), true)
	}
	if rp.vol != pp.vol {
		return "", ErrEscape
	}
	// A drive letter means Windows names: case-insensitive directories, and trailing dots
	// and spaces that Win32 strips. Measured on NTFS, both reached one file under another name.
	windows := rp.vol != ""
	if windows {
		rp.parts = win32Names(rp.parts)
		pp.parts = win32Names(pp.parts)
	}
	if len(pp.parts) < len(rp.parts) {
		return "", ErrEscape
	}
	for i, seg := range rp.parts {
		if pp.parts[i] != seg && !(windows && strings.EqualFold(pp.parts[i], seg)) {
			return "", ErrEscape
		}
	}
	rest := pp.parts[len(rp.parts):]
	if len(rest) == 0 {
		return "", ErrEscape
	}
	return strings.Join(rest, "/"), nil
}

// win32Names drops the trailing dots and spaces that Win32 removes from every name.
// A name made only of them is left alone, as Win32 does not reduce it to nothing.
func win32Names(parts []string) []string {
	out := make([]string, len(parts))
	for i, seg := range parts {
		out[i] = seg
		if t := strings.TrimRight(seg, ". "); t != "" {
			out[i] = t
		}
	}
	return out
}

type lexPath struct {
	vol   string
	abs   bool
	parts []string
}

func parsePath(s string) lexPath {
	s = strings.ReplaceAll(s, "\\", "/")
	var vol string
	if len(s) >= 2 && s[1] == ':' && unicode.IsLetter(rune(s[0])) {
		vol = strings.ToUpper(s[:1])
		s = s[2:]
	}
	abs := strings.HasPrefix(s, "/")
	return lexPath{vol: vol, abs: abs, parts: clean(strings.Split(s, "/"), abs)}
}

// clean drops "." and applies "..". On an absolute path, ".." at the volume
// root is a no-op. On a relative path it is kept so a later join can see the escape.
func clean(raw []string, abs bool) []string {
	var parts []string
	for _, seg := range raw {
		if seg == "" || seg == "." {
			continue
		}
		if seg == ".." {
			if len(parts) > 0 && parts[len(parts)-1] != ".." {
				parts = parts[:len(parts)-1]
				continue
			}
			if abs {
				continue
			}
			parts = append(parts, "..")
			continue
		}
		parts = append(parts, seg)
	}
	return parts
}
