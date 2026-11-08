package gitinfo

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// One directory reached by two paths, here the same path spelt in other case, which Windows
// treats as one directory, has one id; another directory has another, and one that cannot be
// opened has none (D6).
func TestAWorktreeHasOneIDThroughEveryPath(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	if out, err := exec.Command("git", "init", "-q", repo).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	direct, err := Find(repo)
	if err != nil {
		t.Fatal(err)
	}
	through, err := Find(strings.ToUpper(repo))
	if err != nil {
		t.Fatal(err)
	}
	if direct.WorktreeID == "" || direct.WorktreeID != through.WorktreeID || direct.Worktree == through.Worktree {
		t.Fatalf("direct %q at %s, in other case %q at %s", direct.WorktreeID, direct.Worktree, through.WorktreeID, through.Worktree)
	}
	if other := directoryID(root); other == direct.WorktreeID || other == "" {
		t.Fatalf("another directory has id %q", other)
	}
	if missing := directoryID(filepath.Join(root, "missing")); missing != "" {
		t.Fatalf("a missing directory has id %q", missing)
	}
	if bad := directoryID("bad\x00path"); bad != "" {
		t.Fatalf("an unusable path has id %q", bad)
	}
}
