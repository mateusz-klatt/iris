package guard

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func denyHookCreation(t *testing.T, path, rights string) {
	t.Helper()
	before, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	prior, _, err := before.DACL()
	if err != nil {
		t.Fatal(err)
	}
	denied, err := windows.SecurityDescriptorFromString("D:P(D;;" + rights + ";;;WD)(A;OICI;FA;;;WD)")
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := denied.DACL()
	if err != nil {
		t.Fatal(err)
	}
	set := func(acl *windows.ACL) error {
		return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil)
	}
	t.Cleanup(func() {
		if err := set(prior); err != nil {
			t.Error(err)
		}
	})
	if err := set(dacl); err != nil {
		t.Fatal(err)
	}
}

func TestInstallCannotCreateTheHooksDirectoryInADeniedParent(t *testing.T) {
	dir, hook := repoHook(t)
	hooks := filepath.Dir(hook)
	if err := os.RemoveAll(hooks); err != nil {
		t.Fatal(err)
	}
	// FILE_ADD_SUBDIRECTORY denies mkdir while leaving Git's metadata readable.
	denyHookCreation(t, filepath.Dir(hooks), "0x4")
	err := Install(bg, dir, testIris)
	var pathErr *os.PathError
	if !errors.Is(err, os.ErrPermission) || !errors.As(err, &pathErr) || pathErr.Op != "mkdir" || pathErr.Path != hooks {
		t.Fatalf("denied hooks directory creation: %v", err)
	}
	if _, err := os.Stat(hooks); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed installation left a hooks directory: %v", err)
	}
}

func TestInstallCannotCreateATemporaryHookInADeniedDirectory(t *testing.T) {
	dir, hook := repoHook(t)
	hooks := filepath.Dir(hook)
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		t.Fatal(err)
	}
	// FILE_ADD_FILE denies temporary file creation without blocking directory lookup.
	denyHookCreation(t, hooks, "0x2")
	if err := Install(bg, dir, testIris); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("denied hook creation: %v", err)
	}
	if _, err := os.Stat(hook); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed installation left a hook: %v", err)
	}
	entries, err := os.ReadDir(hooks)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".iris-hook-") {
			t.Fatalf("failed installation left a temporary hook: %s", entry.Name())
		}
	}
}

func TestRemovePreservesAHookHeldWithoutDeleteSharing(t *testing.T) {
	dir, hook := repoHook(t)
	if err := Install(bg, dir, testIris); err != nil {
		t.Fatal(err)
	}
	name, err := windows.UTF16PtrFromString(hook)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := windows.CreateFile(name, windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := windows.CloseHandle(handle); err != nil {
			t.Error(err)
		}
	})
	if err := Remove(bg, dir); !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
		t.Fatalf("remove a held hook: %v", err)
	}
	if got := mustRead(t, hook); !bytes.Equal(got, wrapper(testIris, false)) {
		t.Fatalf("failed removal changed the hook: %q", got)
	}
}
