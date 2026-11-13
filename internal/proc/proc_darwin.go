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
		Name:    processName(pid, string(name)),
		StartMS: ms,
	}, true
}

// processName keeps p_comm unless the first argument names a client. Claude Code's native
// binary lives at a path ending in its version, so p_comm is "2.1.319" and only the title it
// writes over argv[0] says "claude". Everything else keeps p_comm, because a login shell's
// argv[0] is "-zsh". The kernel refuses another user's arguments, and those keep p_comm too.
func processName(pid int, comm string) string {
	raw, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		return comm
	}
	if arg := firstArgument(raw); isClient(arg) {
		return baseName(arg)
	}
	return comm
}

func executableOS(pid int, _ Snapshot) (string, error) {
	raw, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		return "", err
	}
	return executablePath(raw), nil
}

func executablePath(raw []byte) string {
	exe, ok, _ := cutProcargs(raw)
	if !ok {
		return ""
	}
	return exe
}

// firstArgument reads argv[0] from a kern.procargs2 buffer: a 32-bit argument count, the
// executable path, NUL padding, then the arguments.
func firstArgument(raw []byte) string {
	_, _, rest := cutProcargs(raw)
	arg, _, _ := bytes.Cut(bytes.TrimLeft(rest, "\x00"), []byte{0})
	return string(arg)
}

func cutProcargs(raw []byte) (exe string, complete bool, rest []byte) {
	if len(raw) < 4 {
		return "", false, nil
	}
	pathBytes, after, found := bytes.Cut(raw[4:], []byte{0})
	if !found {
		return "", false, nil
	}
	return string(pathBytes), true, after
}
