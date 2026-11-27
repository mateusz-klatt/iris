package hook

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/sessionproof"
)

var (
	// errLocalProofDamaged stops registration cleanly without a .failures error when
	// local proof damage already recorded DamagedGenerationProof under succession ON.
	errLocalProofDamaged = errors.New("local session proof is damaged")
	// errDamagedAfterEnd records a non-degraded failure (Doctor silent) alongside the
	// damage observation when a confirmed .ended marker coexists with damaged proof.
	errDamagedAfterEnd = errors.New("damaged session proof after confirmed end")
	// errUnusableClassification records a non-degraded failure (Doctor silent) when an
	// opted hook receives an unclassified, incomplete, or conflicting session reply.
	errUnusableClassification = errors.New("hub reply is unusable for session succession")
)

type registrationAttempt struct {
	state             *runState
	ev                Event
	mem               Memory
	files             fileMemory
	hasFiles          bool
	client            string
	session           string
	stage             string
	successionEnabled bool

	cleanedExpired bool
	confirmedEnd   bool
	damaged        bool
	vetoed         bool
	retryUsed      bool
	genFloor       int
	endedFloor     int
	sentGen        int

	admitted       bool
	published      bool
	executionID    string
	admittedGen    int
	candidate      hubclient.Candidate
	classification hubclient.Classification
}

func lockDeadlineFromCtx(ctx context.Context) time.Time {
	d, _ := ctx.Deadline()
	return d
}

func endedMarkerPath(prefix string) string { return prefix + ".ended" }

func (r Runner) newRegistrationAttempt(ctx context.Context, state *runState, ev Event, stage string) *registrationAttempt {
	m := r.memory()
	a := &registrationAttempt{
		state:             state,
		ev:                ev,
		mem:               m,
		client:            string(ev.Client),
		session:           ev.SessionID,
		stage:             stage,
		successionEnabled: r.SuccessionEnabled != nil && r.SuccessionEnabled(ctx),
	}
	if files, ok := m.(fileMemory); ok {
		a.files = files
		a.hasFiles = true
	}
	return a
}

func (a *registrationAttempt) recordLocalVeto() {
	a.vetoed = true
	a.state.setLocalVeto()
}

func (a *registrationAttempt) damagedProofPath(snap sessionproof.Snapshot, prefix string) string {
	if a.successionEnabled && snap.RecordState == sessionproof.FileDamaged {
		return executionPath(prefix)
	}
	if snap.GenerationState == sessionproof.FileDamaged {
		return prefix
	}
	return ""
}

func (a *registrationAttempt) preInspect(ctx context.Context, retained int) (generation, endedThrough int, err error) {
	if a.mem == nil {
		return retained, a.endedFloor, nil
	}
	if !a.hasFiles {
		generation, err = a.mem.RecallGeneration(a.client, a.session)
		if err != nil {
			a.recordLocalVeto()
			return 0, 0, refuseRestoreFloor(a.state, a.ev, a.mem, retained, RestoreFloorRefused)
		}
		if generation > a.genFloor {
			a.genFloor = generation
		}
		return max(retained, generation), 0, nil
	}

	prefix := a.files.name(a.client, a.session)
	endedPath := endedMarkerPath(prefix)
	cleanExpired := !a.cleanedExpired
	a.cleanedExpired = true

	// Per §1 exception 3a, a fresh SessionStart or PreToolUse still inspects <hash> and .ended
	// under one sessions lock when session_succession is OFF using acquireOnly (v0.1.15 acquisition
	// budget). Other OFF events without local files keep skipping the lock when both are absent.
	if !a.successionEnabled && a.ev.Kind != KindSessionStart && a.ev.Kind != KindPreTool {
		if _, err := lstatSessionFile(prefix); sessionPathAbsent(prefix, err) {
			if _, endErr := lstatSessionFile(endedPath); sessionPathAbsent(endedPath, endErr) {
				return max(retained, a.genFloor), a.endedFloor, nil
			}
		}
	}

	a.state.enterMandatory(a.stage)
	snap, inspectErr := a.files.inspectEvidenceWithTouch(a.client, a.session, lockDeadlineFromCtx(ctx), a.successionEnabled, cleanExpired, true)
	if inspectErr != nil {
		if snap.EndedState == sessionproof.FileValid {
			a.endedFloor = max(a.endedFloor, snap.Ended)
			a.confirmedEnd = true
		}
		a.recordLocalVeto()
		a.state.failMandatory(inspectErr)
		a.state.leaveMandatory()
		return 0, a.endedFloor, inspectErr
	}
	a.state.leaveMandatory()

	switch snap.EndedState {
	case sessionproof.FileDamaged:
		a.state.observe(damagedGenerationObservation(endedPath))
		a.damaged = true
		a.recordLocalVeto()
		return 0, 0, errDamagedEndMarker
	case sessionproof.FileValid:
		a.endedFloor = max(a.endedFloor, snap.Ended)
		a.confirmedEnd = true
	}

	a.genFloor = max(a.genFloor, snap.Generation)
	if snap.RecordState == sessionproof.FileValid {
		a.genFloor = max(a.genFloor, snap.Record.Generation)
	}

	if damagedPath := a.damagedProofPath(snap, prefix); damagedPath != "" {
		note := damagedGenerationObservation(damagedPath)
		a.damaged = true
		if a.confirmedEnd {
			a.state.observe(note)
			a.recordLocalVeto()
			a.state.failMandatory(errDamagedAfterEnd)
			return 0, a.endedFloor, errDamagedAfterEnd
		}
		if !a.successionEnabled {
			if retained > 0 {
				a.recordLocalVeto()
			}
			return 0, 0, refuseRestoreFloor(a.state, a.ev, a.mem, retained, note)
		}
		a.state.observe(note)
		a.recordLocalVeto()
		return 0, a.endedFloor, errLocalProofDamaged
	}

	candidateGen := snap.GenerationFloor()
	ended := a.endedFloor > 0 && ((snap.GenerationState == sessionproof.FileValid && snap.Generation <= a.endedFloor) ||
		(retained > 0 && retained <= a.endedFloor) ||
		(snap.RecordState == sessionproof.FileValid && recordEndedAt(snap.Record, a.endedFloor)))
	if ended {
		if retained == 0 && a.ev.Kind == KindSessionStart {
			return 0, a.endedFloor, nil
		}
		if !a.successionEnabled && retained > 0 {
			return max(retained, candidateGen), a.endedFloor, nil
		}
		a.state.observe(endedProof(endedPath).Error())
		a.recordLocalVeto()
		return 0, a.endedFloor, errEndedGeneration
	}

	return max(candidateGen, retained), a.endedFloor, nil
}

func (a *registrationAttempt) recheckEndedOnHubError(ctx context.Context, proofless bool, err error) error {
	if !a.hasFiles {
		return err
	}
	prefix := a.files.name(a.client, a.session)
	endedPath := endedMarkerPath(prefix)
	if !a.successionEnabled {
		ended, expired, rerr := a.files.readEndMarker(endedPath)
		if rerr != nil {
			a.state.observe(damagedGenerationObservation(endedPath))
			a.damaged = true
			a.recordLocalVeto()
			return errDamagedEndMarker
		}
		if !expired && ended > 0 {
			a.endedFloor = max(a.endedFloor, ended)
			a.confirmedEnd = true
		}
		var hubErr *hubclient.Error
		if proofless && a.endedFloor > 0 && errors.As(err, &hubErr) && hubErr.Code == "lost" && hubErr.IsEndedThroughRefusal(a.endedFloor) {
			a.state.observe(endedProof(endedPath).Error())
			a.recordLocalVeto()
			return errEndedGeneration
		}
		return err
	}

	a.state.enterMandatory(a.stage)
	snap, inspectErr := a.files.inspectEvidenceDeadline(a.client, a.session, lockDeadlineFromCtx(ctx), true, false)
	if inspectErr != nil {
		if snap.EndedState == sessionproof.FileValid {
			a.endedFloor = max(a.endedFloor, snap.Ended)
			a.confirmedEnd = true
		}
		a.recordLocalVeto()
		a.state.failMandatory(inspectErr)
		a.state.leaveMandatory()
		return inspectErr
	}
	a.state.leaveMandatory()

	switch snap.EndedState {
	case sessionproof.FileDamaged:
		a.state.observe(damagedGenerationObservation(endedPath))
		a.damaged = true
		a.recordLocalVeto()
		return errDamagedEndMarker
	case sessionproof.FileValid:
		a.endedFloor = max(a.endedFloor, snap.Ended)
		a.confirmedEnd = true
	}

	a.genFloor = max(a.genFloor, snap.GenerationFloor())

	if damagedPath := a.damagedProofPath(snap, prefix); damagedPath != "" {
		a.state.observe(damagedGenerationObservation(damagedPath))
		a.damaged = true
		a.recordLocalVeto()
		if a.confirmedEnd {
			a.state.failMandatory(errDamagedAfterEnd)
			return errDamagedAfterEnd
		}
		return errLocalProofDamaged
	}

	var hubErr *hubclient.Error
	if proofless && a.endedFloor > 0 && errors.As(err, &hubErr) && hubErr.Code == "lost" && hubErr.IsEndedThroughRefusal(a.endedFloor) {
		a.state.observe(endedProof(endedPath).Error())
		a.recordLocalVeto()
		return errEndedGeneration
	}
	return err
}

// admitReply validates the registration response against stage 1b classification rules
// and monotonic invocation floors without writing any local evidence files.
func (a *registrationAttempt) admitReply(ctx context.Context, sess hubclient.SessionResponse, err error, proofless, allowRetry bool) (retry bool, outErr error) {
	if err != nil {
		checkedErr := a.recheckEndedOnHubError(ctx, proofless, err)
		if allowRetry && !a.vetoed && !a.retryUsed && hubclient.IsCode(checkedErr, "lost") && a.sentGen > 0 && a.ev.Kind != KindSessionEnd {
			return true, checkedErr
		}
		return false, checkedErr
	}

	if hubclient.SuccessionProofCapable(sess.Capabilities) {
		cand := sess.Candidate()
		cls, classErr := cand.Classify()
		if classErr != nil {
			a.recordLocalVeto()
			outErr = fmt.Errorf("%w: %v", errUnusableClassification, classErr)
			a.state.failMandatory(outErr)
			return false, outErr
		}
		if cls.Kind == hubclient.CandidateShimSuccession {
			if !cls.HookSeen || sess.Generation < 2 || sessionproof.ValidateExecutionID(sess.ExecutionID) != nil ||
				!cand.PriorExecutionID.Valid || sessionproof.ValidateExecutionID(cand.PriorExecutionID.Value) != nil {
				a.recordLocalVeto()
				a.state.failMandatory(errUnusableClassification)
				return false, errUnusableClassification
			}
		} else {
			rec := sessionproof.Record{
				Version:     sessionproof.FormatVersion,
				Origin:      sessionproof.OriginHook,
				Generation:  sess.Generation,
				ExecutionID: sess.ExecutionID,
			}
			if recErr := rec.Validate(); recErr != nil {
				a.recordLocalVeto()
				outErr = fmt.Errorf("%w: %v", errUnusableClassification, recErr)
				a.state.failMandatory(outErr)
				return false, outErr
			}
		}
		a.candidate = cand
		a.classification = cls
	}

	if a.endedFloor > 0 && sess.Generation <= a.endedFloor {
		if a.hasFiles {
			a.state.observe(endedProof(endedMarkerPath(a.files.name(a.client, a.session))).Error())
		}
		a.recordLocalVeto()
		return false, errEndedGeneration
	}
	if a.genFloor > 0 && sess.Generation < a.genFloor {
		return false, refuseRestoreFloor(a.state, a.ev, a.mem, a.genFloor, RestoreFloorRefused)
	}

	a.admitted = true
	a.published = false
	a.executionID = sess.ExecutionID
	a.admittedGen = sess.Generation
	return false, nil
}

// finalAdmitAndPublish performs the final locked admission recheck immediately before a
// dependent operation and only then publishes exact execution and generation evidence.
func (r Runner) finalAdmitAndPublish(ctx context.Context, a *registrationAttempt) (bool, error) {
	if a.mem == nil {
		a.published = true
		return true, nil
	}
	callerDeadline := lockDeadlineFromCtx(ctx)
	if !a.hasFiles {
		a.mem.Remember(a.client, a.session, a.admittedGen)
		a.published = true
		return true, nil
	}

	if !a.successionEnabled {
		if err := a.finalInspectOFF(callerDeadline); err != nil {
			return false, err
		}
		a.files.rememberWithSuccession(a.client, a.session, a.admittedGen, callerDeadline, false)
		a.published = true
		return true, nil
	}

	return a.finalInspectAndPublishON(callerDeadline)
}

func (a *registrationAttempt) finalInspectOFF(callerDeadline time.Time) error {
	prefix := a.files.name(a.client, a.session)
	endedPath := endedMarkerPath(prefix)
	ended, expired, endErr := a.files.readEndMarker(endedPath)
	if endErr != nil {
		a.state.observe(damagedGenerationObservation(endedPath))
		a.damaged = true
		a.recordLocalVeto()
		return errDamagedEndMarker
	}
	if !expired && ended > 0 {
		a.endedFloor = max(a.endedFloor, ended)
		a.confirmedEnd = true
	}

	gen, genErr := readGenerationDeadline(prefix, a.files.monoClock(), callerDeadline)
	if errors.Is(genErr, ErrInvalidGeneration) {
		if a.confirmedEnd {
			a.state.observe(damagedGenerationObservation(prefix))
			a.damaged = true
			a.recordLocalVeto()
			a.state.failMandatory(errDamagedAfterEnd)
			return errDamagedAfterEnd
		}
		a.damaged = true
		a.recordLocalVeto()
		return refuseRestoreFloor(a.state, a.ev, a.mem, 0, damagedGenerationObservation(prefix))
	}
	if a.endedFloor > 0 && a.admittedGen <= a.endedFloor {
		a.state.observe(endedProof(endedPath).Error())
		a.recordLocalVeto()
		return errEndedGeneration
	}
	a.genFloor = max(a.genFloor, gen)
	if a.admittedGen < a.genFloor {
		return refuseRestoreFloor(a.state, a.ev, a.mem, a.genFloor, RestoreFloorRefused)
	}
	return nil
}

func (a *registrationAttempt) finalInspectAndPublishON(callerDeadline time.Time) (bool, error) {
	a.state.enterMandatory(a.stage)
	mode := a.files.fullHold(callerDeadline)
	unlock, lockErr := a.files.lockAcquire(mode)
	if lockErr != nil {
		a.recordLocalVeto()
		a.state.failMandatory(lockErr)
		a.state.leaveMandatory()
		return false, lockErr
	}
	defer unlock()

	mono := a.files.monoClock()
	holdDeadline := mode.holdDeadline
	prefix := a.files.name(a.client, a.session)
	endedPath := endedMarkerPath(prefix)
	execPath := executionPath(prefix)

	ended, expired, endErr := a.files.readEndMarkerDeadline(endedPath, holdDeadline)
	var endedState sessionproof.FileState = sessionproof.FileAbsent
	switch {
	case errors.Is(endErr, ErrInvalidGeneration):
		endedState = sessionproof.FileDamaged
	case endErr != nil:
		a.recordLocalVeto()
		a.state.failMandatory(endErr)
		a.state.leaveMandatory()
		return false, endErr
	case !expired && ended > 0:
		endedState = sessionproof.FileValid
		a.endedFloor = max(a.endedFloor, ended)
		a.confirmedEnd = true
	}

	gen, genErr := readGenerationDeadline(prefix, mono, holdDeadline)
	var genState sessionproof.FileState = sessionproof.FileAbsent
	switch {
	case errors.Is(genErr, ErrInvalidGeneration):
		genState = sessionproof.FileDamaged
	case errors.Is(genErr, os.ErrDeadlineExceeded):
		a.recordLocalVeto()
		a.state.failMandatory(genErr)
		a.state.leaveMandatory()
		return false, genErr
	case genErr == nil && gen > 0:
		genState = sessionproof.FileValid
	}

	rec, recState, recErr := readExactRecordDeadline(execPath, mono, holdDeadline)
	if errors.Is(recErr, os.ErrDeadlineExceeded) {
		a.recordLocalVeto()
		a.state.failMandatory(recErr)
		a.state.leaveMandatory()
		return false, recErr
	}

	a.state.leaveMandatory()

	if endedState == sessionproof.FileDamaged {
		a.state.observe(damagedGenerationObservation(endedPath))
		a.damaged = true
		a.recordLocalVeto()
		return false, errDamagedEndMarker
	}

	snap := sessionproof.Snapshot{
		BasePath:        prefix,
		Generation:      gen,
		GenerationState: genState,
		Record:          rec,
		RecordState:     recState,
		Ended:           ended,
		EndedState:      endedState,
	}
	if damagedPath := a.damagedProofPath(snap, prefix); damagedPath != "" {
		a.state.observe(damagedGenerationObservation(damagedPath))
		a.damaged = true
		a.recordLocalVeto()
		if a.confirmedEnd {
			a.state.failMandatory(errDamagedAfterEnd)
			return false, errDamagedAfterEnd
		}
		return false, errLocalProofDamaged
	}

	a.genFloor = max(a.genFloor, snap.GenerationFloor())
	if a.endedFloor > 0 && a.admittedGen <= a.endedFloor {
		a.state.observe(endedProof(endedPath).Error())
		a.recordLocalVeto()
		return false, errEndedGeneration
	}

	if a.classification.Kind == hubclient.CandidateShimSuccession {
		if recState != sessionproof.FileValid || rec.Origin != sessionproof.OriginShim ||
			rec.Generation != a.admittedGen || rec.ExecutionID != a.executionID ||
			rec.PriorExecutionID != a.candidate.PriorExecutionID.Value {
			a.recordLocalVeto()
			a.state.observe(BindingGenerationRefused + " at " + execPath)
			a.state.failMandatory(errUnusableClassification)
			return false, errUnusableClassification
		}
		if a.endedFloor > 0 && recordEndedAt(rec, a.endedFloor) {
			a.state.observe(endedProof(endedPath).Error())
			a.recordLocalVeto()
			return false, errEndedGeneration
		}
	} else if recState == sessionproof.FileValid && (rec.Generation > a.admittedGen || (rec.Generation == a.admittedGen && rec.ExecutionID != a.executionID)) {
		a.recordLocalVeto()
		a.state.observe(BindingGenerationRefused + " at " + execPath)
		a.state.failMandatory(errUnusableClassification)
		return false, errUnusableClassification
	}

	if a.admittedGen < a.genFloor {
		a.recordLocalVeto()
		return false, refuseRestoreFloor(a.state, a.ev, a.mem, a.genFloor, RestoreFloorRefused)
	}

	// Mandatory admission checks are complete. Optional evidence writes may fail
	// without blocking dependent tool/session actions or grading silent.
	if a.classification.Kind != "" && a.classification.Kind != hubclient.CandidateShimSuccession {
		wantExec := sessionproof.Record{
			Version:     sessionproof.FormatVersion,
			Origin:      sessionproof.OriginHook,
			Generation:  a.admittedGen,
			ExecutionID: a.executionID,
		}
		if recState != sessionproof.FileValid || rec != wantExec {
			if body, err := sessionproof.EncodeRecord(wantExec); err == nil {
				if err := writeEvidenceFile(execPath, string(body), holdDeadline, a.files); err != nil {
					a.state.observe(SessionEvidenceWriteFailed + " at " + execPath)
				}
			}
		}
	}
	if genState != sessionproof.FileValid || gen != a.admittedGen {
		if err := writeEvidenceFile(prefix, strconv.Itoa(a.admittedGen), holdDeadline, a.files); err != nil {
			a.state.observe(SessionEvidenceWriteFailed + " at " + prefix)
		}
	}
	a.published = true
	return true, nil
}

func writeEvidenceFile(path, body string, deadline time.Time, files fileMemory) error {
	tmp, err := prepareSessionFile(files.dir, body, files.restrict)
	if err != nil {
		return err
	}
	if err := replaceSessionFileDeadline(tmp, path, deadline, false, files.monoClock(), files.pause); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
