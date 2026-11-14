//go:build linux || darwin

package proc

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// openDevNull opens the null device for a detached process's standard streams. Tests replace it.
var openDevNull = func() (*os.File, error) { return os.OpenFile(os.DevNull, os.O_RDWR, 0) }

func startDetachedOS(exe string, args, env []string, pid int, startMS int64) (Detached, error) {
	if snap, ok := lookupOS(pid); !ok || snap.StartMS != startMS {
		return Detached{}, ErrNotFound
	}
	devNull, err := openDevNull()
	if err != nil {
		return Detached{}, err
	}
	defer func() { _ = devNull.Close() }()
	cmd := exec.Command(exe, args...)
	cmd.Env = env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = devNull, devNull, devNull
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return Detached{}, err
	}
	p := cmd.Process
	return Detached{
		PID:     p.Pid,
		Release: func() { _ = p.Release() },
		Kill: func() {
			_ = p.Kill()
			_, _ = p.Wait()
		},
	}, nil
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
