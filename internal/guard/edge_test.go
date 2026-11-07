package guard

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/mateusz-klatt/iris/internal/gitinfo"
)

var bg = context.Background()

func repoHook(t *testing.T) (dir, hook string) {
	t.Helper()
	dir = realPath(t, t.TempDir())
	git(t, dir, "init")
	hook, err := hookPath(bg, dir)
	if err != nil {
		t.Fatal(err)
	}
	return dir, hook
}

// brokenRepo has a .git that Find accepts and git itself refuses.
func brokenRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(dir))
	return dir
}

func needsPermissions(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" || os.Getuid() == 0 {
		t.Skip("directory modes are not enforced here")
	}
}

func needsSymlinks(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need a privilege on Windows")
	}
}

func seed(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

func replaceRename(t *testing.T, fn func(from, to string) error) {
	t.Helper()
	rename = fn
	t.Cleanup(func() { rename = os.Rename })
}

func TestEveryCommandRefusesADirectoryOutsideGit(t *testing.T) {
	dir := t.TempDir()
	if err := Install(bg, dir); !errors.Is(err, gitinfo.ErrNotRepo) {
		t.Fatal(err)
	}
	if err := Remove(bg, dir); !errors.Is(err, gitinfo.ErrNotRepo) {
		t.Fatal(err)
	}
	if _, err := Check(bg, dir, nil); !errors.Is(err, gitinfo.ErrNotRepo) {
		t.Fatal(err)
	}
}

func TestGitFailureIsReported(t *testing.T) {
	dir := brokenRepo(t)
	if err := Install(bg, dir); err == nil || !strings.Contains(err.Error(), "find Git hooks directory") {
		t.Fatal(err)
	}
	if _, err := Check(bg, dir, nil); err == nil {
		t.Fatal("Check ignored a failing git")
	}
}

func TestInstallRefusesAHookItCannotRead(t *testing.T) {
	dir, hook := repoHook(t)
	if err := os.MkdirAll(hook, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Install(bg, dir); err == nil {
		t.Fatal("no error")
	}
	if err := Remove(bg, dir); err == nil {
		t.Fatal("no error")
	}
}

func TestInstallRefusesASymlink(t *testing.T) {
	needsSymlinks(t)
	dir, hook := repoHook(t)
	target := filepath.Join(t.TempDir(), "shared")
	seed(t, target, "#!/bin/sh\n")
	if err := os.MkdirAll(filepath.Dir(hook), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, hook); err != nil {
		t.Fatal(err)
	}
	if err := Install(bg, dir); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatal(err)
	}
}

func TestInstallRefusesToReplaceABackup(t *testing.T) {
	dir, hook := repoHook(t)
	seed(t, hook, "#!/bin/sh\nmine\n")
	seed(t, hook+".iris-original", "older\n")
	if err := Install(bg, dir); err == nil || !strings.Contains(err.Error(), "backup already exists") {
		t.Fatal(err)
	}
	if got := mustRead(t, hook); string(got) != "#!/bin/sh\nmine\n" {
		t.Fatalf("%q", got)
	}
}

func TestInstallCannotCreateTheHooksDirectory(t *testing.T) {
	needsPermissions(t)
	dir, hook := repoHook(t)
	hooks := filepath.Dir(hook)
	if err := os.RemoveAll(hooks); err != nil {
		t.Fatal(err)
	}
	parent := filepath.Dir(hooks)
	if err := os.Chmod(parent, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o755) })
	if err := Install(bg, dir); !errors.Is(err, os.ErrPermission) {
		t.Fatal(err)
	}
}

func TestInstallCannotWriteInTheHooksDirectory(t *testing.T) {
	needsPermissions(t)
	dir, hook := repoHook(t)
	hooks := filepath.Dir(hook)
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(hooks, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(hooks, 0o755) })
	if err := Install(bg, dir); err == nil {
		t.Fatal("no error")
	}
}

func TestInstallLeavesTheOriginalInPlaceWhenBackupFails(t *testing.T) {
	dir, hook := repoHook(t)
	seed(t, hook, "#!/bin/sh\nmine\n")
	failure := errors.New("no rename")
	replaceRename(t, func(string, string) error { return failure })
	if err := Install(bg, dir); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	if got := mustRead(t, hook); string(got) != "#!/bin/sh\nmine\n" {
		t.Fatalf("%q", got)
	}
}

func TestInstallRollsBackWhenTheNewHookCannotBePlaced(t *testing.T) {
	dir, hook := repoHook(t)
	original := "#!/bin/sh\nmine\n"
	seed(t, hook, original)
	failure := errors.New("no rename")
	replaceRename(t, func(from, to string) error {
		if to == hook && !strings.HasSuffix(from, ".iris-original") {
			return failure
		}
		return os.Rename(from, to)
	})
	if err := Install(bg, dir); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	if got := mustRead(t, hook); string(got) != original {
		t.Fatalf("%q", got)
	}
	if _, err := os.Stat(hook + ".iris-original"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("backup left behind: %v", err)
	}
}

func TestInstallWithoutAHookHasNothingToRollBack(t *testing.T) {
	dir, hook := repoHook(t)
	failure := errors.New("no rename")
	replaceRename(t, func(string, string) error { return failure })
	if err := Install(bg, dir); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	if _, err := os.Stat(hook); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("hook appeared: %v", err)
	}
}

func TestRemoveRefusesAChainWithoutItsOriginal(t *testing.T) {
	dir, hook := repoHook(t)
	if err := os.MkdirAll(filepath.Dir(hook), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hook, wrapper(true), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Remove(bg, dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if !bytes.Equal(mustRead(t, hook), wrapper(true)) {
		t.Fatal("hook was removed with nothing to restore")
	}
}

func TestRemoveCannotDeleteTheHook(t *testing.T) {
	needsPermissions(t)
	dir, hook := repoHook(t)
	if err := Install(bg, dir); err != nil {
		t.Fatal(err)
	}
	hooks := filepath.Dir(hook)
	if err := os.Chmod(hooks, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(hooks, 0o755) })
	if err := Remove(bg, dir); err == nil {
		t.Fatal("no error")
	}
}

func TestCheckReportsAMissingHostname(t *testing.T) {
	dir, _ := repoHook(t)
	seed(t, filepath.Join(dir, "a.txt"), "x")
	git(t, dir, "add", "a.txt")
	failure := errors.New("no hostname")
	hostname = func() (string, error) { return "", failure }
	t.Cleanup(func() { hostname = os.Hostname })
	if _, err := Check(bg, dir, nil); !errors.Is(err, failure) {
		t.Fatal(err)
	}
}

func TestInstalledHookRunsOnlyForItsOwner(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no such mode")
	}
	dir, hook := repoHook(t)
	if err := Install(bg, dir); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(hook)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Fatalf("mode %o", got)
	}
}
