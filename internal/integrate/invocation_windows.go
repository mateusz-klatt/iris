//go:build windows

package integrate

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

func (in Invocation) start(ctx context.Context) (*exec.Cmd, error) {
	if len(in.Argv) > 0 {
		return exec.CommandContext(ctx, in.Argv[0], in.Argv[1:]...), nil
	}
	if in.Shell == "" {
		return nil, errors.New("hook command is empty")
	}
	switch in.Kind {
	case ShellPowerShell:
		exe := powershellExe(exec.LookPath)
		cmd := exec.CommandContext(ctx, exe)
		cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: quoteCmdExe(exe) + " -NoProfile -NonInteractive -Command " + in.Shell}
		return cmd, nil
	case ShellSh:
		return exec.CommandContext(ctx, "sh", "-c", in.Shell), nil
	case ShellCmd, "":
		comspec := os.Getenv("COMSPEC")
		if comspec == "" {
			comspec = "cmd.exe"
		}
		cmd := exec.CommandContext(ctx, comspec)
		// Clients append configured command text without a second quoting pass.
		cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: quoteCmdExe(comspec) + " /d /c " + in.Shell}
		return cmd, nil
	default:
		return nil, errors.New("unknown hook shell")
	}
}

func quoteCmdExe(s string) string {
	if strings.ContainsAny(s, " \t") {
		return `"` + s + `"`
	}
	return s
}
