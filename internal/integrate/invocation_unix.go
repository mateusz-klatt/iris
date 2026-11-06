//go:build !windows

package integrate

import (
	"context"
	"errors"
	"os/exec"
)

func (in Invocation) start(ctx context.Context) (*exec.Cmd, error) {
	switch {
	case len(in.Argv) > 0:
		return exec.CommandContext(ctx, in.Argv[0], in.Argv[1:]...), nil
	case in.Shell != "":
		switch in.Kind {
		case ShellPowerShell:
			return exec.CommandContext(ctx, powershellExe(exec.LookPath), "-NoProfile", "-NonInteractive", "-Command", in.Shell), nil
		case ShellCmd:
			return exec.CommandContext(ctx, "cmd", "/d", "/c", in.Shell), nil
		case ShellSh, "":
			return exec.CommandContext(ctx, "/bin/sh", "-c", in.Shell), nil
		default:
			return nil, errors.New("unknown hook shell")
		}
	default:
		return nil, errors.New("hook command is empty")
	}
}
