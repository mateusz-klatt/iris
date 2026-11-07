package doctor

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/mateusz-klatt/iris/internal/integrate"
)

// Codex records every directory it runs in as trusted, by adding a
// [projects."<dir>"] table to its config.toml. A live run must not leave
// one behind per scratch repository, so the file is put back afterwards.

// codexTrust remembers config.toml before the run. The returned function
// restores it and says what it could not do, or returns "".
func codexTrust(l integrate.Layout, repo string) func() string {
	path, err := integrate.CodexConfig(l)
	if err != nil {
		return func() string { return "" }
	}
	before, readErr := os.ReadFile(path)
	existed := readErr == nil
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return func() string {
			return "could not read " + path + " before the run, so any trust entry Codex added for " + repo + " is left there"
		}
	}
	return func() string {
		after, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) && !existed {
			return ""
		}
		if err == nil && existed && bytes.Equal(after, before) {
			return ""
		}
		if err == nil && bytes.Equal(withoutProject(after, projectHeaders(repo)), before) {
			if existed {
				// The file exists, so WriteFile keeps its permissions.
				err = os.WriteFile(path, before, 0o600)
			} else {
				err = os.Remove(path)
			}
			if err == nil {
				return ""
			}
			return "could not restore " + path + ": " + err.Error() + "; it holds a [projects] table for " + repo
		}
		return path + " changed during the run in other ways, so it was left alone; it may hold a [projects] table for " + repo
	}
}

// projectHeaders are the spellings of the table header for dir. Codex may
// record the path as given or with symbolic links resolved, as on macOS
// where the temporary directory is under /private.
func projectHeaders(dir string) []string {
	dirs := []string{dir}
	if real, err := filepath.EvalSymlinks(dir); err == nil && real != dir {
		dirs = append(dirs, real)
	}
	var out []string
	for _, d := range dirs {
		out = append(out, "[projects."+integrate.TOMLString(d)+"]", "[projects.'"+d+"']")
	}
	return out
}

// withoutProject removes the first table whose header is one of headers,
// from its header to the next table, with the blank line that separated it,
// provided it holds nothing but what Codex writes there.
// Every other byte is kept, so comparing the result with the original file
// proves that the table was the only change.
func withoutProject(raw []byte, headers []string) []byte {
	lines := strings.SplitAfter(string(raw), "\n")
	start := -1
	for i, line := range lines {
		for _, h := range headers {
			if strings.TrimSpace(line) == h {
				start = i
			}
		}
		if start >= 0 {
			break
		}
	}
	if start < 0 {
		return raw
	}
	end := start + 1
	for end < len(lines) && !strings.HasPrefix(strings.TrimSpace(lines[end]), "[") {
		// Codex writes only trust_level. Anything else under the header,
		// a comment included, is not its own, so the table is not removed.
		if line := strings.TrimSpace(lines[end]); line != "" && !strings.HasPrefix(line, "trust_level") {
			return raw
		}
		end++
	}
	// The table's own trailing blank lines stay with it; one blank line
	// before it is the separator Codex added.
	if start > 0 && strings.TrimSpace(lines[start-1]) == "" && (end == len(lines) || strings.TrimSpace(lines[end-1]) != "") {
		start--
	}
	return []byte(strings.Join(lines[:start], "") + strings.Join(lines[end:], ""))
}
