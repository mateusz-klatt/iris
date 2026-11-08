package gitinfo

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Status leaves out files that are only untracked inside a submodule, and still reports a
// submodule whose tracked content changed. Scanning every submodule for untracked files is what
// took git status past the hook's budget on a repository with large submodules (5.5 s against
// 20 ms without it), and a reconcile past its budget detects nothing at all.
func TestStatusSkipsUntrackedFilesInSubmodulesOnly(t *testing.T) {
	root := t.TempDir()
	git := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "protocol.file.allow=always"}, args...)...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	write := func(path, text string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	sub, super := filepath.Join(root, "sub"), filepath.Join(root, "super")
	for _, dir := range []string{sub, super} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		git(dir, "init", "-q")
	}
	write(filepath.Join(sub, "a"), "a\n")
	git(sub, "add", "a")
	git(sub, "commit", "-qm", "a")
	git(super, "submodule", "-q", "add", sub, "lib")
	git(super, "commit", "-qm", "lib")

	write(filepath.Join(super, "lib", "new"), "untracked\n")
	write(filepath.Join(super, "top"), "untracked\n")
	got, err := Status(context.Background(), super)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got["top"] != "??" {
		t.Fatalf("with only an untracked file in the submodule: %v", got)
	}
	write(filepath.Join(super, "lib", "a"), "changed\n")
	if got, err = Status(context.Background(), super); err != nil || got["lib"] != " M" || got["top"] != "??" {
		t.Fatalf("with a tracked change in the submodule: %v, %v", got, err)
	}
}
