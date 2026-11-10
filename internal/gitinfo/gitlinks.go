package gitinfo

import (
	"bytes"
	"context"
	"errors"
	"sort"
)

// The bounds on reading an index's gitlinks for a reduced snapshot (ADR 018). Past any of them
// the set is not known, and an unknown set is never sent as an empty one.
const (
	gitlinkRecordLimit = 8 << 10  // one ls-files record: its header, TAB and path
	gitlinkOutputLimit = 64 << 20 // the whole listing
	// GitlinkLimit is the most submodule paths a reduced snapshot may name, the hub's own bound.
	GitlinkLimit = 500
)

var (
	errGitlinkRecord = errors.New("invalid git ls-files record")
	errGitlinkLimit  = errors.New("git ls-files output exceeds its limit")
)

// Gitlinks lists the submodule paths recorded in the index of the worktree at root: every path
// whose entry has mode 160000, at any stage, so a conflicted gitlink is in it too. They are
// sorted in byte order and without repeats, the form the hub requires. The listing of
// `git ls-files --stage -z` is read record by record as git writes it, so a large index is never
// held whole. Without --sparse, a sparse index is listed expanded. A malformed, unterminated or
// oversized listing, or a git that fails, is an error, never an empty set.
func Gitlinks(ctx context.Context, root string) ([]string, error) {
	cmd := gitCommand(ctx, root, "--no-optional-locks", "ls-files", "--stage", "-z")
	var listing gitlinkListing
	cmd.Stdout = &listing
	if err := run(ctx, cmd); err != nil {
		return nil, err
	}
	return listing.paths()
}

// gitlinkListing parses ls-files output as os/exec copies it in. record holds the one record
// not yet ended by its NUL.
type gitlinkListing struct {
	record []byte
	total  int
	found  map[string]bool
}

func (l *gitlinkListing) Write(p []byte) (int, error) {
	if l.total += len(p); l.total > gitlinkOutputLimit {
		return 0, errGitlinkLimit
	}
	for rest := p; len(rest) > 0; {
		end := bytes.IndexByte(rest, 0)
		if end < 0 {
			end = len(rest)
		}
		if l.record = append(l.record, rest[:end]...); len(l.record) > gitlinkRecordLimit {
			return 0, errGitlinkLimit
		}
		if end == len(rest) {
			break
		}
		if err := l.take(l.record); err != nil {
			return 0, err
		}
		l.record, rest = l.record[:0], rest[end+1:]
	}
	return len(p), nil
}

// take reads one record: "mode SP object SP stage TAB path", split at the first TAB, since a
// path may hold spaces and tabs of its own.
func (l *gitlinkListing) take(record []byte) error {
	header, path, ok := bytes.Cut(record, []byte{'\t'})
	fields := bytes.Split(header, []byte{' '})
	if !ok || len(path) == 0 || len(fields) != 3 || !indexModes[string(fields[0])] || !object(fields[1]) || !digits(fields[2], 1, "0123") {
		return errGitlinkRecord
	}
	if string(fields[0]) != "160000" {
		return nil
	}
	if l.found == nil {
		l.found = map[string]bool{}
	}
	if l.found[string(path)] = true; len(l.found) > GitlinkLimit {
		return errGitlinkLimit
	}
	return nil
}

// indexModes are the modes an index entry can have: a file, an executable file, a symbolic link
// and a gitlink. Any other, such as the sparse directory that ls-files prints only with --sparse,
// means the listing is not one this reads, and a mode it skipped could have hidden a gitlink.
var indexModes = map[string]bool{"100644": true, "100755": true, "120000": true, "160000": true}

// paths is the set once git has finished. Output that does not end with a NUL was cut short.
func (l *gitlinkListing) paths() ([]string, error) {
	if len(l.record) != 0 {
		return nil, errGitlinkRecord
	}
	out := make([]string, 0, len(l.found))
	for p := range l.found {
		out = append(out, p)
	}
	sort.Strings(out)
	return out, nil
}

// object is a SHA-1 or SHA-256 object name, as git prints one.
func object(b []byte) bool {
	return (len(b) == 40 || len(b) == 64) && digits(b, len(b), "0123456789abcdef")
}

// digits is whether b is n characters, each one of set.
func digits(b []byte, n int, set string) bool {
	if len(b) != n {
		return false
	}
	for _, c := range b {
		if !bytes.ContainsRune([]byte(set), rune(c)) {
			return false
		}
	}
	return true
}
