package hook

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/proc"
)

// AntigravityWakeFileName is the opt-in wake marker beside hub.token.
const AntigravityWakeFileName = "antigravity-wake"

const (
	antigravityHelperDirName    = "antigravity-helpers"
	antigravityHelperLockBudget = time.Second
	maxSupersededSessions       = 16
	antigravityScanPassBudget   = 4 << 20
	antigravityMaxLineBytes     = 64 << 10
)

var (
	createAntigravityTemp      = os.CreateTemp
	syncAntigravityTemp        = func(f *os.File) error { return f.Sync() }
	lookupAntigravityProc      = proc.Lookup
	terminateAntigravityProc   = proc.Terminate
	irisExecutablePath         = os.Executable
	pinnedAntigravityConfigDir = hubclient.PinnedConfigDir
	readTranscriptChunk        = func(f *os.File, off, limit int64) ([]byte, error) {
		return io.ReadAll(io.NewSectionReader(f, off, limit))
	}
	antigravityScanLimit int64 = antigravityScanPassBudget
	antigravityLineLimit       = antigravityMaxLineBytes
)

// AntigravityWakePath returns the path to the Antigravity wake marker inside configDir.
func AntigravityWakePath(configDir string) string {
	return filepath.Join(configDir, AntigravityWakeFileName)
}

// AntigravityWakeEnabled reports whether Antigravity wake is explicitly enabled ("on").
// Missing, unreadable or "off" markers report false.
func AntigravityWakeEnabled(configDir string) bool {
	if configDir == "" {
		return false
	}
	raw, err := os.ReadFile(AntigravityWakePath(configDir))
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(raw)) == "on"
}

// AntigravityHelperOwner identifies one detached wake helper instance for a host agy process.
type AntigravityHelperOwner struct {
	AgyPID        int
	AgyStartMS    int64
	HelperPID     int
	HelperStartMS int64
	SessionID     string
	AttemptID     string
}

// AntigravityConvState tracks idle state and transcript metadata for one Antigravity conversation.
type AntigravityConvState struct {
	Idle           bool   `json:"idle"`
	TranscriptPath string `json:"transcript_path,omitempty"`
	FileID         string `json:"file_id,omitempty"`
	IdleOffset     int64  `json:"idle_offset,omitempty"`
}

// AntigravityScanCursor is the incremental forward transcript cursor for an outstanding wake attempt.
type AntigravityScanCursor struct {
	TranscriptPath string `json:"transcript_path"`
	FileID         string `json:"file_id,omitempty"`
	ScannedOffset  int64  `json:"scanned_offset"`
	SkippingLine   bool   `json:"skipping_line,omitempty"`
}

type antigravityProcState struct {
	CurrentSessionID     string                   `json:"current_session_id,omitempty"`
	SupersededSessionIDs []string                 `json:"superseded_session_ids,omitempty"`
	Helper               *antigravityHelperRecord `json:"helper,omitempty"`
}

type antigravityHelperRecord struct {
	Status    string `json:"status"`
	PID       int    `json:"helper_pid,omitempty"`
	StartMS   int64  `json:"helper_start_ms,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	AttemptID string `json:"attempt_id,omitempty"`
}

func antigravityHelpersMemory(configDir string) fileMemory {
	return fileMemory{
		dir:      filepath.Join(configDir, antigravityHelperDirName),
		now:      time.Now,
		restrict: protectPath,
	}
}

func antigravityProcFile(dir string, pid int, startMS int64) string {
	return filepath.Join(dir, fmt.Sprintf("%d-%d.json", pid, startMS))
}

func antigravityConvFile(dir, sessionID string) string {
	sum := sha256.Sum256([]byte("antigravity\x00" + sessionID))
	return filepath.Join(dir, "conv-"+hex.EncodeToString(sum[:16])+".json")
}

func readAntigravityProcFile(path string) (antigravityProcState, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return antigravityProcState{}, false
	}
	var st antigravityProcState
	if json.Unmarshal(raw, &st) != nil {
		return antigravityProcState{}, false
	}
	return st, true
}

func readAntigravityProcState(dir string, pid int, startMS int64) antigravityProcState {
	st, _ := readAntigravityProcFile(antigravityProcFile(dir, pid, startMS))
	return st
}

func readAntigravityConvFile(dir, sessionID string) (AntigravityConvState, bool) {
	raw, err := os.ReadFile(antigravityConvFile(dir, sessionID))
	if err != nil {
		return AntigravityConvState{}, false
	}
	var st AntigravityConvState
	if json.Unmarshal(raw, &st) != nil {
		return AntigravityConvState{}, false
	}
	return st, true
}

// ReadAntigravityConvState reads the hook-recorded idle and transcript state for sessionID.
func ReadAntigravityConvState(configDir, sessionID string) (AntigravityConvState, bool) {
	if configDir == "" || !validGrokSession(sessionID) {
		return AntigravityConvState{}, false
	}
	return readAntigravityConvFile(filepath.Join(configDir, antigravityHelperDirName), sessionID)
}

func writeAntigravityJSON(dir, target string, v any) error {
	// State structs hold only strings, ints and bools, so Marshal cannot fail.
	raw, _ := json.Marshal(v)
	f, err := createAntigravityTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = f.Write(append(raw, '\n'))
	if err == nil {
		err = syncAntigravityTemp(f)
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = protectPath(tmp)
	}
	if err == nil {
		err = os.Rename(tmp, target)
	}
	if err != nil {
		_ = os.Remove(tmp)
	}
	return err
}

func helperAlive(h *antigravityHelperRecord, lookup func(int) (proc.Snapshot, bool)) bool {
	if h == nil || h.Status != "live" || h.PID <= 0 || h.StartMS <= 0 {
		return false
	}
	snap, ok := lookup(h.PID)
	return ok && snap.StartMS == h.StartMS
}

// ReadAntigravitySession returns the authoritative current_session_id for (agyPID, agyStartMS)
// and whether a live helper already owns that conversation.
func ReadAntigravitySession(configDir string, agyPID int, agyStartMS int64, lookup func(int) (proc.Snapshot, bool)) (string, bool) {
	if configDir == "" || agyPID <= 0 || agyStartMS <= 0 {
		return "", false
	}
	if lookup == nil {
		lookup = lookupAntigravityProc
	}
	dir := filepath.Join(configDir, antigravityHelperDirName)
	st := readAntigravityProcState(dir, agyPID, agyStartMS)
	if st.CurrentSessionID == "" {
		return "", false
	}
	live := st.Helper != nil && st.Helper.SessionID == st.CurrentSessionID && helperAlive(st.Helper, lookup)
	return st.CurrentSessionID, live
}

// PublishAntigravityHelper durably records a newly spawned detached helper under the helper lock
// without ever overwriting an already-live helper for the current conversation.
func PublishAntigravityHelper(configDir string, owner AntigravityHelperOwner, lookup func(int) (proc.Snapshot, bool)) (bool, error) {
	if configDir == "" || owner.AgyPID <= 0 || owner.AgyStartMS <= 0 || owner.HelperPID <= 0 || owner.HelperStartMS <= 0 || !validGrokSession(owner.SessionID) || owner.AttemptID == "" {
		return false, errors.New("invalid antigravity helper owner")
	}
	if lookup == nil {
		lookup = lookupAntigravityProc
	}
	m := antigravityHelpersMemory(configDir)
	if err := m.directory(); err != nil {
		return false, err
	}
	unlock, err := m.lock(antigravityHelperLockBudget)
	if err != nil {
		return false, err
	}
	var oldPID int
	var oldStart int64
	alreadyLive := false
	err = func() error {
		defer unlock()
		st := readAntigravityProcState(m.dir, owner.AgyPID, owner.AgyStartMS)
		if st.CurrentSessionID != owner.SessionID {
			return errors.New("antigravity session is not active on this process")
		}
		if st.Helper != nil && st.Helper.SessionID == owner.SessionID && helperAlive(st.Helper, lookup) {
			alreadyLive = true
			return nil
		}
		if st.Helper != nil && st.Helper.Status == "live" && st.Helper.SessionID != owner.SessionID && st.Helper.PID > 0 && st.Helper.StartMS > 0 {
			oldPID, oldStart = st.Helper.PID, st.Helper.StartMS
		}
		st.Helper = &antigravityHelperRecord{
			Status:    "live",
			PID:       owner.HelperPID,
			StartMS:   owner.HelperStartMS,
			SessionID: owner.SessionID,
			AttemptID: owner.AttemptID,
		}
		return writeAntigravityJSON(m.dir, antigravityProcFile(m.dir, owner.AgyPID, owner.AgyStartMS), st)
	}()
	if oldPID > 0 {
		_ = terminateAntigravityProc(oldPID, oldStart)
	}
	return alreadyLive, err
}

// FailAntigravityHelper conditionally records failed for (agyPID, agyStartMS) when sessionID is
// the active current_session_id and no live helper owns that current session.
func FailAntigravityHelper(configDir string, agyPID int, agyStartMS int64, sessionID string, lookup func(int) (proc.Snapshot, bool)) error {
	if configDir == "" || agyPID <= 0 || agyStartMS <= 0 || !validGrokSession(sessionID) {
		return errors.New("invalid antigravity helper failure target")
	}
	if lookup == nil {
		lookup = lookupAntigravityProc
	}
	m := antigravityHelpersMemory(configDir)
	if err := m.directory(); err != nil {
		return err
	}
	unlock, err := m.lock(antigravityHelperLockBudget)
	if err != nil {
		return err
	}
	defer unlock()
	st := readAntigravityProcState(m.dir, agyPID, agyStartMS)
	if st.CurrentSessionID != sessionID {
		return nil
	}
	if st.Helper != nil && st.Helper.SessionID == st.CurrentSessionID && helperAlive(st.Helper, lookup) {
		return nil
	}
	st.Helper = &antigravityHelperRecord{Status: "failed"}
	return writeAntigravityJSON(m.dir, antigravityProcFile(m.dir, agyPID, agyStartMS), st)
}

// VerifyAntigravityHelper reports whether wake remains enabled, the host agy process is alive,
// current_session_id still equals owner.SessionID, and the helper record still names owner.
func VerifyAntigravityHelper(configDir string, owner AntigravityHelperOwner, lookup func(int) (proc.Snapshot, bool)) bool {
	if !AntigravityWakeEnabled(configDir) || owner.AgyPID <= 0 || owner.AgyStartMS <= 0 {
		return false
	}
	if lookup == nil {
		lookup = lookupAntigravityProc
	}
	snap, ok := lookup(owner.AgyPID)
	if !ok || snap.StartMS != owner.AgyStartMS {
		return false
	}
	st := readAntigravityProcState(filepath.Join(configDir, antigravityHelperDirName), owner.AgyPID, owner.AgyStartMS)
	return st.CurrentSessionID == owner.SessionID &&
		st.Helper != nil &&
		st.Helper.Status == "live" &&
		st.Helper.PID == owner.HelperPID &&
		st.Helper.StartMS == owner.HelperStartMS &&
		st.Helper.SessionID == owner.SessionID &&
		st.Helper.AttemptID == owner.AttemptID
}

// CleanupAntigravityHelper removes the helper entry on (agyPID, agyStartMS) only if it still
// carries owner's exact (HelperPID, HelperStartMS, AttemptID).
func CleanupAntigravityHelper(configDir string, owner AntigravityHelperOwner) error {
	if configDir == "" || owner.AgyPID <= 0 || owner.AgyStartMS <= 0 {
		return nil
	}
	m := antigravityHelpersMemory(configDir)
	if _, err := os.Stat(m.dir); err != nil {
		return nil
	}
	unlock, err := m.lock(antigravityHelperLockBudget)
	if err != nil {
		return err
	}
	defer unlock()
	st := readAntigravityProcState(m.dir, owner.AgyPID, owner.AgyStartMS)
	if st.Helper == nil || st.Helper.Status != "live" ||
		st.Helper.PID != owner.HelperPID || st.Helper.StartMS != owner.HelperStartMS || st.Helper.AttemptID != owner.AttemptID {
		return nil
	}
	st.Helper = nil
	return writeAntigravityJSON(m.dir, antigravityProcFile(m.dir, owner.AgyPID, owner.AgyStartMS), st)
}

// StopAntigravityWakeHelpers terminates any live recorded Antigravity wake helpers under configDir
// and clears their owned helper entries.
func StopAntigravityWakeHelpers(configDir string) error {
	if configDir == "" {
		return nil
	}
	m := antigravityHelpersMemory(configDir)
	entries, err := os.ReadDir(m.dir)
	if err != nil {
		// On Windows, ReadDir on a regular file wraps ERROR_PATH_NOT_FOUND, which matches
		// os.ErrNotExist; Lstat distinguishes a missing directory from a non-directory entry.
		if errors.Is(err, os.ErrNotExist) {
			if _, statErr := os.Lstat(m.dir); errors.Is(statErr, os.ErrNotExist) {
				return nil
			}
		}
		return err
	}
	type candidate struct {
		path string
		rec  antigravityHelperRecord
	}
	var targets []candidate
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || strings.HasPrefix(name, "conv-") || !strings.HasSuffix(name, ".json") {
			continue
		}
		full := filepath.Join(m.dir, name)
		st, ok := readAntigravityProcFile(full)
		if !ok || st.Helper == nil || st.Helper.Status != "live" {
			continue
		}
		targets = append(targets, candidate{path: full, rec: *st.Helper})
	}
	if len(targets) == 0 {
		return nil
	}
	var firstErr error
	for _, t := range targets {
		if err := terminateAntigravityProc(t.rec.PID, t.rec.StartMS); err != nil && !errors.Is(err, proc.ErrNotFound) && firstErr == nil {
			firstErr = err
		}
	}
	unlock, err := m.lock(antigravityHelperLockBudget)
	if err != nil {
		if firstErr != nil {
			return firstErr
		}
		return err
	}
	defer unlock()
	for _, t := range targets {
		st, ok := readAntigravityProcFile(t.path)
		if !ok || st.Helper == nil || st.Helper.Status != "live" {
			continue
		}
		if st.Helper.PID == t.rec.PID && st.Helper.StartMS == t.rec.StartMS && st.Helper.AttemptID == t.rec.AttemptID {
			st.Helper = nil
			if werr := writeAntigravityJSON(m.dir, t.path, st); werr != nil && firstErr == nil {
				firstErr = werr
			}
		}
	}
	return firstErr
}

func pushSupersededSession(list []string, sessionID string) []string {
	if sessionID == "" {
		return list
	}
	out := make([]string, 0, len(list)+1)
	for _, id := range list {
		if id != sessionID {
			out = append(out, id)
		}
	}
	out = append(out, sessionID)
	if len(out) > maxSupersededSessions {
		out = out[len(out)-maxSupersededSessions:]
	}
	return out
}

func removeSession(list []string, sessionID string) []string {
	if len(list) == 0 {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, id := range list {
		if id != sessionID {
			out = append(out, id)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func safeAntigravityIrisPath(p string) bool {
	if p == "" {
		return false
	}
	for i := 0; i < len(p); i++ {
		b := p[i]
		if b < 0x20 || b == 0x7f || b == '"' {
			return false
		}
	}
	return true
}

func isPlainIrisPath(p, goos string) bool {
	for i := 0; i < len(p); i++ {
		b := p[i]
		switch {
		case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9':
		case b == '_' || b == '-' || b == '.' || b == '/':
		case goos == "windows" && (b == '\\' || b == ':'):
		default:
			return false
		}
	}
	return true
}

func quoteAntigravityIrisPath(p, goos string) (string, bool) {
	if !safeAntigravityIrisPath(p) {
		return "", false
	}
	if isPlainIrisPath(p, goos) {
		return p, true
	}
	if goos == "windows" {
		return `"` + p + `"`, true
	}
	return "'" + strings.ReplaceAll(p, "'", `'\''`) + "'", true
}

func antigravityWakeReason(quotedIris string) string {
	cmd := quotedIris + " watch --client antigravity --wake"
	return "Iris wake helper request (this asks once; it cannot force execution): call `run_command` synchronously (`IsDaemon: false`, `WaitMsBeforeAsync: 5000`, and `BypassSandbox: true` when `BypassSandbox` is available) with `CommandLine` set to `" + cmd + "`, then stop without adding extra text."
}

func isIrisBinaryName(p string) bool {
	base := strings.ToLower(path.Base(strings.ReplaceAll(p, "\\", "/")))
	return strings.TrimSuffix(base, ".exe") == "iris"
}

func isAntigravityWakeCommand(cmd, irisPath string) bool {
	tokens, ok := splitWakeCommandTokens(cmd)
	if !ok || len(tokens) != 5 {
		return false
	}
	if tokens[1] != "watch" || tokens[2] != "--client" || tokens[3] != "antigravity" || tokens[4] != "--wake" {
		return false
	}
	return tokens[0] != "" && (tokens[0] == irisPath || isIrisBinaryName(tokens[0]))
}

func splitWakeCommandTokens(cmd string) ([]string, bool) {
	var tokens []string
	var cur strings.Builder
	inToken := false
	for i := 0; i < len(cmd); {
		b := cmd[i]
		switch {
		case b == ' ' || b == '\t':
			if inToken {
				tokens = append(tokens, cur.String())
				cur.Reset()
				inToken = false
			}
			i++
		case b == '\'':
			inToken = true
			i++
			end := strings.IndexByte(cmd[i:], '\'')
			if end < 0 {
				return nil, false
			}
			cur.WriteString(cmd[i : i+end])
			i += end + 1
		case b == '"':
			inToken = true
			i++
			end := strings.IndexByte(cmd[i:], '"')
			if end < 0 {
				return nil, false
			}
			cur.WriteString(cmd[i : i+end])
			i += end + 1
		case b == '\\' && i+1 < len(cmd) && cmd[i+1] == '\'':
			inToken = true
			cur.WriteByte('\'')
			i += 2
		case b < 0x20 || b == 0x7f || strings.IndexByte(";&|><`$()", b) >= 0:
			return nil, false
		default:
			inToken = true
			cur.WriteByte(b)
			i++
		}
	}
	if inToken {
		tokens = append(tokens, cur.String())
	}
	return tokens, true
}

func (r Runner) goos() string {
	if r.GOOS != "" {
		return r.GOOS
	}
	return runtime.GOOS
}

func (r Runner) irisBinaryPath() string {
	if r.IrisPath != nil {
		return r.IrisPath()
	}
	exe, err := irisExecutablePath()
	if err != nil {
		return ""
	}
	return exe
}

func (r Runner) antigravityTokenPath() string {
	if r.Getenv == nil {
		return ""
	}
	token, err := (hubclient.EnvConfig{Getenv: r.Getenv}).TokenPath()
	if err != nil {
		return ""
	}
	return token
}

func (r Runner) antigravityConfigMatchesPinned(configDir string) bool {
	fn := r.PinnedConfigDir
	if fn == nil {
		fn = pinnedAntigravityConfigDir
	}
	pinnedDir, err := fn()
	if err != nil || pinnedDir == "" {
		return false
	}
	ca, cb := filepath.Clean(configDir), filepath.Clean(pinnedDir)
	if r.goos() == "windows" {
		return strings.EqualFold(ca, cb)
	}
	return ca == cb
}

func (r Runner) antigravityAncestor() (proc.Ancestor, bool) {
	ancestor := r.Ancestor
	if ancestor == nil {
		ancestor = proc.Nearest
	}
	a, err := ancestor()
	if err != nil || a.PID <= 0 || a.StartMS <= 0 || !proc.SingleSession(a, "antigravity") {
		return proc.Ancestor{}, false
	}
	return a, true
}

func (r Runner) antigravityHook(ev Event) (Output, bool) {
	if ev.Client != ClientAntigravity || ev.SessionID == CanarySession || !validGrokSession(ev.SessionID) {
		return Output{}, false
	}
	tokenPath := r.antigravityTokenPath()
	if tokenPath == "" {
		return Output{}, false
	}
	configDir := filepath.Dir(tokenPath)
	anc, hasAnc := r.antigravityAncestor()
	wakeOn := ev.Kind == KindStop && r.antigravityConfigMatchesPinned(configDir) && AntigravityWakeEnabled(configDir)

	var stopFileID string
	var stopFileSize int64
	if ev.Kind == KindStop && ev.TranscriptPath != "" {
		if f, id, sz, err := openRegularTranscript(ev.TranscriptPath); err == nil {
			stopFileID = id
			stopFileSize = sz
			_ = f.Close()
		}
	}

	m := antigravityHelpersMemory(configDir)
	if err := m.directory(); err != nil {
		return Output{}, false
	}
	unlock, err := m.lock(hookLockBudget)
	if err != nil {
		return Output{}, false
	}

	var oldPID int
	var oldStart int64
	var askOut Output
	askContinue := false
	superseded := false

	func() {
		defer unlock()
		var procState antigravityProcState
		procDirty := false
		if hasAnc {
			procState = readAntigravityProcState(m.dir, anc.PID, anc.StartMS)
			if ev.Kind == KindSessionStart {
				if procState.CurrentSessionID != ev.SessionID {
					procState.SupersededSessionIDs = pushSupersededSession(procState.SupersededSessionIDs, procState.CurrentSessionID)
					procState.SupersededSessionIDs = removeSession(procState.SupersededSessionIDs, ev.SessionID)
					procState.CurrentSessionID = ev.SessionID
					procDirty = true
				}
			} else if slices.Contains(procState.SupersededSessionIDs, ev.SessionID) {
				superseded = true
				return
			} else if procState.CurrentSessionID != ev.SessionID {
				procState.SupersededSessionIDs = pushSupersededSession(procState.SupersededSessionIDs, procState.CurrentSessionID)
				procState.CurrentSessionID = ev.SessionID
				procDirty = true
			}
		}

		convState, _ := readAntigravityConvFile(m.dir, ev.SessionID)
		convDirty := false

		switch ev.Kind {
		case KindSessionStart, KindPostInvocation:
			if ev.TranscriptPath != "" && convState.TranscriptPath != ev.TranscriptPath {
				convState.TranscriptPath = ev.TranscriptPath
				convDirty = true
			}
		case KindPreInvocation:
			if ev.TranscriptPath != "" {
				convState.TranscriptPath = ev.TranscriptPath
			}
			convState.Idle = false
			convState.FileID = ""
			convState.IdleOffset = 0
			convDirty = true
		case KindPostTool:
			if hasAnc && ev.ToolName == "run_command" && isAntigravityWakeCommand(ev.Command, r.irisBinaryPath()) {
				if procState.Helper == nil || procState.Helper.SessionID != ev.SessionID || !helperAlive(procState.Helper, lookupAntigravityProc) {
					procState.Helper = &antigravityHelperRecord{Status: "failed"}
					procDirty = true
				}
			}
		case KindStop:
			if hasAnc && wakeOn && ev.AgyStop != nil && ev.AgyStop.FullyIdle && ev.AgyStop.TerminationReason == "NO_TOOL_CALL" {
				liveForSession := procState.Helper != nil && procState.Helper.SessionID == ev.SessionID && helperAlive(procState.Helper, lookupAntigravityProc)
				switch {
				case liveForSession:
				case procState.Helper != nil && procState.Helper.Status == "failed":
				case procState.Helper != nil && procState.Helper.Status == "arm_requested" && procState.Helper.SessionID == ev.SessionID:
					procState.Helper = &antigravityHelperRecord{Status: "failed"}
					procDirty = true
				case ev.AgyStop.ExecutionNum == 0 && !ev.AgyStop.ContinuationActive:
					if quoted, ok := quoteAntigravityIrisPath(r.irisBinaryPath(), r.goos()); ok {
						if procState.Helper != nil && procState.Helper.Status == "live" && procState.Helper.SessionID != ev.SessionID && procState.Helper.PID > 0 && procState.Helper.StartMS > 0 {
							oldPID, oldStart = procState.Helper.PID, procState.Helper.StartMS
						}
						procState.Helper = &antigravityHelperRecord{
							Status:    "arm_requested",
							SessionID: ev.SessionID,
						}
						procDirty = true
						askContinue = true
						askOut = Output{Stdout: jsonLine(agyDec{Decision: "continue", Reason: antigravityWakeReason(quoted)})}
					}
				}
			}
			if ev.TranscriptPath != "" {
				convState.TranscriptPath = ev.TranscriptPath
			}
			if !askContinue && ev.AgyStop != nil && ev.AgyStop.FullyIdle {
				convState.Idle = true
				convState.FileID = stopFileID
				convState.IdleOffset = stopFileSize
			} else {
				convState.Idle = false
				convState.FileID = ""
				convState.IdleOffset = 0
			}
			convDirty = true
		}

		if procDirty {
			if err := writeAntigravityJSON(m.dir, antigravityProcFile(m.dir, anc.PID, anc.StartMS), procState); err != nil {
				// Without a persisted arm_requested marker, Stop must fail open rather than loop.
				askContinue = false
			}
		}
		if convDirty {
			_ = writeAntigravityJSON(m.dir, antigravityConvFile(m.dir, ev.SessionID), convState)
		}
	}()

	if superseded {
		return Output{}, false
	}
	if oldPID > 0 {
		_ = terminateAntigravityProc(oldPID, oldStart)
	}
	if ev.Kind == KindPostInvocation {
		_, _, _ = ScanAntigravityWakeSession(NewSessions(tokenPath), ev.SessionID, ev.TranscriptPath, r.now(), hookLockBudget)
	}
	if askContinue {
		return askOut, true
	}
	return Output{}, false
}

type antigravityScanTarget struct {
	AttemptID      string
	Message        string
	ObservedStored AntigravityScanCursor
	Cursor         AntigravityScanCursor
}

type antigravityScanPass struct {
	AttemptID          string
	Matched            bool
	ObservedStored     AntigravityScanCursor
	RotatedOrTruncated bool
	HasMore            bool
	Next               AntigravityScanCursor
}

func attemptClosed(a map[string]json.RawMessage) bool {
	if attemptTime(a, "consumed_ms") != 0 {
		return true
	}
	ans := attemptText(a, "answer")
	return ans == "failed" || ans == "aborted"
}

func readAntigravityScanTarget(wakePath, transcriptHint string) (antigravityScanTarget, bool) {
	d, ok := readWake(wakePath)
	if !ok || len(d.attempts) == 0 {
		return antigravityScanTarget{}, false
	}
	newest := d.attempts[len(d.attempts)-1]
	if attemptClosed(newest) {
		return antigravityScanTarget{}, false
	}
	id := attemptText(newest, "id")
	msg := attemptText(newest, "message")
	if id == "" || msg == "" {
		return antigravityScanTarget{}, false
	}
	var stored AntigravityScanCursor
	_ = json.Unmarshal(newest["scan"], &stored)
	cur := stored
	if transcriptHint != "" && transcriptHint != cur.TranscriptPath {
		cur = AntigravityScanCursor{TranscriptPath: transcriptHint}
	}
	if cur.TranscriptPath == "" {
		return antigravityScanTarget{}, false
	}
	return antigravityScanTarget{AttemptID: id, Message: msg, ObservedStored: stored, Cursor: cur}, true
}

func runAntigravityScanPass(target antigravityScanTarget, sessionID string) (antigravityScanPass, bool) {
	f, fileID, size, err := openRegularTranscript(target.Cursor.TranscriptPath)
	if err != nil {
		return antigravityScanPass{}, false
	}
	defer func() { _ = f.Close() }()

	rotatedOrTruncated := target.Cursor.FileID == "" || fileID != target.Cursor.FileID || size < target.Cursor.ScannedOffset
	startOffset := target.Cursor.ScannedOffset
	skipping := target.Cursor.SkippingLine
	if rotatedOrTruncated {
		startOffset = 0
		skipping = false
	}

	buf, err := readTranscriptChunk(f, startOffset, antigravityScanLimit)
	if err != nil {
		return antigravityScanPass{}, false
	}

	offset := startOffset
	rem := buf
	for len(rem) > 0 {
		nl := bytes.IndexByte(rem, '\n')
		if skipping {
			if nl >= 0 {
				offset += int64(nl + 1)
				rem = rem[nl+1:]
				skipping = false
				continue
			}
			offset += int64(len(rem))
			break
		}
		if nl >= 0 {
			line := rem[:nl]
			if len(line) <= antigravityLineLimit && matchesAntigravityWakeLine(line, sessionID, target.Message) {
				return antigravityScanPass{AttemptID: target.AttemptID, Matched: true}, true
			}
			offset += int64(nl + 1)
			rem = rem[nl+1:]
			continue
		}
		if len(rem) > antigravityLineLimit {
			skipping = true
			offset += int64(len(rem))
		}
		break
	}

	return antigravityScanPass{
		AttemptID:          target.AttemptID,
		ObservedStored:     target.ObservedStored,
		RotatedOrTruncated: rotatedOrTruncated,
		HasMore:            startOffset+int64(len(buf)) < size,
		Next: AntigravityScanCursor{
			TranscriptPath: target.Cursor.TranscriptPath,
			FileID:         fileID,
			ScannedOffset:  offset,
			SkippingLine:   skipping,
		},
	}, true
}

const antigravitySystemPreamble = "The following is a <SYSTEM_MESSAGE> not actually sent by the user. It is provided by the system as important information to pay attention to.\n\n"

func matchesAntigravityWakeLine(line []byte, sessionID, wantMessage string) bool {
	var step struct {
		Source  string `json:"source"`
		Type    string `json:"type"`
		Content string `json:"content"`
	}
	if json.Unmarshal(line, &step) != nil || step.Source != "SYSTEM" || step.Type != "SYSTEM_MESSAGE" {
		return false
	}
	body := strings.TrimPrefix(step.Content, antigravitySystemPreamble)
	body, ok := strings.CutPrefix(body, "<SYSTEM_MESSAGE>\n")
	if !ok {
		return false
	}
	body, ok = strings.CutSuffix(body, "\n</SYSTEM_MESSAGE>")
	if !ok {
		return false
	}
	rest, ok := strings.CutPrefix(body, "[Message] timestamp=")
	if !ok {
		return false
	}
	ts, rest, ok := strings.Cut(rest, " ")
	if !ok || ts == "" || strings.ContainsAny(ts, "\t\r\n") {
		return false
	}
	rest, ok = strings.CutPrefix(rest, "sender=")
	if !ok {
		return false
	}
	sender, rest, ok := strings.Cut(rest, " ")
	if !ok || (sender != sessionID && sender != "system") {
		return false
	}
	rest, ok = strings.CutPrefix(rest, "priority=")
	if !ok {
		return false
	}
	prio, rest, ok := strings.Cut(rest, " ")
	if !ok || prio == "" || strings.ContainsAny(prio, "\t\r\n") {
		return false
	}
	payload, ok := strings.CutPrefix(rest, "content=")
	return ok && payload == wantMessage
}

func shouldAdvanceAntigravityCursor(stored AntigravityScanCursor, pass antigravityScanPass) bool {
	if pass.RotatedOrTruncated {
		return stored == pass.ObservedStored
	}
	return stored.TranscriptPath == pass.Next.TranscriptPath &&
		stored.FileID == pass.Next.FileID &&
		pass.Next.FileID != "" &&
		(pass.Next.ScannedOffset > stored.ScannedOffset ||
			(pass.Next.ScannedOffset == stored.ScannedOffset && pass.Next.SkippingLine && !stored.SkippingLine))
}

// ScanAntigravityWakeSession executes one bounded (<= 4 MiB) lock-free forward scan pass on
// sessionID's transcript and merges consumption or monotonic cursor progress by attempt.ID.
func ScanAntigravityWakeSession(s *Sessions, sessionID, transcriptHint string, now time.Time, budget time.Duration) (consumed, hasMore bool, err error) {
	if s == nil || !validGrokSession(sessionID) {
		return false, false, nil
	}
	wakePath := s.WakePath("antigravity", sessionID)
	target, ok := readAntigravityScanTarget(wakePath, transcriptHint)
	if !ok {
		return false, false, nil
	}
	pass, ok := runAntigravityScanPass(target, sessionID)
	if !ok {
		return false, false, nil
	}
	if !pass.Matched && !pass.RotatedOrTruncated && pass.Next == target.Cursor {
		return false, false, nil
	}
	if err := s.Protect(); err != nil {
		return false, false, err
	}
	unlock, err := s.Lock(budget)
	if err != nil {
		return false, false, err
	}
	defer unlock()

	d, ok := readWake(wakePath)
	if !ok || len(d.attempts) == 0 {
		return false, false, nil
	}
	newest := d.attempts[len(d.attempts)-1]
	if attemptText(newest, "id") != pass.AttemptID || attemptClosed(newest) {
		return false, false, nil
	}
	if pass.Matched {
		newest["consumed_ms"] = json.RawMessage(strconv.FormatInt(now.UnixMilli(), 10))
		d.forget(now.Add(-memoryAge))
		return true, false, s.memory.replace(wakePath, d.encode())
	}
	var stored AntigravityScanCursor
	_ = json.Unmarshal(newest["scan"], &stored)
	if !shouldAdvanceAntigravityCursor(stored, pass) {
		return false, false, nil
	}
	// AntigravityScanCursor holds only strings, int64 and bool, so Marshal cannot fail.
	rawScan, _ := json.Marshal(pass.Next)
	newest["scan"] = rawScan
	return false, pass.HasMore, s.memory.replace(wakePath, d.encode())
}
