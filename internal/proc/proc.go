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
