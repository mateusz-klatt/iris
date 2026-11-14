//go:build linux || darwin

package proc

import (
	"errors"
	"os/exec"
	"syscall"
)

func detachUnderOS(cmd *exec.Cmd, pid int, startMS int64) (func(), error) {
	if snap, ok := lookupOS(pid); !ok || snap.StartMS != startMS {
		return nil, ErrNotFound
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return func() {}, nil
}

func noConsoleOS(*exec.Cmd) {}

func terminateOS(pid int, _ int64) error {
	return terminateSignal(pid, syscall.Kill)
}

func terminateSignal(pid int, kill func(int, syscall.Signal) error) error {
	if err := kill(pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}
