package doctor

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func symlinkToMissing(t *testing.T, link string) {
	t.Helper()
	err := os.Symlink(filepath.Join(t.TempDir(), "missing-generation"), link)
	if err == nil {
		return
	}
	if errors.Is(err, windows.ERROR_PRIVILEGE_NOT_HELD) {
		t.Skip("creating a symlink needs a privilege this process does not have")
	}
	t.Fatal(err)
}

func symlinkTo(t *testing.T, target, link string) {
	t.Helper()
	err := os.Symlink(target, link)
	if err == nil {
		return
	}
	if errors.Is(err, windows.ERROR_PRIVILEGE_NOT_HELD) {
		t.Skip("creating a symlink needs a privilege this process does not have")
	}
	t.Fatal(err)
}
