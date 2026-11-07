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

// Windows refuses a read or a write through ACLs, not through mode bits, so the Unix permission
// tests skip here. These reach the same error paths the way a Windows user meets them: a file or
// folder whose ACL denies the access, and a name Windows cannot hold.
//
// A file held open without sharing was tried first. It was refused locally but read without error
// on the windows-latest runner, so it cannot carry a test. See denyRead for the ACL case.

// setDACL replaces path's DACL with sddl, and gives Everyone full access again before the
// temporary directory is removed.
func setDACL(t *testing.T, path, sddl string) {
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
		return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
			windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil)
	}
	if err := set(sddl); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = set("D:P(A;OICI;FA;;;WD)") })
}

// denyRead lets Everyone do anything to a file but read its contents (0x1, FILE_READ_DATA). An
// account that reads it anyway skips the test: measured on the windows-latest runner, where both
// this ACL and a file held open without sharing were read without error, while a folder that
// takes no new files was refused there too.
func denyRead(t *testing.T, file string) {
	t.Helper()
	setDACL(t, file, "D:P(D;;0x1;;;WD)(A;;FA;;;WD)")
	if _, err := os.ReadFile(file); err == nil {
		t.Skip("this account reads a file its ACL denies")
	}
}

// denyNewFiles refuses adding files or folders to dir (0x6, FILE_ADD_FILE and
// FILE_ADD_SUBDIRECTORY). WD is Everyone.
func denyNewFiles(t *testing.T, dir string) { setDACL(t, dir, "D:P(D;;0x6;;;WD)(A;OICI;FA;;;WD)") }

func TestUpdateCannotReadADeniedFile(t *testing.T) {
	dir := t.TempDir()
	l := Layout{StateDir: filepath.Join(dir, "state")}
	path := filepath.Join(dir, "config")
	seedFile(t, path, "x")
	denyRead(t, path)
	if err := l.update(Codex, path, writeNext); !errors.Is(err, os.ErrPermission) {
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

func TestUnwireCannotReadOrReplaceADeniedFile(t *testing.T) {
	l := testLayout(t)
	c, _ := ByName(Codex)
	if err := c.Install(l, nil); err != nil {
		t.Fatal(err)
	}
	files, _ := c.Files(l)
	editByHand(t, files[0])

	// Each case checks the file itself, so a case that wrongly succeeds cannot hide behind the one
	// before it.
	unchanged := func(t *testing.T) {
		t.Helper()
		if !strings.Contains(string(mustRead(t, files[0])), "--client codex") {
			t.Fatal("a failed removal changed the file")
		}
	}
	t.Run("unreadable", func(t *testing.T) {
		denyRead(t, files[0])
		if err := c.Remove(l, nil); !errors.Is(err, os.ErrPermission) {
			t.Fatal(err)
		}
	})
	unchanged(t)
	// The file can be read, but the replacement cannot be created beside it.
	t.Run("folder takes no new files", func(t *testing.T) {
		denyNewFiles(t, filepath.Dir(files[0]))
		if err := c.Remove(l, nil); !errors.Is(err, os.ErrPermission) {
			t.Fatal(err)
		}
		unchanged(t)
	})
}
