package store

import "context"

// GenerationChange contains only hub-assigned identities, counters and fixed
// labels. A registration's retry marker is a client claim for diagnostics only;
// it is never evidence that grants authority to a lost generation.
type GenerationChange struct {
	ExecutionID, PreviousExecutionID, Reason, Source, Proof string
	Generation, PreviousGeneration, RetryGeneration         int64
}

type generationObserverKey struct{}

// WithGenerationObserver observes replacement generations only after their
// transaction commits and releases its lock. A live session's repeated
// registration, resume and restart reattachment preserve its generation and
// emit no replacement.
func WithGenerationObserver(ctx context.Context, observe func(GenerationChange)) context.Context {
	return context.WithValue(ctx, generationObserverKey{}, observe)
}

func (t *transaction) generationChanged(previous, current execution, r Request) {
	if previous.Generation == 0 {
		return
	}
	reason := previous.State
	if reason == "lost" {
		reason = "lease_expired"
		if previous.Recovery.Valid {
			reason = "recovery_expired"
		} else if previous.Epoch != t.s.epoch {
			// Loss may have committed before a later restart changed the epoch.
			// Without a persisted cause, the mismatch cannot explain that loss.
			reason = "prior_lost"
		}
	}
	source := r.Source
	switch source {
	case "hook", "shim", "watch", "mcp":
	case "":
		source = "legacy"
	default:
		source = "other"
	}
	proof := "absent"
	if r.RetryGeneration > 0 {
		proof = "stale_retry"
	}
	t.generationChange = &GenerationChange{
		ExecutionID: current.ID, PreviousExecutionID: previous.ID,
		Generation: current.Generation, PreviousGeneration: previous.Generation,
		Reason: reason, Source: source, Proof: proof, RetryGeneration: max(r.RetryGeneration, 0),
	}
}
