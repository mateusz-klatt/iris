package gitinfo

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// Status must not write the index. git status normally refreshes a stale index, holding
// .git/index.lock while it writes; the hook kills git at its 300 ms budget, and a git killed
// then left the lock behind, so the user's next commit failed (measured: 6 of 40 killed runs).
// Not writing the index is what --no-optional-locks guarantees, so that is what is checked.
func TestStatusDoesNotRewriteTheIndex(t *testing.T) {
	repo := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	file := filepath.Join(repo, "a.txt")
	if err := os.WriteFile(file, []byte("a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("add", "a.txt")
	run("-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-qm", "init")
	// A changed mtime with the same content makes the index stale: plain status would refresh it.
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(file, later, later); err != nil {
		t.Fatal(err)
	}
	index := filepath.Join(repo, ".git", "index")
	before, err := os.ReadFile(index)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Status(context.Background(), repo); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(index)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("Status rewrote the index, so a killed Status can leave index.lock behind")
	}
}
