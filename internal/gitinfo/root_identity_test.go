package gitinfo

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Registration must treat an unreadable root as unknown, rather than mistake a
// visible recent commit for the root and split the same history between hosts.
func TestLongLooseHistoryKeepsRootUnknown(t *testing.T) {
	root, git := gitDirWith(t, "ref: refs/heads/main\n")
	first := fmt.Sprintf("%040x", 1)
	previous := ""
	for i := 1; i <= 129; i++ {
		commit := fmt.Sprintf("%040x", i)
		body := "tree x\n"
		if previous != "" {
			body += "parent " + previous + "\n"
		}
		writeLoose(t, git, commit, body+"\ncommit\n")
		previous = commit
	}
	writeFile(t, filepath.Join(git, "refs", "heads", "main"), previous+"\n")
	writeFile(t, filepath.Join(git, "config"), "[remote \"origin\"]\nurl = https://example.test/project.git\n")
	if got := rootCommit(git, first); got != first {
		t.Fatalf("the loose root itself must remain readable: %q", got)
	}
	repo, err := Find(root)
	if err != nil || repo.RootCommit != "" || repo.RemoteURL != "https://example.test/project.git" || repo.Branch != "main" {
		t.Fatalf("long history must retain the remote but report no root: %+v, %v", repo, err)
	}
}

func TestPackedHistoryKeepsRootUnknown(t *testing.T) {
	dir := t.TempDir()
	run := func(stdin string, args ...string) string {
		t.Helper()
		command := exec.Command("git", append([]string{"-C", dir}, args...)...)
		command.Stdin = strings.NewReader(stdin)
		out, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("", "init", "--initial-branch=main")
	tree := run("", "hash-object", "-w", "-t", "tree", "--stdin")
	commit := run("root\n", "-c", "user.name=Iris Test", "-c", "user.email=iris@example.test", "commit-tree", tree)
	run("", "update-ref", "refs/heads/main", commit)
	run("", "remote", "add", "origin", "https://example.test/project.git")
	loose, err := Find(dir)
	if err != nil || loose.RootCommit != commit {
		t.Fatalf("loose history root: %+v, %v; want %s", loose, err, commit)
	}
	run("", "repack", "-ad")
	if _, err := os.Stat(filepath.Join(dir, ".git", "objects", commit[:2], commit[2:])); !os.IsNotExist(err) {
		t.Fatalf("fixture must have no loose commit: %v", err)
	}
	if packs, err := filepath.Glob(filepath.Join(dir, ".git", "objects", "pack", "*.pack")); err != nil || len(packs) == 0 {
		t.Fatalf("fixture must have packed history: %v, %v", packs, err)
	}
	packed, err := Find(dir)
	if err != nil || packed.RootCommit != "" || packed.Branch != loose.Branch || packed.RemoteURL != loose.RemoteURL {
		t.Fatalf("packed history must preserve its known metadata and report no root: %+v, %v", packed, err)
	}
	if got := run("", "rev-list", "--max-parents=0", "HEAD"); got != commit {
		t.Fatalf("packing must leave the Git root unchanged: %q; want %q", got, commit)
	}
}
