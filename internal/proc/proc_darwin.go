//go:build darwin

package proc

import (
	"bytes"
	"encoding/binary"

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
	comm := string(name)
	visible, exe, launcher := comm, "", ""
	// kern.procargs2 is the executable path plus argv. There is no separate exe link.
	// A refused buffer leaves p_comm, so a node whose arguments cannot be read is not guessed.
	if raw, err := unix.SysctlRaw("kern.procargs2", pid); err == nil {
		visible, exe, launcher = identityFromProcargs(comm, raw)
	}
	return Snapshot{
		PID:      pid,
		PPID:     int(k.Eproc.Ppid),
		Name:     visible,
		StartMS:  ms,
		Exe:      exe,
		Launcher: launcher,
	}, true
}

// identityFromProcargs keeps p_comm unless the first argument names a client or the executable
// or a launcher argument resolves the agent alias. Claude Code's native binary lives at a path
// ending in its version, so p_comm is "2.1.319" and only the title it writes over argv[0] says
// "claude". Cursor's launcher is the opposite: p_comm is "node" and argv[0] is "agent" (or its
// full path), while index.js may follow node flags such as --use-system-ca. A login shell's
// argv[0] is "-zsh", so anything that is not a client or that alias keeps p_comm.
func identityFromProcargs(comm string, raw []byte) (name, exe, launcher string) {
	exe = executablePath(raw)
	argv0 := firstArgument(raw)
	if args, err := procargsArgs(raw); err == nil {
		launcher = launcherArg(args)
	}
	return promotedProcessName(comm, argv0, exe, launcher), exe, launcher
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

// noDevice is NODEV, the e_tdev of a process without a controlling terminal.
const noDevice = -1

// kinfoProc reads a process's kinfo_proc. Tests replace it.
var kinfoProc = func(pid int) (*unix.KinfoProc, error) { return unix.SysctlKinfoProc("kern.proc.pid", pid) }

// readsTerminalOS reports whether the process has a controlling terminal. macOS gives no cheap view
// of another process's standard input, and Grok 1.0.77 starts its hooks without its terminal, so
// the hook's own terminal says nothing (ADR 031).
func readsTerminalOS(pid int) (bool, error) {
	k, err := kinfoProc(pid)
	if err != nil {
		return false, err
	}
	if k == nil {
		return false, ErrNotFound
	}
	return k.Eproc.Tdev != noDevice, nil
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
