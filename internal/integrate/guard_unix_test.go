//go:build !windows

package integrate

import "os/exec"

func startGuard(script string) *exec.Cmd {
	return exec.Command("/bin/sh", "-c", script)
}
