package hook

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/mateusz-klatt/iris/internal/sessionproof"
)

// SessionEvidenceWriteFailed names the local observation recorded when hook evidence persistence fails.
const SessionEvidenceWriteFailed = "session evidence write failed"

var (
	lstatSessionFile  = os.Lstat
	removeSessionFile = os.Remove
)

func executionPath(prefix string) string {
	return prefix + ".execution"
}

func recordEndedAt(rec sessionproof.Record, ended int) bool {
	return rec.Generation <= ended || (rec.Origin == sessionproof.OriginShim && rec.PriorGeneration <= ended)
}

func prepareSessionFile(dir, body string, restrict func(string) error) (string, error) {
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return "", err
	}
	tmp := f.Name()
	if body != "" {
		_, err = f.WriteString(strings.TrimRight(body, "\n") + "\n")
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	// The file is protected before it gets its name, so it is never readable as an entry.
	if err == nil && restrict != nil {
		err = restrict(tmp)
	}
	if err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	return tmp, nil
}

func writePreparedSessionFile(path, body string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, err = f.WriteString(strings.TrimRight(body, "\n") + "\n")
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	return err
}

func readExactRecordDeadline(path string, now func() time.Time, deadline time.Time) (sessionproof.Record, sessionproof.FileState, error) {
	raw, err := readRegularBoundedBytesDeadline(path, sessionproof.RecordReadLimit, now, deadline)
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return sessionproof.Record{}, "", err
	}
	if errors.Is(err, os.ErrNotExist) {
		if deadlineExpired(now, deadline) {
			return sessionproof.Record{}, "", os.ErrDeadlineExceeded
		}
		if _, lerr := lstatSessionFile(path); errors.Is(lerr, os.ErrNotExist) {
			return sessionproof.Record{}, sessionproof.FileAbsent, nil
		}
		return sessionproof.Record{}, sessionproof.FileDamaged, sessionproof.ErrInvalidRecord
	}
	if err != nil {
		return sessionproof.Record{}, sessionproof.FileDamaged, fmt.Errorf("%w: %v", sessionproof.ErrInvalidRecord, err)
	}
	rec, decErr := sessionproof.DecodeRecord(raw)
	if decErr != nil {
		return sessionproof.Record{}, sessionproof.FileDamaged, decErr
	}
	return rec, sessionproof.FileValid, nil
}

// cleanupExpiredEnd removes aged session proof and exact execution artifacts when allowed,
// leaving <hash>.ended in place until every eligible file is retired and removing .ended LAST.
func (m fileMemory) cleanupExpiredEnd(prefix string, ended int, deadline time.Time, successionEnabled bool) (bool, error) {
	mono := m.monoClock()
	marker := prefix + ".ended"
	execPath := executionPath(prefix)

	current, genErr := readGenerationDeadline(prefix, mono, deadline)
	if genErr != nil {
		return false, genErr
	}
	removeHash := current > 0 && current <= ended

	removeExec := false
	if successionEnabled {
		rec, recState, recErr := readExactRecordDeadline(execPath, mono, deadline)
		if errors.Is(recErr, os.ErrDeadlineExceeded) {
			return false, recErr
		}
		if recState == sessionproof.FileDamaged {
			return false, recErr
		}
		if recState == sessionproof.FileValid && recordEndedAt(rec, ended) {
			removeExec = true
		}
	} else if current <= ended {
		if deadlineExpired(mono, deadline) {
			return false, os.ErrDeadlineExceeded
		}
		if _, lerr := lstatSessionFile(execPath); lerr == nil {
			removeExec = true
		} else if !errors.Is(lerr, os.ErrNotExist) {
			return false, lerr
		}
	}

	if removeHash {
		if deadlineExpired(mono, deadline) {
			return false, os.ErrDeadlineExceeded
		}
		if err := removeSessionFile(prefix); err != nil {
			return false, err
		}
	}
	if removeExec {
		if deadlineExpired(mono, deadline) {
			return false, os.ErrDeadlineExceeded
		}
		if err := removeSessionFile(execPath); err != nil {
			return false, err
		}
	}
	if deadlineExpired(mono, deadline) {
		return false, os.ErrDeadlineExceeded
	}
	if err := removeSessionFile(marker); err != nil {
		return false, err
	}
	return true, nil
}

func (m fileMemory) inspectEvidenceDeadline(client, session string, callerDeadline time.Time, successionEnabled, cleanExpired bool) (sessionproof.Snapshot, error) {
	if client == "" || session == "" {
		return sessionproof.Snapshot{}, nil
	}
	prefix := m.name(client, session)
	snap := sessionproof.Snapshot{BasePath: prefix}
	deadline := m.lockBudgetDeadline(callerDeadline)
	mono := m.monoClock()
	if deadlineExpired(mono, deadline) {
		return snap, os.ErrDeadlineExceeded
	}
	if _, err := lstatSessionFile(m.dir); errors.Is(err, os.ErrNotExist) {
		snap.GenerationState = sessionproof.FileAbsent
		snap.EndedState = sessionproof.FileAbsent
		if successionEnabled {
			snap.RecordState = sessionproof.FileAbsent
		} else {
			snap.RecordState = sessionproof.FileSkipped
		}
		return snap, nil
	}
	var inspectErr error
	if err := m.underLockDeadline(deadline, func() {
		ended, expired, markerErr := m.readEndMarkerDeadline(prefix+".ended", deadline)
		if errors.Is(markerErr, os.ErrDeadlineExceeded) {
			inspectErr = markerErr
			return
		}
		if cleanExpired && markerErr == nil && ended > 0 && expired {
			_, cleanErr := m.cleanupExpiredEnd(prefix, ended, deadline, successionEnabled)
			if errors.Is(cleanErr, os.ErrDeadlineExceeded) {
				inspectErr = cleanErr
				return
			}
			ended, _, markerErr = m.readEndMarkerDeadline(prefix+".ended", deadline)
			if errors.Is(markerErr, os.ErrDeadlineExceeded) {
				inspectErr = markerErr
				return
			}
		}
		switch {
		case markerErr != nil:
			snap.EndedState = sessionproof.FileDamaged
		case ended > 0:
			snap.Ended = ended
			snap.EndedState = sessionproof.FileValid
		default:
			snap.EndedState = sessionproof.FileAbsent
		}

		current, genErr := readGenerationDeadline(prefix, mono, deadline)
		switch {
		case errors.Is(genErr, os.ErrDeadlineExceeded):
			inspectErr = genErr
			return
		case genErr != nil:
			snap.GenerationState = sessionproof.FileDamaged
		case current > 0:
			snap.Generation = current
			snap.GenerationState = sessionproof.FileValid
		default:
			snap.GenerationState = sessionproof.FileAbsent
		}

		if !successionEnabled {
			snap.RecordState = sessionproof.FileSkipped
			return
		}
		rec, recState, recErr := readExactRecordDeadline(executionPath(prefix), mono, deadline)
		if errors.Is(recErr, os.ErrDeadlineExceeded) {
			inspectErr = recErr
			return
		}
		snap.Record = rec
		snap.RecordState = recState
	}); err != nil {
		return snap, err
	}
	return snap, inspectErr
}

// isExecutionFile reports whether path ends in the exact-record suffix.
func isExecutionFile(path string) bool {
	return strings.HasSuffix(path, ".execution")
}
