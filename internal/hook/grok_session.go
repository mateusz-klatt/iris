package hook

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/mateusz-klatt/iris/internal/config"
	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/proc"
)

// grokPIDDirName is the directory of pid files, beside the sessions directory and not inside it.
// Pruning walks sessions and would otherwise delete a file for a session that is still alive.
const grokPIDDirName = "grok-pids"

// protectPath applies the token's owner-only protection. Tests replace it. On Unix it is a no-op
// and the mode bits are what keep the file private.
var protectPath = config.OwnerOnly

// validGrokSession accepts the ids Grok puts in a hook payload: a UUID, and nothing a file
// name or a second line could be made from. The file name itself is only the pid and start time.
func validGrokSession(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.' || r == '_' || r == ':' || r == '-':
		default:
			return false
		}
	}
	return true
}

func isGrokProcess(name string) bool {
	name = strings.ReplaceAll(name, "\\", "/")
	name = strings.ToLower(path.Base(name))
	return strings.TrimSuffix(name, ".exe") == "grok"
}

func grokPIDDirectory(getenv func(string) string) (string, error) {
	if getenv == nil {
		return "", nil
	}
	token, err := (hubclient.EnvConfig{Getenv: getenv}).TokenPath()
	if err != nil || token == "" {
		return "", err
	}
	return filepath.Join(filepath.Dir(token), grokPIDDirName), nil
}

func grokPIDFile(dir string, pid int, startMS int64) string {
	return filepath.Join(dir, fmt.Sprintf("%d-%d", pid, startMS))
}

// writeGrokSession replaces one pid file. A reader sees the previous id or the new one.
func writeGrokSession(dir string, pid int, startMS int64, sessionID string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := protectPath(dir); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = f.WriteString(sessionID + "\n")
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = protectPath(tmp)
	}
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, grokPIDFile(dir, pid, startMS)); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// ReadGrokSession returns the session id recorded for this exact process. A missing file, a
// pid whose start time differs, or a line that is not a session id is no id.
func ReadGrokSession(dir string, pid int, startMS int64) (string, bool) {
	if dir == "" || pid <= 0 || startMS <= 0 {
		return "", false
	}
	raw, err := os.ReadFile(grokPIDFile(dir, pid, startMS))
	if err != nil {
		return "", false
	}
	id := strings.TrimSpace(string(raw))
	if !validGrokSession(id) {
		return "", false
	}
	return id, true
}

// forgetGrokSession removes the file only when it still names sessionID. A subagent ends
// with its own id and must leave the parent's file in place.
func forgetGrokSession(dir string, pid int, startMS int64, sessionID string) error {
	path := grokPIDFile(dir, pid, startMS)
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if strings.TrimSpace(string(raw)) != sessionID {
		return nil
	}
	return os.Remove(path)
}

// recordGrokSession writes the session id of a top-level Grok SessionStart. It runs only after
// the hub has accepted the session. Anything else — a subagent, a process that is not grok, a
// start time the OS did not give — writes nothing. A write that fails is ignored: the hook
// still has to allow the session (D3), and this file is not the sessions directory whose
// failure is reported as a skipped run.
func (r Runner) recordGrokSession(ev Event) {
	if ev.Client != ClientGrok || ev.Kind != KindSessionStart || ev.ParentSessionID != "" || !validGrokSession(ev.SessionID) {
		return
	}
	anc := r.ancestor()
	if anc.PID <= 0 || anc.StartMS <= 0 || !isGrokProcess(anc.Name) {
		return
	}
	dir, err := grokPIDDirectory(r.Getenv)
	if err != nil || dir == "" {
		return
	}
	_ = writeGrokSession(dir, anc.PID, anc.StartMS, ev.SessionID)
}

// forgetGrokSessionFile drops the pid file after the hub has ended this execution, and only
// when the file still names this session. A failed end keeps the file, so a later watch can
// still find the execution the hub did not end.
func (r Runner) forgetGrokSessionFile(ev Event) {
	if ev.Client != ClientGrok || ev.Kind != KindSessionEnd || !validGrokSession(ev.SessionID) {
		return
	}
	anc := r.ancestor()
	if anc.PID <= 0 || anc.StartMS <= 0 {
		return
	}
	dir, err := grokPIDDirectory(r.Getenv)
	if err != nil || dir == "" {
		return
	}
	_ = forgetGrokSession(dir, anc.PID, anc.StartMS, ev.SessionID)
}

// GrokSessionEnv fills an empty GROK_SESSION_ID from the pid file of the nearest grok process.
// The monitor shell does not inherit that variable (measured on Grok 1.0.77); the hook process
// does. A value already in the environment wins. A process that is not grok, or whose start
// time is unknown, leaves the variable empty so binding does not guess.
func GrokSessionEnv(getenv func(string) string, nearest func() (proc.Ancestor, error)) func(string) string {
	if getenv == nil {
		getenv = os.Getenv
	}
	return func(key string) string {
		if v := getenv(key); v != "" || key != "GROK_SESSION_ID" {
			return v
		}
		if nearest == nil {
			return ""
		}
		anc, err := nearest()
		if err != nil || anc.PID <= 0 || anc.StartMS <= 0 || !isGrokProcess(anc.Name) {
			return ""
		}
		dir, err := grokPIDDirectory(getenv)
		if err != nil || dir == "" {
			return ""
		}
		id, ok := ReadGrokSession(dir, anc.PID, anc.StartMS)
		if !ok {
			return ""
		}
		return id
	}
}
