//go:build !linux

package proc

import (
	"context"
	"os/exec"
)

type exeHandle struct{}

// Close releases any open executable descriptor held by e.
func (*ProcessExecutable) Close() error { return nil }

// Command prepares cmd to execute e with args.
func (e *ProcessExecutable) Command(ctx context.Context, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, e.Path, args...)
}

func openDeletedExeOS(int, int64, string, func(int) (Snapshot, bool)) (*ProcessExecutable, error) {
	return nil, ErrExecutableDeleted
}
