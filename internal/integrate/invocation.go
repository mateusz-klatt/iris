package integrate

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
)

// Ran is what one hook command did: its output on each stream and its exit code.
type Ran struct {
	Stdout, Stderr []byte
	Exit           int
}

// Run executes the configured hook command with stdin, and env added to this process's
// environment. A shell command is run by its client's configured shell. The exit code is
// returned even when the process exits non-zero. A missing binary is an error.
func (in Invocation) Run(ctx context.Context, stdin []byte, env ...string) (Ran, error) {
	cmd, err := in.start(ctx)
	if err != nil {
		return Ran{Exit: -1}, err
	}
	cmd.Stdin = bytes.NewReader(stdin)
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err = cmd.Run()
	ran := Ran{Stdout: out.Bytes(), Stderr: errb.Bytes()}
	if err == nil {
		return ran, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		ran.Exit = exit.ExitCode()
		return ran, nil
	}
	ran.Exit = -1
	return ran, err
}

// powershellExe prefers PowerShell 7 and falls back to Windows PowerShell.
func powershellExe(look func(string) (string, error)) string {
	if p, err := look("pwsh"); err == nil {
		return p
	}
	return "powershell"
}
