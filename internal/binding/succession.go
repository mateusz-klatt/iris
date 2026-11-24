package binding

import (
	"context"

	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/proc"
)

// succeedAfterLostHeld makes one guarded post-window attempt after exact-generation reattachment
// was refused. It never converts the attempt into generation-zero registration.
func (r *Resolver) succeedAfterLostHeld(ctx context.Context, session string, rung, generationFloor int) (Bound, bool, error) {
	if r.Succession == nil || r.HeartbeatSource == "watch" || r.wakeSession != "" || r.ParentAlive == nil {
		return Bound{}, false, nil
	}
	if !r.ParentAlive() {
		return Bound{}, true, errLost
	}
	processBound := rung == 3 || r.last.SessionID == session && r.last.Rung == 3
	var anc proc.Ancestor
	// Id-mode root succession is pinned to the session id, so its rung is 2. When the shim's own
	// ancestor walk finds the session's single-session client process, the hub still requires that
	// verified process identity. The pid and start come from the walk, never from the environment
	// or the tool-call request.
	if !processBound {
		if mode := r.decided.Load(); mode != nil && mode.mode == root && r.Ancestor != nil {
			if walked, err := r.Ancestor(); err == nil && walked.PID > 0 && walked.StartMS > 0 && proc.SingleSession(walked, r.Client) {
				anc = walked
				processBound = true
			}
		}
	}
	if processBound && anc == (proc.Ancestor{}) {
		if r.Ancestor == nil {
			return Bound{}, true, errGenerationFloor
		}
		var err error
		anc, err = r.Ancestor()
		if err != nil {
			return Bound{}, true, &LostError{Cause: err}
		}
		if anc.PID <= 0 || anc.StartMS <= 0 || (!proc.SingleSession(anc, r.Client) && (r.Client != "claude" || rung != 3)) {
			return Bound{}, true, errGenerationFloor
		}
	}
	lastExecution := ""
	if r.last.SessionID == session {
		lastExecution = r.last.ExecutionID
	}
	priorGeneration, priorExecution, err := r.Succession.PrepareSuccession(r.Client, session, max(generationFloor, r.processGenerationHeld(session)), lastExecution, anc)
	if err != nil {
		return Bound{}, true, &LostError{Cause: err}
	}
	proofGeneration, endedThrough, err := r.recallGeneration(session)
	if err != nil {
		return Bound{}, true, &LostError{Cause: err}
	}
	if proofGeneration != priorGeneration {
		return Bound{}, true, errGenerationFloor
	}
	if endedThrough >= priorGeneration {
		return Bound{}, true, r.refuseBindingGeneration(session, false, true)
	}
	if processBound {
		after, err := r.Ancestor()
		if err != nil {
			return Bound{}, true, &LostError{Cause: err}
		}
		if after.PID != anc.PID || after.StartMS != anc.StartMS || after.Name != anc.Name {
			return Bound{}, true, errGenerationFloor
		}
	}
	request := map[string]any{
		"client": r.Client, "host": r.Host, "session_id": session, "source": "shim",
		"prior_generation": priorGeneration, "prior_execution_id": priorExecution,
		"ended_through": endedThrough, "client_pid": 0, "client_pid_start_ms": 0,
	}
	if processBound {
		request["client_pid"] = anc.PID
		request["client_pid_start_ms"] = anc.StartMS
	}
	b := Bound{SessionID: session, Rung: rung}
	if err := r.Hub.Do(ctx, "POST", "/v1/sessions/succeed", request, &b); err != nil {
		if IsCode(err, "lost") {
			winner, adoptErr := r.adoptSuccessionWinnerHeld(ctx, session, rung, priorGeneration, priorExecution, anc, processBound)
			if adoptErr != nil {
				return Bound{}, true, &LostError{Cause: adoptErr}
			}
			if winner.ExecutionID != "" {
				return winner, true, nil
			}
		}
		return Bound{}, true, &LostError{Cause: err}
	}
	if b.ExecutionID == "" {
		return Bound{}, true, &LostError{Cause: hubclient.ErrNoExecution}
	}
	if b.SessionID != session || b.Generation != priorGeneration+1 {
		return Bound{}, true, &LostError{Cause: errGenerationFloor}
	}
	if err := r.recheckSuccessionWitness(session, anc); err != nil {
		return Bound{}, true, &LostError{Cause: err}
	}
	current, advanced, err := r.Succession.AdvanceGeneration(r.Client, session, priorExecution, priorGeneration, b.Generation, b.ExecutionID)
	if err != nil {
		return Bound{}, true, &LostError{Cause: err}
	}
	if current != b.Generation {
		winner, adoptErr := r.adoptSuccessionWinnerHeld(ctx, session, rung, priorGeneration, priorExecution, anc, processBound)
		if adoptErr != nil {
			return Bound{}, true, &LostError{Cause: adoptErr}
		}
		if winner.ExecutionID != "" {
			return winner, true, nil
		}
		return Bound{}, true, &LostError{Cause: errGenerationFloor}
	}
	if !advanced {
		if err := r.verifySuccessionWinnerHeld(b, anc); err != nil {
			return Bound{}, true, &LostError{Cause: err}
		}
	}
	if err := r.checkGenerationHeld(b); err != nil {
		return Bound{}, true, err
	}
	if advanced {
		if err := r.recheckSuccessionWitness(session, anc); err != nil {
			return Bound{}, true, &LostError{Cause: err}
		}
	}
	return b, true, nil
}

// adoptSuccessionWinnerHeld performs one read-only lookup and accepts only a winner confirmed by
// both the hub and the local proof/witness state. Lookup failure leaves the original lost in place.
func (r *Resolver) adoptSuccessionWinnerHeld(ctx context.Context, session string, rung, priorGeneration int, priorExecution string, anc proc.Ancestor, processBound bool) (Bound, error) {
	req := map[string]any{"client": r.Client, "host": r.Host, "cwd": r.CWD,
		"prior_generation": priorGeneration, "prior_execution_id": priorExecution}
	if processBound {
		req["client_pid"], req["client_pid_start_ms"] = anc.PID, anc.StartMS
		rung = 3
	} else {
		req["session_id"] = session
	}
	var reply struct {
		Bound
		Match string `json:"match"`
	}
	err := r.Hub.Do(ctx, "POST", "/v1/bind", req, &reply)
	b := reply.Bound
	b.Rung = rung
	if err != nil || reply.Match != "succession" || r.gone[b.ExecutionID] || b.SessionID != "" && b.SessionID != session ||
		b.Generation < priorGeneration || b.Generation == priorGeneration && b.ExecutionID != priorExecution {
		return Bound{}, nil
	}
	if b.SessionID == "" {
		b.SessionID = session
	}
	if b.Generation == priorGeneration+1 {
		if err := r.recheckSuccessionWitness(session, anc); err != nil {
			return Bound{}, err
		}
		if _, _, err := r.Succession.AdvanceGeneration(r.Client, session, priorExecution, priorGeneration, b.Generation, b.ExecutionID); err != nil {
			return Bound{}, err
		}
	}
	if err := r.verifySuccessionWinnerHeld(b, anc); err != nil {
		return Bound{}, err
	}
	if err := r.checkGenerationHeld(b); err != nil {
		return Bound{}, err
	}
	return b, nil
}

// recheckSuccessionWitness covers a SessionStart that ran while the hub request was in flight.
func (r *Resolver) recheckSuccessionWitness(session string, anc proc.Ancestor) error {
	// Every verified process-bound launcher has a witness except top-level Claude, which
	// can bind by process but has no per-process current-session record.
	if anc.PID == 0 || r.Client == "claude" {
		return nil
	}
	current, ok, err := r.Succession.CurrentSession(r.Client, anc)
	if err != nil {
		return err
	}
	if !ok || current != session {
		return errGenerationFloor
	}
	return nil
}

func (r *Resolver) verifySuccessionWinnerHeld(b Bound, anc proc.Ancestor) error {
	generation, execution, err := r.Succession.PrepareSuccession(r.Client, b.SessionID, b.Generation, b.ExecutionID, anc)
	if err != nil {
		return err
	}
	if generation != b.Generation || execution != b.ExecutionID {
		return errGenerationFloor
	}
	return nil
}
