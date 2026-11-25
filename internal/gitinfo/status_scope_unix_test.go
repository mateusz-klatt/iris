//go:build !windows

package gitinfo

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Each scope passes its own --ignore-submodules mode, and reduced never passes all, which hides
// even a submodule commit staged in the superproject (ADR 018).
func TestEachScopeAsksGitForItsMode(t *testing.T) {
	args := filepath.Join(t.TempDir(), "args")
	fakeGit(t, "printf '%s\\n' \"$@\" > '"+args+"'\n")
	for scope, want := range map[Scope]string{FullScope: "--ignore-submodules=untracked", ReducedScope: "--ignore-submodules=dirty"} {
		if _, err := StatusScope(context.Background(), t.TempDir(), scope, &Attempt{}); err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(args)
		if err != nil {
			t.Fatal(err)
		}
		got := strings.Fields(string(raw))
		if got[len(got)-1] != want || strings.Contains(string(raw), "--ignore-submodules=all") {
			t.Fatalf("scope %d: %v", scope, got)
		}
	}
}

// A status whose output passes the cap fails with ErrStatusTooLarge even when git exits with
// SIGPIPE, and its attempt says the cap ended it, so the breaker never reads it as a slow git.
func TestACappedStatusSaysSo(t *testing.T) {
	for _, size := range []string{"60000", "262144"} {
		t.Run(size, func(t *testing.T) {
			fakeGit(t, "head -c "+size+" /dev/zero\n")
			a := &Attempt{}
			_, err := StatusScope(context.Background(), t.TempDir(), FullScope, a)
			if !errors.Is(err, ErrStatusTooLarge) || !a.Started() || !a.Capped() {
				t.Fatalf("size %s: started %t, capped %t: %v", size, a.Started(), a.Capped(), err)
			}
		})
	}
	fakeGit(t, "exit 1\n")
	a := &Attempt{}
	_, err := StatusScope(context.Background(), t.TempDir(), FullScope, a)
	var exited *exec.ExitError
	if !errors.As(err, &exited) || errors.Is(err, ErrStatusTooLarge) || a.Capped() {
		t.Fatalf("ordinary exit failure: capped %t: %v", a.Capped(), err)
	}
}
