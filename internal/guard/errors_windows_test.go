package guard

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

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
