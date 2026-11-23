package store

import (
	"database/sql"
	"errors"
	"strings"
)

// ensureWorktreeKey returns the hub's stable key for one directory on one host.
// The same (host, WorktreeID) keeps its key when the display path, branch, remote
// or root changes. An empty host or id has no identity, so the key is null and
// the claim stays repository-wide (ADR 037). Two hosts never share a key.
func (t *transaction) ensureWorktreeKey(host, worktreeID string) (string, error) {
	if host == "" || worktreeID == "" {
		return "", nil
	}
	var key string
	err := t.row("SELECT worktree_key FROM WorktreeIdentity WHERE host_id=? AND worktree_id=?", host, worktreeID).Scan(&key)
	if err == nil {
		return key, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	key = id("wt_")
	if _, err = t.exec("INSERT INTO WorktreeIdentity(host_id,worktree_id,worktree_key) VALUES(?,?,?)", host, worktreeID, key); err != nil {
		return "", err
	}
	return key, nil
}

// canonicalDisplayPath is the spelling stored on a claim: absolute, lexically
// cleaned, with trailing separators removed without reducing a filesystem root.
// Windows drive letters are upper-case and UNC keeps its server and share. The
// stored spelling keeps its case; comparison folds Windows paths separately.
func canonicalDisplayPath(raw string) string {
	if raw == "" {
		return ""
	}
	if isUNCPath(raw) {
		return "//" + strings.Join(cleanDisplay(trimUNC(raw), 2), "/")
	}
	if drive, rest, ok := drivePath(raw); ok {
		parts := cleanDisplay(rest, 0)
		if len(parts) == 0 {
			return drive + ":/"
		}
		return drive + ":/" + strings.Join(parts, "/")
	}
	cleaned := cleanPOSIX(raw)
	if cleaned == "." {
		return ""
	}
	return cleaned
}

// worktreePathsEqual compares display paths for the legacy guard fallback.
// Windows drive and UNC paths use ADR 003's canonical caseless match. POSIX
// paths keep their case: macOS case aliases share a worktree through WorktreeID,
// not through folding every POSIX spelling together.
func worktreePathsEqual(a, b string) bool {
	ca, cb := canonicalDisplayPath(a), canonicalDisplayPath(b)
	if windowsDisplay(ca) || windowsDisplay(cb) {
		if windowsDisplay(ca) != windowsDisplay(cb) {
			return false
		}
		return pathKey(ca, "folded") == pathKey(cb, "folded")
	}
	return ca == cb
}

func windowsDisplay(p string) bool {
	if isUNCPath(p) {
		return true
	}
	_, _, ok := drivePath(p)
	return ok
}

func isUNCPath(p string) bool {
	return strings.HasPrefix(p, `\\`) || strings.HasPrefix(p, "//")
}

// trimUNC returns the UNC body with forward slashes and the leading separators
// removed. Server and share stay in the body so they remain part of the root.
func trimUNC(p string) string {
	p = strings.ReplaceAll(p, `\`, "/")
	for strings.HasPrefix(p, "/") {
		p = p[1:]
	}
	return p
}

func drivePath(p string) (letter, rest string, ok bool) {
	if len(p) < 2 || p[1] != ':' || !asciiLetter(p[0]) {
		return "", "", false
	}
	return strings.ToUpper(p[:1]), strings.ReplaceAll(p[2:], `\`, "/"), true
}

func asciiLetter(b byte) bool {
	return (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z')
}

// cleanDisplay splits a slash-separated body, drops the dots and spaces Win32
// strips, and applies "..". protect is the number of leading components that
// stay: two for a UNC root, zero for a drive.
func cleanDisplay(body string, protect int) []string {
	var parts []string
	for _, seg := range strings.Split(body, "/") {
		if seg == "" || seg == "." {
			continue
		}
		if seg == ".." {
			if len(parts) > protect {
				parts = parts[:len(parts)-1]
			}
			continue
		}
		if trimmed := strings.TrimRight(seg, ". "); trimmed != "" {
			seg = trimmed
		}
		parts = append(parts, seg)
	}
	return parts
}

// cleanPOSIX lexically cleans a path that is not a Windows drive or UNC path.
// A backslash is a character of a name, not a separator, and case is kept.
func cleanPOSIX(p string) string {
	rooted := strings.HasPrefix(p, "/")
	var parts []string
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." {
			continue
		}
		if seg == ".." {
			if len(parts) > 0 && parts[len(parts)-1] != ".." {
				parts = parts[:len(parts)-1]
				continue
			}
			if rooted {
				continue
			}
		}
		parts = append(parts, seg)
	}
	out := strings.Join(parts, "/")
	if rooted {
		return "/" + out
	}
	if out == "" {
		return "."
	}
	return out
}
