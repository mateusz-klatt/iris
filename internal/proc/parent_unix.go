//go:build linux || darwin

package proc

import "os"

// ParentWatch captures the launching process before it can exit or its PID be reused.
type ParentWatch struct {
	pid   int
	start int64
}

func WatchParent() (*ParentWatch, error) { return watch(os.Getppid()) }
func watch(pid int) (*ParentWatch, error) {
	p, ok := lookupOS(pid)
	if pid <= 1 || !ok || p.StartMS <= 0 {
		return nil, ErrNotFound
	}
	return &ParentWatch{pid: pid, start: p.StartMS}, nil
}
func (p *ParentWatch) Alive() bool {
	if os.Getppid() != p.pid {
		return false
	}
	current, ok := lookupOS(p.pid)
	return ok && current.StartMS == p.start
}
func (*ParentWatch) Close() {}
