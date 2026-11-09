//go:build !windows

package gitinfo

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// signalGroup kills the process group pgid. Tests replace it to see what would be signalled.
var signalGroup = func(pgid int) error { return syscall.Kill(-pgid, syscall.SIGKILL) }

// stopDescendants runs git in a process group of its own and kills that group when the context
// ends. os/exec calls Cancel only for a command it has started, so Process is set. It can call
// it just after git has exited: what git left in its group is stopped all the same, and an
// empty group means the command is done.
//
// It cannot reach a process that has left the group, and the kernel finishes uninterruptible
// work first. The kill is sent as the deadline passes; nothing waits to see it land.
func stopDescendants(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return stopGroup(cmd.Process.Pid) }
}

// stopGroup kills the group that the git with this pid leads. kill(2) reads 0 as the caller's
// own group and -1 as every process the caller may signal, so a pid that cannot be a started
// git's is refused instead of negated.
//
// The number stays that group's while git or anything in the group lives, so the signal
// reaches that group or, once it is empty, nothing. Only a pid recycled between git's reaping
// and this call, which os/exec makes at most once, could make it another group's.
func stopGroup(pid int) error {
	if pid <= 1 {
		return errors.New("gitinfo: no process group to stop")
	}
	err := signalGroup(pid)
	if errors.Is(err, syscall.ESRCH) {
		return os.ErrProcessDone
	}
	return err
}
