package guard

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

// denyNewFiles refuses adding files or folders to dir (0x6, FILE_ADD_FILE and
// FILE_ADD_SUBDIRECTORY) for Everyone (WD). Mode bits do nothing here, and this ACL refuses
// the administrator on the windows-latest runner too.
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
	if err := set("D:P(D;;0x6;;;WD)(A;OICI;FA;;;WD)"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = set("D:P(A;OICI;FA;;;WD)") })
}

func TestInstallCannotCreateAHookInADeniedFolder(t *testing.T) {
	dir, hook := repoHook(t)
	hooks := filepath.Dir(hook)
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		t.Fatal(err)
	}
	denyNewFiles(t, hooks)
	if err := Install(bg, dir, testIris); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("install in a folder that takes no new files: %v", err)
	}
	if _, err := os.Lstat(hook); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a failed install left a hook: %v", err)
	}
}

func TestInstallCannotUseAFileAsTheHooksDirectory(t *testing.T) {
	dir, _ := repoHook(t)
	hooks := filepath.Join(dir, "hooks-file")
	want := []byte("preserve this file")
	if err := os.WriteFile(hooks, want, 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "config", "core.hooksPath", hooks)
	err := Install(bg, dir, testIris)
	var pathErr *os.PathError
	if !errors.As(err, &pathErr) || pathErr.Op != "mkdir" || pathErr.Path != hooks {
		t.Fatalf("file used as hooks directory: %v", err)
	}
	if got := mustRead(t, hooks); !bytes.Equal(got, want) {
		t.Fatalf("failed installation changed the existing file: %q", got)
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
