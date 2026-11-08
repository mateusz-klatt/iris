package guard

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/mateusz-klatt/iris/internal/gitinfo"
)

// The hook's cheap check must find the hook where git does, or the hint would
// nag a repository that has a guard. Status asks git; NeedsHint reads files.
func TestTheHintAgreesWithGitAboutWhereTheHookIs(t *testing.T) {
	for _, custom := range []string{"", "relative hooks", "absolute"} {
		t.Run(custom, func(t *testing.T) {
			dir := realPath(t, t.TempDir())
			git(t, dir, "init")
			git(t, dir, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-m", "init")
			linked := filepath.Join(realPath(t, t.TempDir()), "linked")
			git(t, dir, "worktree", "add", "-b", "linked", linked)
			if custom != "" {
				p := custom
				if custom == "absolute" {
					p = filepath.Join(dir, "absolute hooks")
				}
				git(t, dir, "config", "core.hooksPath", p)
			}
			ctx := context.Background()
			for _, where := range []string{dir, linked} {
				repo, err := gitinfo.Find(where)
				if err != nil {
					t.Fatal(err)
				}
				if !NeedsHint(repo) {
					t.Fatalf("%s: no hint before install", where)
				}
				if state, _, err := Status(ctx, where); err != nil || state != StateMissing {
					t.Fatalf("%s: %q %v", where, state, err)
				}
				if err = Install(ctx, where, testIris); err != nil {
					t.Fatal(err)
				}
				if NeedsHint(repo) {
					t.Fatalf("%s: hint after install", where)
				}
				state, path, err := Status(ctx, where)
				if err != nil || state != StateInstalled || filepath.Base(path) != "pre-commit" {
					t.Fatalf("%s: %q %q %v", where, state, path, err)
				}
				if err = Remove(ctx, where); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestTheHintCanBeHiddenAndSeesAForeignHook(t *testing.T) {
	dir := realPath(t, t.TempDir())
	git(t, dir, "init")
	repo, err := gitinfo.Find(dir)
	if err != nil {
		t.Fatal(err)
	}
	hook := filepath.Join(dir, ".git", "hooks", "pre-commit")
	if err = os.MkdirAll(filepath.Dir(hook), 0o755); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(hook, []byte("#!/bin/sh\nmake lint\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if !NeedsHint(repo) {
		t.Fatal("a hook without Iris is not a guard")
	}
	if state, _, err := Status(context.Background(), dir); err != nil || state != StateOther {
		t.Fatalf("%q %v", state, err)
	}
	git(t, dir, "config", HintSetting, "true")
	if !NeedsHint(repo) {
		t.Fatal("true hides nothing")
	}
	git(t, dir, "config", HintSetting, "false")
	if NeedsHint(repo) {
		t.Fatal("the setting did not hide the hint")
	}
}

func TestStatusReportsAHookItCannotRead(t *testing.T) {
	dir := realPath(t, t.TempDir())
	git(t, dir, "init")
	if err := os.MkdirAll(filepath.Join(dir, ".git", "hooks", "pre-commit"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Status(context.Background(), dir); err == nil {
		t.Fatal("a directory read as a hook")
	}
	if _, _, err := Status(context.Background(), t.TempDir()); err == nil {
		t.Fatal("a directory outside git has a guard state")
	}
}

func TestHooksDirResolvesAsGitDoes(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	if got := hooksDir("/work", "~/hooks"); got != filepath.Join(home, "hooks") {
		t.Fatal(got)
	}
	abs := filepath.Join(home, "abs")
	if got := hooksDir("/work", abs); got != abs {
		t.Fatal(got)
	}
	if got := hooksDir("/work", "rel"); got != filepath.Join("/work", "rel") {
		t.Fatal(got)
	}
	t.Setenv("HOME", "")
	t.Setenv("USERPROFILE", "")
	t.Setenv("home", "")
	// Without a home, git would fail too; the path is then simply not found.
	if got := hooksDir("/work", "~/hooks"); got != filepath.Join("/work", "~/hooks") {
		t.Fatal(got)
	}
}

func TestIsFalseFollowsGitBooleans(t *testing.T) {
	for _, v := range []string{"false", "No", "OFF", "0", ""} {
		if !isFalse(v) {
			t.Fatal(v)
		}
	}
	for _, v := range []string{"true", "yes", "1", "on"} {
		if isFalse(v) {
			t.Fatal(v)
		}
	}
}
