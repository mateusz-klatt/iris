package hook

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/mateusz-klatt/iris/internal/config"
	"github.com/mateusz-klatt/iris/internal/kimiwake"
	"github.com/mateusz-klatt/iris/internal/proc"
)

// ErrSuccessionProof means local proof or current-session evidence does not permit succession.
var ErrSuccessionProof = errors.New("session succession proof does not match")

var successionPinnedDir = config.PinnedDir

// processSessionLookup reads a single-session process's current session. PrepareSuccession calls
// it outside the sessions lock, twice, and fails closed if the two reads differ.
var processSessionLookup = currentProcessSession

func processWitnessPath(dir string, anc proc.Ancestor) string {
	return filepath.Join(dir, fmt.Sprintf("proc-%d-%d", anc.PID, anc.StartMS))
}

// rememberProcessSession records a single-session launcher's current native session before its
// SessionStart registration. Antigravity and Kimi publish their established process state instead.
func (m fileMemory) rememberProcessSession(client, session string, anc proc.Ancestor) error {
	if anc.PID <= 0 || anc.StartMS <= 0 || !proc.SingleSession(anc, client) || client == string(ClientAntigravity) || client == string(ClientKimi) {
		return nil
	}
	if err := m.directory(); err != nil {
		return err
	}
	return m.underLock(func() {
		_ = m.replace(processWitnessPath(m.dir, anc), session)
	})
}

func readExecutionProof(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
	}
	if err != nil {
		return "", ErrInvalidGeneration
	}
	id := strings.TrimSpace(string(raw))
	if !strings.HasPrefix(id, "ex_") || len(id) == 3 {
		return "", ErrInvalidGeneration
	}
	for _, c := range id[3:] {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '_' && c != '-' {
			return "", ErrInvalidGeneration
		}
	}
	return id, nil
}

func currentProcessSession(client string, anc proc.Ancestor, sessionsDir string) (string, []string, error) {
	switch client {
	case string(ClientAntigravity):
		m := antigravityHelpersMemory(filepath.Dir(sessionsDir))
		dir, err := os.Lstat(m.dir)
		if errors.Is(err, os.ErrNotExist) {
			return "", nil, nil
		}
		if err != nil || !dir.IsDir() {
			return "", nil, ErrSuccessionProof
		}
		path := antigravityProcFile(m.dir, anc.PID, anc.StartMS)
		if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
			return "", nil, nil
		}
		unlock, err := m.lock(hookLockBudget)
		if err != nil {
			return "", nil, err
		}
		defer unlock()
		state, ok := readAntigravityProcFile(path)
		if !ok || state.CurrentSessionID == "" {
			return "", nil, ErrSuccessionProof
		}
		return state.CurrentSessionID, state.SupersededSessionIDs, nil
	case string(ClientKimi):
		dir, err := successionPinnedDir()
		if err != nil {
			return "", nil, err
		}
		current, _, err := (&kimiwake.Store{Root: kimiwake.Root(dir)}).Session(anc.PID, anc.StartMS, hookLockBudget)
		return current, nil, err
	default:
		raw, err := os.ReadFile(processWitnessPath(sessionsDir, anc))
		if errors.Is(err, os.ErrNotExist) {
			return "", nil, nil
		}
		if err != nil {
			return "", nil, err
		}
		current := strings.TrimSpace(string(raw))
		if current == "" {
			return "", nil, ErrSuccessionProof
		}
		return current, nil, nil
	}
}

func (s *Sessions) currentSession(client string, anc proc.Ancestor) (string, []string, error) {
	return currentProcessSession(client, anc, s.memory.dir)
}

// CurrentSession identifies the current session of a positively single-session client process.
func (s *Sessions) CurrentSession(client string, anc proc.Ancestor) (string, bool, error) {
	if anc.PID <= 0 || anc.StartMS <= 0 || !proc.SingleSession(anc, client) {
		return "", false, nil
	}
	current, _, err := s.currentSession(client, anc)
	return current, current != "", err
}

// PrepareSuccession reads exact generation and Execution proof and checks /clear state.
// The sessions lock covers only the hash files. A single-session witness is read under its own
// lock before that and again after it is released; a witness that changed in between fails closed.
func (s *Sessions) PrepareSuccession(client, session string, minimumGeneration int, lastExecution string, anc proc.Ancestor) (int, string, error) {
	if err := s.Protect(); err != nil {
		return 0, "", err
	}
	checkWitness := proc.SingleSession(anc, client)
	var beforeCurrent string
	var beforeSuperseded []string
	var witnessErr error
	if checkWitness {
		beforeCurrent, beforeSuperseded, witnessErr = processSessionLookup(client, anc, s.memory.dir)
	}
	generation, executionID, err := s.readSuccessionProof(client, session, minimumGeneration, lastExecution)
	if err != nil {
		return 0, "", err
	}
	if !checkWitness {
		return generation, executionID, nil
	}
	if witnessErr != nil {
		return 0, "", witnessErr
	}
	afterCurrent, afterSuperseded, err := processSessionLookup(client, anc, s.memory.dir)
	if err != nil {
		return 0, "", err
	}
	if afterCurrent != beforeCurrent || !slices.Equal(afterSuperseded, beforeSuperseded) || afterCurrent != session || slices.Contains(afterSuperseded, session) {
		return 0, "", ErrSuccessionProof
	}
	return generation, executionID, nil
}

// readSuccessionProof reads generation proof while holding only the sessions lock.
func (s *Sessions) readSuccessionProof(client, session string, minimumGeneration int, lastExecution string) (generation int, executionID string, err error) {
	unlock, err := s.Lock(hookLockBudget)
	if err != nil {
		return 0, "", err
	}
	damaged := ""
	defer func() {
		unlock()
		if damaged != "" {
			s.recordDamagedGeneration(client, damaged)
		}
	}()
	path := s.memory.name(client, session)
	generation, err = readGeneration(path)
	if err != nil {
		damaged = path
		return 0, "", err
	}
	if generation == 0 || generation < minimumGeneration {
		return 0, "", ErrSuccessionProof
	}
	executionPath := path + ".execution"
	executionID, err = readExecutionProof(executionPath)
	if err != nil {
		damaged = executionPath
		return 0, "", err
	}
	if executionID == "" || lastExecution != "" && executionID != lastExecution {
		return 0, "", ErrSuccessionProof
	}
	endedPath := path + ".ended"
	ended, err := readGeneration(endedPath)
	if err != nil {
		damaged = endedPath
		return 0, "", err
	}
	if ended >= generation {
		return 0, "", ErrSuccessionProof
	}
	return generation, executionID, nil
}

// AdvanceGeneration compares and advances both local proof files without recreating forgotten proof.
func (s *Sessions) AdvanceGeneration(client, session, expectedExec string, expectedOld, newGen int, newExec string) (current int, updated bool, err error) {
	if expectedOld <= 0 || expectedExec == "" || newGen != expectedOld+1 || newExec == "" {
		return 0, false, ErrSuccessionProof
	}
	if err := s.Protect(); err != nil {
		return 0, false, err
	}
	unlock, err := s.Lock(hookLockBudget)
	if err != nil {
		return 0, false, err
	}
	damaged := ""
	defer func() {
		unlock()
		if damaged != "" {
			s.recordDamagedGeneration(client, damaged)
		}
	}()
	path := s.memory.name(client, session)
	current, err = readGeneration(path)
	if err != nil {
		damaged = path
		return 0, false, err
	}
	executionPath := path + ".execution"
	executionID, execErr := readExecutionProof(executionPath)
	if execErr != nil {
		damaged = executionPath
		return current, false, execErr
	}
	endedPath := path + ".ended"
	ended, endedErr := readGeneration(endedPath)
	if endedErr != nil {
		damaged = endedPath
		return current, false, endedErr
	}
	if current > expectedOld {
		return current, false, nil
	}
	if current == 0 || ended >= expectedOld {
		return 0, false, nil
	}
	if current != expectedOld || executionID != expectedExec {
		return current, false, nil
	}
	// Replace the execution file first. A failure before the generation file is replaced leaves
	// the remembered generation at expectedOld, so a reader of <hash> does not observe an advance
	// that did not finish.
	if err := s.memory.replace(executionPath, newExec); err != nil {
		return current, false, err
	}
	if err := s.memory.replace(path, strconv.Itoa(newGen)); err != nil {
		return current, false, err
	}
	return newGen, true, nil
}
