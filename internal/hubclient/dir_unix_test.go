//go:build !windows

package hubclient

import (
	"os"
	"path/filepath"
	"testing"
)

func mode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}

func TestWriteConfigMakesItsDirectoryPrivate(t *testing.T) {
	root := t.TempDir()
	fresh := filepath.Join(root, "fresh", "iris")
	if err := WriteConfig(filepath.Join(fresh, "client.json"), FileConfig{Hub: "https://hub.example"}); err != nil {
		t.Fatal(err)
	}
	if got := mode(t, fresh); got != 0o700 {
		t.Fatalf("a new directory is %v", got)
	}
	// The join steps used to create it with `mkdir -p`, which gives 0755.
	open := filepath.Join(root, "open")
	if err := os.Mkdir(open, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(open, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := WriteConfig(filepath.Join(open, "client.json"), FileConfig{Hub: "https://hub.example"}); err != nil {
		t.Fatal(err)
	}
	if got := mode(t, open); got != 0o700 {
		t.Fatalf("an existing 0755 directory became %v", got)
	}
}

func TestPrivateDirLeavesOthersDirectoriesAlone(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	before := dirOwner
	t.Cleanup(func() { dirOwner = before })
	dirOwner = func(os.FileInfo) int { return os.Geteuid() + 1 }
	if err := privateDir(dir); err != nil {
		t.Fatal(err)
	}
	if got := mode(t, dir); got != 0o755 {
		t.Fatalf("someone else's directory was changed to %v", got)
	}
}

func TestPrivateDirReportsAPathThroughAFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := privateDir(filepath.Join(file, "iris")); err == nil {
		t.Fatal("a path through a file was accepted")
	}
}
