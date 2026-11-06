// Package proc identifies the process that launched a hook.
// The walk records the nearest non-shell ancestor and stops at the first
// client process: continuing past that client bound the wrong session.
package proc

import (
	"errors"
	"os"
	"path"
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

// Nearest returns the ancestor of the current process that Iris records.
func Nearest() (Ancestor, error) {
	s, ok := NearestFrom(os.Getpid(), lookupOS)
	if !ok {
		return Ancestor{}, ErrNotFound
	}
	return Ancestor{PID: s.PID, PPID: s.PPID, Name: s.Name, StartMS: s.StartMS}, nil
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
		raw, err := os.ReadFile("/proc/" + strconv.Itoa(a.PID) + "/cmdline")
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
