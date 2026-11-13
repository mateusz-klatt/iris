//go:build linux || darwin

package proc

import (
	"errors"
	"os/exec"
	"syscall"
)

func detachOS(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

func terminateOS(pid int, _ int64) error {
	return terminateSignal(pid, syscall.Kill)
}

func terminateSignal(pid int, kill func(int, syscall.Signal) error) error {
	if err := kill(pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}
