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

// ErrExecutableDeleted means a live process's executable was unlinked after start and could
// not be opened from its running image.
var ErrExecutableDeleted = errors.New("process executable was deleted")

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
// Linux and Windows read the process's own standard input; on Windows, its StandardInput is read
// through its PEB and duplicated. macOS offers no cheap view of another process's input, so there
// it reports whether the process itself has a controlling terminal. On macOS that counts a client
// whose input was redirected at a terminal as reading from it (ADR 031).
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

// ProcessExecutable is a verified executable of a live process, backed either by its regular
// file path or, on Linux after an in-place update unlinked the running binary, by an open
// descriptor to that running image. Always call Close when done.
type ProcessExecutable struct {
	Path    string
	Deleted bool
	exeHandle
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
	img, err := openExecutableFrom(pid, startMS, get, readExe, stat, func(int, int64, string, func(int) (Snapshot, bool)) (*ProcessExecutable, error) {
		return nil, ErrNotFound
	})
	if err != nil {
		return "", err
	}
	return img.Path, nil
}

// OpenExecutable resolves the verified regular-file executable of a live process matching
// (pid, startMS). On Linux, when /proc/<pid>/exe ends in " (deleted)", it opens /proc/<pid>/exe,
// verifies a regular file via fstat, rechecks (pid, startMS), and returns a descriptor-backed
// handle whose Command runs that descriptor via ExtraFiles and /proc/self/fd/3 rather than a
// path on disk. On other platforms a deleted executable path returns ErrExecutableDeleted.
func OpenExecutable(pid int, startMS int64) (*ProcessExecutable, error) {
	return openExecutableFrom(pid, startMS, lookupOS, executableOS, os.Stat, openDeletedExeOS)
}

func openExecutableFrom(
	pid int,
	startMS int64,
	get func(int) (Snapshot, bool),
	readExe func(int, Snapshot) (string, error),
	stat func(string) (os.FileInfo, error),
	openDeleted func(int, int64, string, func(int) (Snapshot, bool)) (*ProcessExecutable, error),
) (*ProcessExecutable, error) {
	if pid <= 0 || startMS <= 0 {
		return nil, ErrNotFound
	}
	snap, ok := get(pid)
	if !ok || snap.StartMS != startMS {
		return nil, ErrNotFound
	}
	exe, err := readExe(pid, snap)
	if err != nil || exe == "" || !filepath.IsAbs(exe) {
		return nil, ErrNotFound
	}
	if strings.HasSuffix(exe, " (deleted)") {
		return openDeleted(pid, startMS, exe, get)
	}
	info, err := stat(exe)
	if err != nil || !info.Mode().IsRegular() {
		return nil, ErrNotFound
	}
	after, ok := get(pid)
	if !ok || after.StartMS != startMS {
		return nil, ErrNotFound
	}
	return &ProcessExecutable{Path: exe}, nil
}

// Detached is a process that StartDetached created. Release forgets it and leaves it running;
// Kill stops it and waits for it. Call either Release or Kill, once.
type Detached struct {
	PID     int
	Release func()
	Kill    func()
}

// StartDetached starts exe with args and env detached from the caller, with its standard streams
// discarded, as a child of the live process (pid, startMS). It refuses, before anything starts,
// when that process no longer matches.
//
// On Windows the process is created under (pid, startMS) instead of the caller, so it takes that
// process's job and token rather than the caller's. A command that agy runs sits in a job that
// ends with the command and forbids breakaway, and on a desktop of its own that ends with it too
// (ADR 028). So when the caller's desktop is not its window station's Default desktop, where agy
// runs, the process is placed there; a desktop that cannot be read that way refuses the start. On
// Unix the caller stays the parent: a new session already separates the process, and (pid,
// startMS) is only checked.
func StartDetached(exe string, args, env []string, pid int, startMS int64) (Detached, error) {
	return startDetachedOS(exe, args, env, pid, startMS)
}

// NoConsole starts cmd without a console. On Windows a console program started by a process
// that has none, such as the detached Antigravity helper, would otherwise get a console window of
// its own on the user's desktop. Without one it runs unseen, and the pipes set on cmd still carry
// its output. On Unix it leaves cmd unchanged.
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
func Nearest() (Ancestor, error) { return nearest(os.Getpid(), lookupOS, os.Getenv) }
func nearest(self int, get func(int) (Snapshot, bool), getenv func(string) string) (Ancestor, error) {
	s, ok := NearestFrom(self, get, getenv)
	if !ok {
		return Ancestor{}, ErrNotFound
	}
	return Ancestor(s), nil
}

// NearestFrom walks parents of self using get and getenv. Shells are skipped. The first
// recognized client process or unidentified agent boundary wins, so a parent client is never
// chosen over the session that actually spawned the hook. When the chain has no client, the nearest
// non-shell ancestor is returned.
func NearestFrom(self int, get func(int) (Snapshot, bool), getenv func(string) string) (Snapshot, bool) {
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
			if isClient(p.Name, getenv) || AgentAlias(p.Name) {
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

func isClient(name string, getenv func(string) string) bool {
	_, ok := ClientNameWithEnv(name, getenv)
	return ok
}

// AgentAlias reports Cursor's generic alias, which unrelated tools can also use.
func AgentAlias(name string) bool { return baseName(name) == "agent" }

// CursorEnvironment reports the marker pair Cursor supplies to a cross-loaded Claude hook.
func CursorEnvironment(getenv func(string) string) bool {
	return getenv != nil && getenv("CURSOR_PROJECT_DIR") != "" && getenv("CURSOR_VERSION") != ""
}

// ClientNameWithEnv also recognizes Cursor's generic agent alias, but only with its marker pair.
func ClientNameWithEnv(name string, getenv func(string) string) (string, bool) {
	if AgentAlias(name) && CursorEnvironment(getenv) {
		return "cursor", true
	}
	return ClientName(name)
}

// ClientName maps a client process's executable name to Iris's client name.
func ClientName(name string) (string, bool) {
	switch baseName(name) {
	case "claude":
		return "claude", true
	case "codex":
		return "codex", true
	case "copilot":
		return "copilot", true
	case "cursor", "cursor-agent":
		return "cursor", true
	case "agy", "antigravity":
		return "antigravity", true
	case "grok":
		return "grok", true
	case "kimi", "kimi-code":
		return "kimi", true
	default:
		return "", false
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
