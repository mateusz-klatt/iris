package gitinfo

import (
	"context"
	"os/exec"
)

// gitCommand is a git command that stops at ctx together with what it started. git status starts
// another git inside each submodule, and killing only the first left that one running after the
// deadline (docs/research/adr018-git-process-index-2026-11-09.md).
//
// git is found through PATH, as the user's shell finds it. This package does not check which git
// that is.
func gitCommand(ctx context.Context, root string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", args...) // NOSONAR: go:S4036; the user's git is found through PATH on purpose.
	cmd.Dir = root
	stopDescendants(cmd)
	return cmd
}
