package binding

import (
	"context"
	"time"

	"github.com/mateusz-klatt/iris/internal/sessionproof"
)

// EvidenceStore reads a session's files in one bounded snapshot. An inspection
// with succession disabled skips the contents of the exact execution record.
type EvidenceStore interface {
	InspectEvidence(client, session string, deadline time.Time, successionEnabled, cleanExpired bool) (sessionproof.Snapshot, error)
	ObserveRefusal(client, session, suffix string, damaged bool)
}

func (r *Resolver) successionEnabled(ctx context.Context) bool {
	return r.SuccessionEnabled != nil && r.SuccessionEnabled(ctx)
}

// inspectEvidence keeps the legacy reader when succession is off, and uses the
// single-lock snapshot for succession evidence. It leaves retirement to writers.
func (r *Resolver) inspectEvidence(ctx context.Context, session string, opted bool) (sessionproof.Snapshot, error) {
	if session == "" {
		return sessionproof.Snapshot{}, nil
	}
	if r.Evidence == nil || (!opted && r.Recall != nil) {
		gen, ended, err := r.recallGeneration(session)
		snap := sessionproof.Snapshot{
			Generation: gen, GenerationState: sessionproof.FileAbsent,
			Ended: ended, EndedState: sessionproof.FileAbsent,
			RecordState: sessionproof.FileSkipped,
		}
		if gen > 0 {
			snap.GenerationState = sessionproof.FileValid
		}
		if ended > 0 {
			snap.EndedState = sessionproof.FileValid
		}
		return snap, err
	}
	deadline, _ := ctx.Deadline()
	snap, err := r.Evidence.InspectEvidence(r.Client, session, deadline, opted, false)
	if err != nil {
		return sessionproof.Snapshot{}, errMalformedGeneration
	}
	for _, file := range []struct {
		suffix string
		state  sessionproof.FileState
	}{
		{".ended", snap.EndedState},
		{"", snap.GenerationState},
		{".execution", snap.RecordState},
	} {
		if file.state == sessionproof.FileDamaged {
			r.Evidence.ObserveRefusal(r.Client, session, file.suffix, true)
			return sessionproof.Snapshot{}, errMalformedGeneration
		}
	}
	return snap, nil
}

// admitCandidate rechecks files after a request, retaining any floor already
// observed by that attempt even if a writer has retired a file in the meantime.
func (r *Resolver) admitCandidate(ctx context.Context, b Bound, retained int, ownSession, allowHookless, opted bool, prior sessionproof.Snapshot) error {
	snap, err := r.inspectEvidence(ctx, b.SessionID, opted)
	if err != nil {
		return err
	}
	if !opted {
		prior.RecordState = sessionproof.FileSkipped
	}
	snap.Ended = max(snap.Ended, prior.Ended)
	if prior.GenerationFloor() > snap.GenerationFloor() {
		snap.Generation, snap.GenerationState = prior.GenerationFloor(), sessionproof.FileValid
	}
	if pair, ok := prior.ExactPair(); ok && pair.Generation == b.Generation && pair.ExecutionID != b.ExecutionID {
		r.Evidence.ObserveRefusal(r.Client, b.SessionID, ".execution", false)
		return errGenerationFloor
	}
	return r.checkCandidate(b, retained, ownSession, allowHookless, opted, snap)
}

func (r *Resolver) checkCandidate(b Bound, retained int, ownSession, allowHookless, opted bool, snap sessionproof.Snapshot) error {
	endedVeto := snap.Ended > 0 && b.Generation <= snap.Ended
	liveVeto := ownSession && b.Generation < snap.GenerationFloor()
	if endedVeto || liveVeto || ownSession && b.Generation < retained {
		if snap.BasePath != "" && (endedVeto || liveVeto) {
			suffix := ""
			if endedVeto {
				suffix = ".ended"
			} else if snap.RecordState == sessionproof.FileValid && snap.Record.Generation > b.Generation {
				suffix = ".execution"
			}
			r.Evidence.ObserveRefusal(r.Client, b.SessionID, suffix, false)
			return errGenerationFloor
		}
		return r.refuseBindingGeneration(b.SessionID, liveVeto, endedVeto)
	}
	if r.wakeSession != "" && (opted || b.Classified) && !b.HookSeen {
		return errGenerationFloor
	}
	hookless := b.Class.IsHooklessSuccession()
	if hookless && (!opted || !allowHookless || r.ParentAlive == nil || !r.ParentAlive()) {
		return errGenerationFloor
	}
	if pair, ok := snap.ExactPair(); ok && pair.Generation == b.Generation && pair.ExecutionID != b.ExecutionID {
		r.Evidence.ObserveRefusal(r.Client, b.SessionID, ".execution", false)
		return errGenerationFloor
	}
	if !hookless {
		return nil
	}
	record := snap.Record
	if snap.RecordState != sessionproof.FileValid || record.Origin != sessionproof.OriginShim ||
		record.Pair() != (sessionproof.Pair{Generation: b.Generation, ExecutionID: b.ExecutionID}) ||
		record.PriorGeneration != b.Class.PriorGeneration || record.PriorExecutionID != b.Class.PriorExecutionID {
		if r.Evidence != nil {
			r.Evidence.ObserveRefusal(r.Client, b.SessionID, ".execution", false)
		}
		return errGenerationFloor
	}
	if snap.Ended >= record.PriorGeneration {
		r.Evidence.ObserveRefusal(r.Client, b.SessionID, ".ended", false)
		return errGenerationFloor
	}
	return nil
}
