package integrate

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
)

// Run executes the configured hook command with stdin. A shell command is
// run by /bin/sh or cmd. The exit code is returned even when the process
// exits non-zero. A missing binary is an error.
func (in Invocation) Run(ctx context.Context, stdin []byte) ([]byte, int, error) {
	cmd, err := in.start(ctx)
	if err != nil {
		return nil, -1, err
	}
	cmd.Stdin = bytes.NewReader(stdin)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err = cmd.Run()
	if err == nil {
		return out.Bytes(), 0, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return out.Bytes(), exit.ExitCode(), nil
	}
	return out.Bytes(), -1, err
}
