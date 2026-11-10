package gitinfo

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// runGit runs git in dir with no user or system configuration, as the submodule tests do.
func runGit(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "protocol.file.allow=always"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// gitIn is runGit for a command that must succeed.
func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := runGit(dir, args...)
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return out
}

// superproject makes a repository with one committed submodule at lib and returns its root.
func superproject(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	sub, super := filepath.Join(root, "sub"), filepath.Join(root, "super")
	for _, dir := range []string{sub, super} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		gitIn(t, dir, "init", "-q")
	}
	writeFile(t, filepath.Join(sub, "a"), "a\n")
	gitIn(t, sub, "add", "a")
	gitIn(t, sub, "commit", "-qm", "a")
	writeFile(t, filepath.Join(super, "top"), "top\n")
	gitIn(t, super, "add", "top")
	gitIn(t, super, "submodule", "-q", "add", sub, "lib")
	gitIn(t, super, "commit", "-qm", "lib")
	return super
}

func scopeStatus(t *testing.T, root string, scope Scope) map[string]string {
	t.Helper()
	got, err := StatusScope(context.Background(), root, scope, &Attempt{})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// Reduced scope hides a tracked edit inside a submodule, which full scope reports, and still
// reports what matters to the superproject: a new commit in the submodule, a gitlink staged in
// the superproject, and its own files (ADR 018).
func TestReducedScopeHidesOnlyEditsInsideSubmodules(t *testing.T) {
	super := superproject(t)
	writeFile(t, filepath.Join(super, "lib", "a"), "changed\n")
	writeFile(t, filepath.Join(super, "top"), "changed\n")
	if got := scopeStatus(t, super, FullScope); !reflect.DeepEqual(got, map[string]string{"lib": " M", "top": " M"}) {
		t.Fatalf("full: %v", got)
	}
	if got := scopeStatus(t, super, ReducedScope); !reflect.DeepEqual(got, map[string]string{"top": " M"}) {
		t.Fatalf("reduced, edit inside: %v", got)
	}
	gitIn(t, filepath.Join(super, "lib"), "commit", "-qam", "inside")
	if got := scopeStatus(t, super, ReducedScope); got["lib"] != " M" {
		t.Fatalf("reduced, new commit inside: %v", got)
	}
	gitIn(t, super, "add", "lib")
	if got := scopeStatus(t, super, ReducedScope); got["lib"] != "M " {
		t.Fatalf("reduced, gitlink staged: %v", got)
	}
}

// A status records that git started and what was left of the deadline then. One whose deadline
// has already passed never starts git, and says so.
func TestAnAttemptRecordsItsStartAndAllowance(t *testing.T) {
	root := superproject(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	a := &Attempt{}
	if _, err := StatusScope(ctx, root, FullScope, a); err != nil {
		t.Fatal(err)
	}
	if !a.Started() || a.Allowance() <= 0 || a.Allowance() > time.Minute || a.Capped() {
		t.Fatalf("started %t, allowance %v, capped %t", a.Started(), a.Allowance(), a.Capped())
	}
	without := &Attempt{}
	if _, err := StatusScope(context.Background(), root, FullScope, without); err != nil || !without.Started() || without.Allowance() != 0 {
		t.Fatalf("no deadline: started %t, allowance %v, %v", without.Started(), without.Allowance(), err)
	}
	expired, stop := context.WithCancel(context.Background())
	stop()
	late := &Attempt{}
	if _, err := StatusScope(expired, root, FullScope, late); err == nil || late.Started() {
		t.Fatalf("an ended context started git: %v", err)
	}
	// The allowance is measured from the moment git started, not from when it is read.
	deadline, _ := ctx.Deadline()
	exact := &Attempt{}
	exact.Begin(ctx, deadline.Add(-3*time.Second))
	if exact.Allowance() != 3*time.Second {
		t.Fatalf("allowance %v", exact.Allowance())
	}
}
