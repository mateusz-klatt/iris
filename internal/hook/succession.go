package hook

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/mateusz-klatt/iris/internal/proc"
)

// ErrSuccessionProof means local proof or current-session evidence does not permit succession.
var ErrSuccessionProof = errors.New("session succession proof does not match")

// processSessionLookup reads a single-session process's current session. PrepareSuccession calls
// it outside the sessions lock, twice, and fails closed if the two reads differ.
var processSessionLookup = currentProcessSession

func processWitnessPath(dir string, anc proc.Ancestor) string {
	return filepath.Join(dir, fmt.Sprintf("proc-%d-%d", anc.PID, anc.StartMS))
}

// rememberProcessSession records a single-session launcher's current native session before its
// SessionStart registration. Antigravity publishes its established process state separately.
func (m fileMemory) rememberProcessSession(client, session string, anc proc.Ancestor) (err error) {
	if anc.PID <= 0 || anc.StartMS <= 0 || !proc.SingleSession(anc, client) || client == string(ClientAntigravity) {
		return nil
	}
	path := processWitnessPath(m.dir, anc)
	defer func() {
		if err != nil {
			// A missed lock or failed replace withdraws the old witness without another lock wait.
			if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
				err = errors.Join(err, removeErr)
			}
		}
	}()
	if err := m.directory(); err != nil {
		return err
	}
	unlock, err := m.lock(hookLockBudget)
	if err != nil {
		return err
	}
	defer unlock()
	return m.replace(path, session)
}

type executionProof struct {
	generation int
	id         string
}

type executionProofs [2]executionProof

func (proofs executionProofs) at(generation int) string {
	for _, proof := range proofs {
		if proof.generation == generation && proof.id != "-" {
			return proof.id
		}
	}
	return ""
}

func readExecutionProofs(path string) (executionProofs, error) {
	var proofs executionProofs
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
			return proofs, nil
		}
	}
	if err != nil {
		return proofs, ErrInvalidGeneration
	}
	fields := strings.Fields(string(raw))
	// Pre-release sidecars stored a bare ID. Its generation cannot be established, so it
	// remains readable but supplies no exact proof until a registering hook records the pair.
	if len(fields) == 1 && validExecutionProofID(fields[0]) {
		return proofs, nil
	}
	if len(fields) != 2 && len(fields) != 4 {
		return proofs, ErrInvalidGeneration
	}
	previous := 0
	for i := 0; i < len(fields); i += 2 {
		generation, err := strconv.Atoi(fields[i])
		id := fields[i+1]
		if err != nil || generation <= previous || (id != "-" && !validExecutionProofID(id)) {
			return executionProofs{}, ErrInvalidGeneration
		}
		proofs[i/2] = executionProof{generation, id}
		previous = generation
	}
	return proofs, nil
}

func validExecutionProofID(id string) bool {
	if !strings.HasPrefix(id, "ex_") || len(id) == 3 {
		return false
	}
	for _, c := range id[3:] {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '_' && c != '-' {
			return false
		}
	}
	return true
}

func readExecutionProof(path string, generation int) (string, error) {
	proofs, err := readExecutionProofs(path)
	return proofs.at(generation), err
}

// publishGeneration prepares the sidecar while preserving the pair named by the integer marker.
// The marker keeps its legacy format and a failed rename leaves the old pair available for retry.
// The caller holds Sessions.Lock for both replacements.
func (m fileMemory) publishGeneration(path string, current int, proofs executionProofs, generation int, executionID string) error {
	// A counter-only write must not accidentally commit an ID staged by an earlier write.
	if executionID == "" && current < generation && proofs.at(generation) != "" {
		executionID = "-"
	}
	if executionID != "" {
		body := strconv.Itoa(generation) + " " + executionID
		if priorID := proofs.at(current); current < generation && priorID != "" {
			body = strconv.Itoa(current) + " " + priorID + "\n" + body
		}
		if err := m.replace(path+".execution", body); err != nil {
			return err
		}
	}
	if current == generation {
		m.touchGeneration(path)
		return nil
	}
	return m.replace(path, strconv.Itoa(generation))
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
	current, superseded, err := s.currentSession(client, anc)
	if slices.Contains(superseded, current) {
		return "", false, ErrSuccessionProof
	}
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
	executionID, err = readExecutionProof(executionPath, generation)
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

// AdvanceGeneration compares and advances the local proof pair without recreating forgotten proof.
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
	proofs, execErr := readExecutionProofs(executionPath)
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
	if current != expectedOld || proofs.at(current) != expectedExec {
		return current, false, nil
	}
	if err := s.memory.publishGeneration(path, current, proofs, newGen, newExec); err != nil {
		return current, false, err
	}
	return newGen, true, nil
}
