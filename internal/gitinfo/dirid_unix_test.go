//go:build !windows

package gitinfo

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// A worktree reached by two paths, here through a symlink as a bind mount would give it, is
// one directory and has one id; another directory has another, and one that cannot be read
// has none (D6).
func TestAWorktreeHasOneIDThroughEveryPath(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	if out, err := exec.Command("git", "init", "-q", repo).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(repo, alias); err != nil {
		t.Fatal(err)
	}
	direct, err := Find(repo)
	if err != nil {
		t.Fatal(err)
	}
	through, err := Find(alias)
	if err != nil {
		t.Fatal(err)
	}
	if direct.WorktreeID == "" || direct.WorktreeID != through.WorktreeID || direct.Worktree == through.Worktree {
		t.Fatalf("direct %q at %s, through the alias %q at %s", direct.WorktreeID, direct.Worktree, through.WorktreeID, through.Worktree)
	}
	if other := directoryID(root); other == direct.WorktreeID || other == "" {
		t.Fatalf("another directory has id %q", other)
	}
	if missing := directoryID(filepath.Join(root, "missing")); missing != "" {
		t.Fatalf("a missing directory has id %q", missing)
	}
}
