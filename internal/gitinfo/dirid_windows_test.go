package gitinfo

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// One directory reached by two paths, here the same path spelt in other case, which Windows
// treats as one directory, has one id; another directory has another, and one that cannot be
// opened has none (D6).
func TestAWorktreeHasOneIDThroughEveryPath(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	if out, err := exec.Command("git", "init", "-q", repo).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	direct, err := Find(repo)
	if err != nil {
		t.Fatal(err)
	}
	through, err := Find(strings.ToUpper(repo))
	if err != nil {
		t.Fatal(err)
	}
	if direct.WorktreeID == "" || direct.WorktreeID != through.WorktreeID || direct.Worktree == through.Worktree {
		t.Fatalf("direct %q at %s, in other case %q at %s", direct.WorktreeID, direct.Worktree, through.WorktreeID, through.Worktree)
	}
	if other := directoryID(root); other == direct.WorktreeID || other == "" {
		t.Fatalf("another directory has id %q", other)
	}
	if missing := directoryID(filepath.Join(root, "missing")); missing != "" {
		t.Fatalf("a missing directory has id %q", missing)
	}
	if bad := directoryID("bad\x00path"); bad != "" {
		t.Fatalf("an unusable path has id %q", bad)
	}
}

func openDir(t *testing.T, path string) windows.Handle {
	t.Helper()
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	h, err := windows.CreateFile(name, 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = windows.CloseHandle(h) })
	return h
}

// The 128-bit file id names a directory where the file system has one. On NTFS it is the 64-bit
// index widened, and its volume serial widens the 32-bit one, so both readers name the same
// directory; on ReFS and Dev Drives only the 128-bit id is unique.
func TestTheWideFileIDNamesADirectoryAndAgreesWithTheIndex(t *testing.T) {
	dir := t.TempDir()
	wide, narrow := fileID128(openDir(t, dir)), fileID64(openDir(t, dir))
	if !strings.HasPrefix(directoryID(dir), "v128:") || directoryID(dir) != wide {
		t.Fatalf("directoryID %q, 128-bit %q", directoryID(dir), wide)
	}
	var serial, id, index string
	if _, err := fmt.Sscanf(strings.ReplaceAll(wide, ":", " "), "v128 %s %s", &serial, &id); err != nil {
		t.Fatalf("%q: %v", wide, err)
	}
	if parts := strings.Split(narrow, ":"); len(parts) == 2 {
		index = parts[1]
	}
	// FILE_ID_INFO holds the id little-endian: its first 8 bytes are the 64-bit index.
	raw, err := hex.DecodeString(id)
	if err != nil || len(raw) != 16 {
		t.Fatalf("id %q: %v", id, err)
	}
	low := binary.LittleEndian.Uint64(raw[:8])
	if fmt.Sprintf("%016x", low) != index || !strings.HasSuffix(serial, strings.Split(narrow, ":")[0]) {
		t.Fatalf("128-bit %q does not widen 64-bit %q", wide, narrow)
	}
}

// A file system without the 128-bit id, such as FAT, falls back to the 64-bit index, which
// still names the directory the same through every path; with neither there is no id.
func TestWithoutTheWideIDTheIndexStillNamesTheDirectory(t *testing.T) {
	dir := t.TempDir()
	none := func(windows.Handle) string { return "" }
	real := idReaders
	t.Cleanup(func() { idReaders = real })
	idReaders = []func(windows.Handle) string{none, fileID64}
	id := directoryID(dir)
	if id == "" || strings.HasPrefix(id, "v128:") || directoryID(strings.ToUpper(dir)) != id {
		t.Fatalf("fallback id %q, upper-case path %q", id, directoryID(strings.ToUpper(dir)))
	}
	idReaders = []func(windows.Handle) string{none, none}
	if id := directoryID(dir); id != "" {
		t.Fatalf("no reader, id %q", id)
	}
	if fileID128(windows.InvalidHandle) != "" || fileID64(windows.InvalidHandle) != "" {
		t.Fatal("an invalid handle has an id")
	}
}
