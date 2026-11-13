//go:build windows

package proc

import (
	"fmt"
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// openProcess is windows.OpenProcess. Tests replace it to refuse access, which a same-user
// process is not otherwise refused.
var openProcess = windows.OpenProcess

// detachUnderOS opens the parent only for the spawn: PROCESS_CREATE_PROCESS to name it as the
// parent, and PROCESS_DUP_HANDLE so the standard handles can be placed in it first. The
// identity is checked on that same handle, so a reused pid cannot become the parent.
func detachUnderOS(cmd *exec.Cmd, pid int, startMS int64) (func(), error) {
	h, err := openProcess(windows.PROCESS_CREATE_PROCESS|windows.PROCESS_DUP_HANDLE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return nil, fmt.Errorf("open the parent process: %w", err)
	}
	var code uint32
	var created, exit, kernel, user windows.Filetime
	if windows.GetExitCodeProcess(h, &code) != nil || code != stillActive ||
		windows.GetProcessTimes(h, &created, &exit, &kernel, &user) != nil || created.Nanoseconds()/1e6 != startMS {
		_ = windows.CloseHandle(h)
		return nil, ErrNotFound
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.CREATE_NEW_PROCESS_GROUP | windows.DETACHED_PROCESS,
		ParentProcess: syscall.Handle(h),
	}
	return func() { _ = windows.CloseHandle(h) }, nil
}

func noConsoleOS(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.DETACHED_PROCESS}
}

func terminateOS(pid int, startMS int64) error {
	h, err := windows.OpenProcess(windows.PROCESS_TERMINATE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return ErrNotFound
	}
	defer func() { _ = windows.CloseHandle(h) }()
	var created, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &created, &exit, &kernel, &user); err != nil || created.Nanoseconds()/1e6 != startMS {
		return ErrNotFound
	}
	return windows.TerminateProcess(h, 1)
}
