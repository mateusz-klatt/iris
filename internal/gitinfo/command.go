package gitinfo

import (
	"context"
	"os/exec"
)

// gitCommand is a git command that stops at ctx together with what it started. git status starts
// another git inside each submodule, and killing only the first left that one running after the
// deadline (docs/research/adr018-git-process-index-2026-11-09.md).
func gitCommand(ctx context.Context, root string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = root
	stopDescendants(cmd)
	return cmd
}
