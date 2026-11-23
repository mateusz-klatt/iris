//go:build !windows

package hook

import (
	"os"
	"path/filepath"
	"testing"
)

func symlinkToMissing(t *testing.T, link string) {
	t.Helper()
	if err := os.Symlink(filepath.Join(t.TempDir(), "missing-generation"), link); err != nil {
		t.Fatal(err)
	}
}
