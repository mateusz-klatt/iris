//go:build darwin

package proc

import (
	"bytes"
	"encoding/binary"
	"io"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// zombie is SZOMB, the p_stat of a process that has exited but not been collected (sys/proc.h).
const zombie = 5

func lookupOS(pid int) (Snapshot, bool) {
	if pid <= 0 {
		return Snapshot{}, false
	}
	k, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	// A zombie has exited and only waits for its parent to collect it, so it is not running, as
	// on Linux.
	if err != nil || k == nil || k.Proc.P_stat == zombie {
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

func argsOS(pid int) ([]string, error) {
	raw, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		return nil, err
	}
	return procargsArgs(raw)
}

// procargsArgs reads the arguments from a kern.procargs2 buffer: the argument count, the
// executable path and its NUL padding, then that many NUL-terminated arguments before the
// environment. A buffer cut short gives none.
func procargsArgs(raw []byte) ([]string, error) {
	_, complete, rest := cutProcargs(raw)
	n := 0
	if complete {
		n = int(binary.LittleEndian.Uint32(raw[:4]))
	}
	if n <= 0 || n > len(rest) {
		return nil, ErrNotFound
	}
	rest = bytes.TrimLeft(rest, "\x00")
	args := make([]string, 0, n)
	for len(args) < n {
		arg, after, found := bytes.Cut(rest, []byte{0})
		if !found {
			return nil, ErrNotFound
		}
		args, rest = append(args, string(arg)), after
	}
	return args, nil
}

// openTerminal opens the caller's controlling terminal. Tests replace it.
var openTerminal = func() (io.Closer, error) {
	return os.OpenFile("/dev/tty", os.O_RDONLY|syscall.O_NOCTTY, 0)
}

// readsTerminalOS reports whether the caller has a controlling terminal. macOS gives no cheap view
// of another process's standard input, and a hook shares its client's terminal (ADR 031).
func readsTerminalOS(int) (bool, error) {
	f, err := openTerminal()
	if err != nil {
		return false, nil
	}
	_ = f.Close()
	return true, nil
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
