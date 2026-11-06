//go:build darwin

package proc

import (
	"bytes"

	"golang.org/x/sys/unix"
)

func lookupOS(pid int) (Snapshot, bool) {
	if pid <= 0 {
		return Snapshot{}, false
	}
	k, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || k == nil {
		return Snapshot{}, false
	}
	name := k.Proc.P_comm[:]
	if i := bytes.IndexByte(name, 0); i >= 0 {
		name = name[:i]
	}
	ms := k.Proc.P_starttime.Sec*1000 + int64(k.Proc.P_starttime.Usec)/1000
	return Snapshot{
		PID:     pid,
		PPID:    int(k.Eproc.Ppid),
		Name:    string(name),
		StartMS: ms,
	}, true
}
