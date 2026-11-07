//go:build windows

package integrate

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// Windows refuses a read or a write through the sharing mode of another process's handle and
// through ACLs, not through mode bits, so the Unix permission tests skip here. These reach the
// same error paths the way a Windows user meets them: a file held open by an editor or a
// scanner, a folder whose ACL forbids new files, a name Windows cannot hold.

// lock opens path the way another process would, sharing only what share allows, until the test
// ends.
func lock(t *testing.T, path string, share uint32) {
	t.Helper()
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	h, err := windows.CreateFile(p, windows.GENERIC_READ, share, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = windows.CloseHandle(h) })
}

// denyNewFiles gives dir a DACL that refuses adding files or folders to it, and restores full
// access before the temporary directory is removed.
func denyNewFiles(t *testing.T, dir string) {
	t.Helper()
	set := func(sddl string) error {
		sd, err := windows.SecurityDescriptorFromString(sddl)
		if err != nil {
			return err
		}
		dacl, _, err := sd.DACL()
		if err != nil {
			return err
		}
		return windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT,
			windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil)
	}
	// 0x6 is FILE_ADD_FILE and FILE_ADD_SUBDIRECTORY; WD is Everyone.
	if err := set("D:P(D;;0x6;;;WD)(A;OICI;FA;;;WD)"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = set("D:P(A;OICI;FA;;;WD)") })
}

func TestUpdateCannotReadALockedFile(t *testing.T) {
	dir := t.TempDir()
	l := Layout{StateDir: filepath.Join(dir, "state")}
	path := filepath.Join(dir, "config")
	seedFile(t, path, "x")
	lock(t, path, 0)
	if err := l.update(Codex, path, writeNext); !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
		t.Fatal(err)
	}
}

// A name Windows cannot hold fails Lstat with an error that is not "does not exist", so it is
// refused instead of being treated as a file to create.
func TestANameWindowsCannotHoldIsRefused(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "a<b", "config")
	if err := (Layout{StateDir: filepath.Join(dir, "state")}).update(Codex, bad, writeNext); !errors.Is(err, windows.ERROR_INVALID_NAME) {
		t.Fatalf("update: %v", err)
	}
	if err := unwireFile(filepath.Join(dir, "state"), bad, manifestFile{Client: Codex}, stripClient(Codex)); !errors.Is(err, windows.ERROR_INVALID_NAME) {
		t.Fatalf("unwire: %v", err)
	}
}

func TestAtomicWriteCannotCreateInADeniedFolder(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "denied")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	denyNewFiles(t, dir)
	if err := atomicWrite(filepath.Join(dir, "config"), []byte("x")); !errors.Is(err, os.ErrPermission) {
		t.Fatal(err)
	}
}

func TestUnwireCannotReadOrReplaceALockedFile(t *testing.T) {
	l := testLayout(t)
	c, _ := ByName(Codex)
	if err := c.Install(l, nil); err != nil {
		t.Fatal(err)
	}
	files, _ := c.Files(l)
	editByHand(t, files[0])

	t.Run("held without sharing", func(t *testing.T) {
		lock(t, files[0], 0)
		if err := c.Remove(l, nil); !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
			t.Fatal(err)
		}
	})
	// Shared for reading only: Iris can read the file but cannot replace it.
	t.Run("held for reading", func(t *testing.T) {
		lock(t, files[0], windows.FILE_SHARE_READ)
		if err := c.Remove(l, nil); !errors.Is(err, os.ErrPermission) {
			t.Fatal(err)
		}
	})
	if !strings.Contains(string(mustRead(t, files[0])), "--client codex") {
		t.Fatal("a failed removal changed the file")
	}
}
