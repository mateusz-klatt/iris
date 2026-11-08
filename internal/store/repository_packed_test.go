package store

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mateusz-klatt/iris/internal/gitinfo"
)

// A real packed checkout reproduces the missing root seen by hooks. Historical
// aliases model an already split hub: neither the path nor the remote can choose
// one of those rows, but a new session must not add another row every time.
func TestPackedCheckoutReusesRepositoryAcrossSessions(t *testing.T) {
	dir := t.TempDir()
	git := func(stdin string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Stdin = strings.NewReader(stdin)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("", "init", "--initial-branch=main")
	tree := git("", "hash-object", "-w", "-t", "tree", "--stdin")
	root := git("root\n", "-c", "user.name=Iris Test", "-c", "user.email=iris@example.test", "commit-tree", tree)
	head := git("next\n", "-c", "user.name=Iris Test", "-c", "user.email=iris@example.test", "commit-tree", tree, "-p", root)
	git("", "update-ref", "refs/heads/main", head)
	git("", "remote", "add", "origin", rewriteRemote)
	git("", "repack", "-ad")
	if _, err := os.Stat(filepath.Join(dir, ".git", "objects", head[:2], head[2:])); !os.IsNotExist(err) {
		t.Fatalf("fixture still has a loose HEAD: %v", err)
	}
	if got := git("", "rev-list", "--max-parents=0", "HEAD"); got != root {
		t.Fatalf("packed history lost its root: %q, want %q", got, root)
	}
	// Metadata discovery and registration must work without spawning Git.
	t.Setenv("PATH", t.TempDir())
	repo, err := gitinfo.Find(dir)
	if err != nil || repo.RootCommit != "" || repo.RemoteURL != rewriteRemote {
		t.Fatalf("packed hook metadata: %+v, %v", repo, err)
	}
	r := Request{Client: "codex", Host: "wsl", SessionID: "first", Cwd: dir, Worktree: repo.Worktree, CommonDir: repo.CommonDir, WorktreeID: repo.WorktreeID, RemoteURL: repo.RemoteURL, RootCommit: repo.RootCommit, Source: "hook"}
	s, clock, file := testStore(t)
	for i, commit := range []string{"historical-root", "", ""} {
		id := fmt.Sprintf("rp_legacy_%d", i)
		if _, err := s.db.Exec("INSERT INTO Repository(id,root_commit) VALUES(?,?)", id, commit); err != nil {
			t.Fatal(err)
		}
		for _, alias := range append(localRepositoryAliases(r), remoteAlias(r.RemoteURL)) {
			if _, err := s.db.Exec("INSERT INTO RepositoryAlias VALUES(?,?)", alias, id); err != nil {
				t.Fatal(err)
			}
		}
	}
	first := call(t, s, "sessions", r)
	if strings.HasPrefix(first["repository_id"].(string), "rp_legacy_") {
		t.Fatal("registration guessed an ambiguous historical identity", first)
	}
	call(t, s, "end", Request{ExecutionID: first["execution_id"].(string), Generation: first["generation"].(int64)})
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(file, clock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	for i := range 3 {
		r.SessionID = fmt.Sprintf("next-%d", i)
		got := call(t, s, "sessions", r)
		if got["repository_id"] != first["repository_id"] || got["agent"].(map[string]any)["id"] != first["agent"].(map[string]any)["id"] {
			t.Fatalf("new session split the packed checkout or its mailbox: first %v, next %v", first, got)
		}
		call(t, s, "end", Request{ExecutionID: got["execution_id"].(string), Generation: got["generation"].(int64)})
	}
	if got := count(t, s, "SELECT COUNT(*) FROM Repository"); got != 4 {
		t.Fatalf("repeat sessions created repositories: %d, want three legacy rows and one isolated checkout", got)
	}
}
