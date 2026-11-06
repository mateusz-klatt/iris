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
	switch {
	case len(in.Argv) > 0:
		return exec.CommandContext(ctx, in.Argv[0], in.Argv[1:]...), nil
	case in.Shell != "" && in.Comspec:
		comspec := os.Getenv("COMSPEC")
		if comspec == "" {
			comspec = "cmd.exe"
		}
		cmd := exec.CommandContext(ctx, comspec)
		// Codex appends the configured text to cmd.exe /C without a second quoting pass.
		cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: quoteCmdExe(comspec) + " /C " + in.Shell}
		return cmd, nil
	case in.Shell != "":
		return exec.CommandContext(ctx, "cmd", "/c", in.Shell), nil
	default:
		return nil, errors.New("hook command is empty")
	}
}

func quoteCmdExe(s string) string {
	if strings.ContainsAny(s, " \t") {
		return `"` + s + `"`
	}
	return s
}
