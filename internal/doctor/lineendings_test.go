package doctor

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// crlfWorktree commits files LF and rewrites the first crlf of them with CRLF, as Windows git
// with core.autocrlf=true checks them out, in a repository no global config reaches.
func crlfWorktree(t *testing.T, files, crlf int) string {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	root := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q")
	write := func(i int, text string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("f%d.go", i)), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for i := range files {
		write(i, "package f\n")
	}
	git("add", ".")
	git("commit", "-qm", "lf")
	for i := range crlf {
		write(i, "package f\r\n")
	}
	return root
}

// A worktree where most tracked files read as changed only because of CRLF is named, with the
// count, the cost to reconciliation and advice labelled as advice. Doctor finds the root from
// a subdirectory too.
func TestDoctorNamesACRLFWorktree(t *testing.T) {
	root := crlfWorktree(t, 30, 25)
	sub := filepath.Join(root, "sub")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	line := lineEndingsLine(context.Background(), sub)
	for _, want := range []string{"25 of 30 tracked text files", "core.autocrlf", "goes unseen", "detects nothing", "Advice", "git config core.autocrlf true"} {
		if !strings.Contains(line, want) {
			t.Fatalf("no %q in %q", want, line)
		}
	}
	if !strings.Contains(Result{LineEndings: line}.Format(), "line endings: 25 of 30") {
		t.Fatal("the warning is not printed")
	}
}

// A few CRLF files, or fewer than half, are what an editor leaves in any repository: no warning.
// Nor is there one outside a repository, or where git cannot read the index.
func TestDoctorDoesNotWarnWithoutACRLFCheckout(t *testing.T) {
	for name, dir := range map[string]string{
		"below the floor":  crlfWorktree(t, 19, 19),
		"under half":       crlfWorktree(t, 60, 25),
		"not a repository": t.TempDir(),
	} {
		if line := lineEndingsLine(context.Background(), dir); line != "" {
			t.Errorf("%s: %q", name, line)
		}
	}
	broken := crlfWorktree(t, 30, 25)
	if err := os.WriteFile(filepath.Join(broken, ".git", "index"), []byte("not an index"), 0o600); err != nil {
		t.Fatal(err)
	}
	if line := lineEndingsLine(context.Background(), broken); line != "" {
		t.Errorf("unreadable index: %q", line)
	}
	if strings.Contains(Result{}.Format(), "line endings") {
		t.Error("an empty warning is printed")
	}
}
