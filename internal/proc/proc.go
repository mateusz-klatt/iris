// Package proc identifies the process that launched a hook.
// The walk records the nearest non-shell ancestor and stops at the first
// client process: continuing past that client bound the wrong session.
package proc

import (
	"errors"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
)

// ErrNotFound means the walk ran out of ancestors before it found one.
var ErrNotFound = errors.New("no ancestor process")

// Ancestor is the process a hook records at session start.
type Ancestor struct {
	PID     int
	PPID    int
	Name    string
	StartMS int64 // Unix epoch milliseconds, 0 when the OS does not say
}

// Snapshot is one process as seen by the walk.
type Snapshot struct {
	PID     int
	PPID    int
	Name    string
	StartMS int64
}

// Lookup returns one live process using the same platform evidence as the ancestor walk.
func Lookup(pid int) (Snapshot, bool) { return lookupOS(pid) }

// Args returns the arguments of the live process (pid, startMS), argv[0] first. The identity is
// checked before and after the read, so a reused pid never lends its arguments. A process can
// rewrite its own arguments; Iris reads them only to tell a client's interactive session from a
// print-mode run (ADR 031).
func Args(pid int, startMS int64) ([]string, error) {
	return identified(pid, startMS, lookupOS, argsOS)
}

// ReadsTerminal reports whether the live process (pid, startMS) takes its input from a terminal.
// On Linux it reads where the process's standard input points. macOS offers no cheap view of
// another process's input, so there it reports whether the process has a controlling terminal.
// Windows reports whether the caller itself has a console, as a hook has when the client that
// started it runs on one (ADR 031). Both count a client whose input was redirected at a terminal
// as reading from it.
func ReadsTerminal(pid int, startMS int64) (bool, error) {
	return identified(pid, startMS, lookupOS, readsTerminalOS)
}

// identified reads one fact of the live process (pid, startMS). It checks the identity before and
// after the read, so a pid reused in between does not answer for the process.
func identified[T any](pid int, startMS int64, get func(int) (Snapshot, bool), read func(int) (T, error)) (T, error) {
	var zero T
	same := func() bool {
		s, ok := get(pid)
		return ok && s.StartMS == startMS
	}
	if pid <= 0 || startMS <= 0 || !same() {
		return zero, ErrNotFound
	}
	v, err := read(pid)
	if err != nil {
		return zero, err
	}
	if !same() {
		return zero, ErrNotFound
	}
	return v, nil
}

// Executable resolves the verified regular-file executable of a live process matching (pid, startMS).
func Executable(pid int, startMS int64) (string, error) {
	return executableFrom(pid, startMS, lookupOS, executableOS, os.Stat)
}

func executableFrom(
	pid int,
	startMS int64,
	get func(int) (Snapshot, bool),
	readExe func(int, Snapshot) (string, error),
	stat func(string) (os.FileInfo, error),
) (string, error) {
	if pid <= 0 || startMS <= 0 {
		return "", ErrNotFound
	}
	snap, ok := get(pid)
	if !ok || snap.StartMS != startMS {
		return "", ErrNotFound
	}
	exe, err := readExe(pid, snap)
	if err != nil || exe == "" || strings.HasSuffix(exe, " (deleted)") || !filepath.IsAbs(exe) {
		return "", ErrNotFound
	}
	info, err := stat(exe)
	if err != nil || !info.Mode().IsRegular() {
		return "", ErrNotFound
	}
	after, ok := get(pid)
	if !ok || after.StartMS != startMS {
		return "", ErrNotFound
	}
	return exe, nil
}

// DetachUnder configures cmd to run detached from the caller, as a child of the live process
// (pid, startMS). It refuses when that process no longer matches. Call release after cmd.Start.
//
// On Windows the child is created under that process instead of the caller, so it takes that
// process's job and token rather than the caller's. A command that agy runs is in a job that ends
// with the command and forbids breakaway, so a helper it starts would otherwise die with it
// (ADR 028). The child still takes the caller's desktop. Unix keeps the caller as the parent: a
// new session already separates the child, and (pid, startMS) is only checked.
func DetachUnder(cmd *exec.Cmd, pid int, startMS int64) (release func(), err error) {
	return detachUnderOS(cmd, pid, startMS)
}

// NoConsole starts cmd without a console. On Windows a console program started from a process
// on another desktop, such as one in agy's command sandbox, can fail to initialize while
// attaching a new console, and dies with 0xC0000142 before it runs. Without a console it runs,
// and the pipes set on cmd still carry its output. Unix has no consoles and leaves cmd as it is.
func NoConsole(cmd *exec.Cmd) { noConsoleOS(cmd) }

// Terminate stops a live process only when its platform identity still matches (pid, startMS).
func Terminate(pid int, startMS int64) error {
	return terminateFrom(pid, startMS, lookupOS, terminateOS)
}

func terminateFrom(
	pid int,
	startMS int64,
	get func(int) (Snapshot, bool),
	kill func(int, int64) error,
) error {
	if pid <= 0 || startMS <= 0 {
		return ErrNotFound
	}
	snap, ok := get(pid)
	if !ok || snap.StartMS != startMS {
		return ErrNotFound
	}
	return kill(pid, startMS)
}

// Nearest returns the ancestor of the current process that Iris records.
func Nearest() (Ancestor, error) { return nearest(os.Getpid(), lookupOS) }
func nearest(self int, get func(int) (Snapshot, bool)) (Ancestor, error) {
	s, ok := NearestFrom(self, get)
	if !ok {
		return Ancestor{}, ErrNotFound
	}
	return Ancestor(s), nil
}

// NearestFrom walks parents of self using get. Shells are skipped. The first
// client process wins, so a parent client is never chosen over the session
// that actually spawned the hook. When the chain has no client, the nearest
// non-shell ancestor is returned.
func NearestFrom(self int, get func(int) (Snapshot, bool)) (Snapshot, bool) {
	cur, ok := get(self)
	if !ok {
		return Snapshot{}, false
	}
	pid := cur.PPID
	var fallback *Snapshot
	for range 64 {
		if pid <= 1 {
			break
		}
		p, ok := get(pid)
		if !ok {
			break
		}
		if !isShell(p.Name) {
			if fallback == nil {
				cp := p
				fallback = &cp
			}
			if isClient(p.Name) {
				return p, true
			}
		}
		if p.PPID == pid || p.PPID <= 0 {
			break
		}
		pid = p.PPID
	}
	if fallback != nil {
		return *fallback, true
	}
	return Snapshot{}, false
}

func isShell(name string) bool {
	switch baseName(name) {
	case "sh", "bash", "dash", "ash", "zsh", "fish", "ksh", "csh", "tcsh", "cmd", "powershell", "pwsh", "busybox":
		return true
	default:
		return false
	}
}

func isClient(name string) bool {
	switch baseName(name) {
	case "claude", "codex", "copilot", "cursor", "cursor-agent", "agy", "antigravity", "grok", "kimi", "kimi-code":
		return true
	default:
		return false
	}
}

func baseName(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	name = strings.ToLower(path.Base(name))
	name = strings.TrimSuffix(name, ".exe")
	return name
}

// SingleSession recognizes only measured single-session launchers. In particular,
// the Codex app-server and Cursor IDE are deliberately excluded.
func SingleSession(a Ancestor, client string) bool {
	return singleSession(a, client, os.ReadFile)
}

// singleSession reads the command line from /proc, so a Codex process is never
// recognized on Windows or macOS and its session binds by another rung.
func singleSession(a Ancestor, client string, read func(string) ([]byte, error)) bool {
	switch client {
	case "cursor":
		return baseName(a.Name) == "cursor-agent"
	case "antigravity":
		return baseName(a.Name) == "antigravity" || baseName(a.Name) == "agy"
	case "kimi":
		return baseName(a.Name) == "kimi" || baseName(a.Name) == "kimi-code"
	case "codex":
		if baseName(a.Name) != "codex" {
			return false
		}
		raw, err := read("/proc/" + strconv.Itoa(a.PID) + "/cmdline")
		if err != nil {
			return false
		}
		args := strings.Split(string(raw), "\x00")
		// Only an unambiguous command position is positive evidence. A value of
		// --config (or a prompt) containing "exec" must not classify app-server.
		return len(args) > 1 && args[1] == "exec"
	}
	return false
}
