//go:build !windows

package gitinfo

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func readID(t *testing.T, path string) IndexID {
	t.Helper()
	id, err := ReadIndexID(path)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// One unchanged index file has one identity. Replacing it by rename, as git publishes an index,
// gives another, and so does rewriting it in place at the same size, through its last 32
// bytes, where a published index keeps its checksum (ADR 018).
func TestAnIndexIDNamesOneGeneration(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "index")
	writeFile(t, p, strings.Repeat("a", 68)+strings.Repeat("c", 32))
	first := readID(t, p)
	if readID(t, p) != first || string(first.Tail[:]) != strings.Repeat("c", 32) || first.Size != 100 {
		t.Fatalf("an unchanged index: %+v", first)
	}
	lock := filepath.Join(dir, "index.lock")
	writeFile(t, lock, strings.Repeat("a", 68)+strings.Repeat("c", 32))
	if err := os.Rename(lock, p); err != nil {
		t.Fatal(err)
	}
	replaced := readID(t, p)
	if replaced == first {
		t.Fatal("a replacement kept the identity")
	}
	writeFile(t, p, strings.Repeat("a", 68)+strings.Repeat("d", 32))
	if inPlace := readID(t, p); inPlace.Inode != replaced.Inode || inPlace.Tail == replaced.Tail || inPlace == replaced {
		t.Fatalf("an in-place rewrite: %+v after %+v", inPlace, replaced)
	}
	short := filepath.Join(dir, "short")
	writeFile(t, short, "xyz")
	if id := readID(t, short); id.Size != 3 || string(id.Tail[:3]) != "xyz" || id.Tail[3] != 0 {
		t.Fatalf("a short file: %+v", id)
	}
}

// An index that is missing, or cannot be read whole, has no identity, so no reduced snapshot
// rests on it.
func TestAnIndexThatCannotBeReadHasNoID(t *testing.T) {
	if _, err := ReadIndexID(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Fatal("a missing index has an identity")
	}
	p := filepath.Join(t.TempDir(), "index")
	writeFile(t, p, strings.Repeat("a", 64))
	failed := errors.New("injected")
	realStat, realRead := fstat, readAt
	t.Cleanup(func() { fstat, readAt = realStat, realRead })
	fstat = func(int, *unix.Stat_t) error { return failed }
	if _, err := ReadIndexID(p); !errors.Is(err, failed) {
		t.Fatalf("a failed stat: %v", err)
	}
	fstat = realStat
	readAt = func(*os.File, []byte, int64) (int, error) { return 0, io.ErrUnexpectedEOF }
	if _, err := ReadIndexID(p); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("a failed read: %v", err)
	}
}

// A git that fails after printing part of a listing, even a valid part, gives no set.
func TestAFailedListingGivesNoSet(t *testing.T) {
	fakeGit(t, "printf '160000 "+object40+" 0\\tlib\\000'\nexit 1\n")
	if got, err := Gitlinks(context.Background(), t.TempDir()); err == nil {
		t.Fatalf("a failed ls-files gave %v", got)
	}
}
