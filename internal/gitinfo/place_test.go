package gitinfo

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// Place answers the innermost checkout at or above a directory: the known one when the walk
// reaches it, a nested one by its .git entry, and the known one for a directory in none or one
// whose checkout cannot be read. It reads each checkout once per cache.
func TestPlaceFindsTheInnermostCheckout(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{filepath.Join(root, "src"), filepath.Join(root, "frontend", "src", "deep"), filepath.Join(root, "broken", "x")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, git := range []string{filepath.Join(root, ".git"), filepath.Join(root, "frontend", ".git"), filepath.Join(root, "broken", ".git")} {
		if err := os.WriteFile(git, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	known := Repo{Worktree: root, GitDir: filepath.Join(root, ".git")}
	reads := 0
	find := func(dir string) (Repo, error) {
		reads++
		if filepath.Base(dir) == "broken" {
			return Repo{}, errors.New("unreadable")
		}
		return Repo{Worktree: dir, GitDir: filepath.Join(dir, ".git")}, nil
	}
	places := map[string]Repo{}
	if got := Place(filepath.Join(root, "src"), known, find, places); got.Worktree != root || reads != 0 {
		t.Fatalf("the known checkout's directory: %+v after %d reads", got, reads)
	}
	sub := filepath.Join(root, "frontend")
	for _, dir := range []string{filepath.Join(sub, "src", "deep"), filepath.Join(sub, "src")} {
		if got := Place(dir, known, find, places); got.Worktree != sub {
			t.Fatalf("%s: %+v", dir, got)
		}
	}
	if reads != 1 {
		t.Fatalf("the submodule was read %d times", reads)
	}
	if got := Place(filepath.Join(root, "broken", "x"), known, find, nil); got.Worktree != root {
		t.Fatalf("an unreadable nested checkout: %+v", got)
	}
	if got := Place(filepath.Join(t.TempDir(), "elsewhere"), known, find, nil); got.Worktree != root {
		t.Fatalf("a directory in no checkout: %+v", got)
	}
}
