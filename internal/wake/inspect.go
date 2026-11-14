package wake

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/mateusz-klatt/iris/internal/hook"
)

// Inspection is local diagnostic metadata. It carries no prompt, correlation token, message
// content, stream identity, unrecognized diagnostic detail or raw document bytes.
type Inspection struct {
	Path, Status                             string
	Attempts                                 int
	Kind, Outcome                            string
	Outstanding                              bool
	StartedMS, AnsweredMS, ConsumedMS        int64
	Slot0, Slot1                             int
	BinaryPath, BinaryVersion, ClientVersion string
	CheckedMS, FailureMS                     int64
	// FailureReason contains only a recognized fixed command failure or a fixed fallback.
	FailureReason string
}

// ErrInspection is a fixed diagnostic failure; filesystem error text is never reported.
var ErrInspection = errors.New("wake guard inspection is unavailable")

type inspectionIO struct {
	readDir  func(string) ([]os.DirEntry, error)
	readFile func(string) ([]byte, error)
	lock     func(time.Duration) (func(), error)
}

// Inspect reads retained guards without protecting directories or rewriting guards. Missing
// directories create nothing. An existing guard is read and closed under the shared lock;
// each file releases it before another is considered, preserving the hooks' short budget.
// The stable .lock file may be created or normalized by the shared lock implementation.
func Inspect(tokenPath string) ([]Inspection, error) {
	if tokenPath == "" {
		return nil, ErrInspection
	}
	sessions := hook.NewSessions(tokenPath)
	return inspect(sessions, inspectionIO{readDir: os.ReadDir, readFile: os.ReadFile, lock: sessions.Lock})
}

func inspect(sessions *hook.Sessions, ops inspectionIO) ([]Inspection, error) {
	dir := filepath.Dir(sessions.WakePath("codex", ""))
	entries, err := ops.readDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, ErrInspection
	}
	var result []Inspection
	for _, entry := range entries {
		name := entry.Name()
		decoded, err := hex.DecodeString(name)
		if err != nil || len(decoded) != 16 || hex.EncodeToString(decoded) != name {
			continue
		}
		unlock, err := ops.lock(time.Second)
		if err != nil {
			if errors.Is(err, hook.ErrSessionsLocked) {
				return result, hook.ErrSessionsLocked
			}
			return result, ErrInspection
		}
		path := filepath.Join(dir, name)
		body, err := ops.readFile(path)
		unlock()
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			result = append(result, Inspection{Path: path, Status: "unreadable"})
			continue
		}
		result = append(result, inspectDocument(path, body))
	}
	return result, nil
}

func inspectDocument(path string, body []byte) Inspection {
	result := Inspection{Path: path, Status: "unusable"}
	var d document
	var required struct {
		Version  *int               `json:"version"`
		Rev      *int64             `json:"rev"`
		Attempts *[]json.RawMessage `json:"attempts"`
	}
	if json.Unmarshal(body, &d) != nil || json.Unmarshal(body, &required) != nil || required.Version == nil || required.Rev == nil || required.Attempts == nil || !d.valid() {
		return result
	}
	result.Status, result.Attempts = "usable", len(d.Attempts)
	result.Slot0, result.Slot1 = d.quota(0), d.quota(1)
	result.BinaryPath, result.BinaryVersion, result.ClientVersion, result.CheckedMS = d.Binary.Path, d.Binary.Version, d.Binary.ClientVersion, d.Binary.CheckedMS
	if d.LastFailure != nil {
		result.FailureMS = d.LastFailure.AtMS
		result.FailureReason = inspectionFailureReason(d.LastFailure.Detail)
	}
	if len(d.Attempts) == 0 {
		return result
	}
	a := d.Attempts[len(d.Attempts)-1]
	result.Kind, result.Outstanding = a.Kind, a.outstanding()
	result.StartedMS, result.AnsweredMS, result.ConsumedMS = a.StartedMS, a.AnsweredMS, a.ConsumedMS
	switch a.Answer {
	case "submitted", "stored", "failed", "aborted":
		result.Outcome = a.Answer
	default:
		result.Outcome = "unknown"
	}
	return result
}

func inspectionFailureReason(detail string) string {
	switch detail {
	case "Codex executable is not available on PATH",
		"Codex initialization did not name its version",
		"Codex version command did not complete successfully",
		"Codex version response exceeded its bound",
		"Codex version response named no version",
		"Codex executable and initialized client versions differ",
		"Codex status proxy could not start",
		"Codex status proxy did not establish its connection",
		"Codex thread status could not be verified",
		"Codex status proxy exceeded its bound or failed",
		"Codex queue command could not start",
		"Codex has no saved rollout for this thread",
		// The retired ADR 026 web wake wrote these three; wake files from before ADR 030 can
		// still carry them.
		"Kimi executable version could not be determined",
		"Kimi executable and server versions differ",
		"Kimi session is open in another Kimi process",
		antigravityExeUnavailable,
		antigravitySendStartFailed,
		antigravityRejectedWake:
		return detail
	default:
		return "Unrecognized wake failure"
	}
}
