//go:build windows

package proc

import (
	"golang.org/x/sys/windows"
	"os"
)

// ParentWatch retains the original process object even after its PID is reused.
// Windows keeps the inherited parent PID after exit, so comparing PIDs is insufficient.
type ParentWatch struct{ handle windows.Handle }

func WatchParent() (*ParentWatch, error) { return watch(os.Getppid()) }
func watch(pid int) (*ParentWatch, error) {
	if pid <= 0 {
		return nil, ErrNotFound
	}
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return nil, err
	}
	return &ParentWatch{handle: handle}, nil
}
func (p *ParentWatch) Alive() bool {
	state, err := windows.WaitForSingleObject(p.handle, 0)
	return err == nil && state == uint32(windows.WAIT_TIMEOUT)
}
func (p *ParentWatch) Close() { _ = windows.CloseHandle(p.handle) }
