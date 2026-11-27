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

	cleanedExpired   bool
	hadDamagedBefore bool
	confirmedEnd     bool
	damaged          bool
	vetoed           bool
	retryUsed        bool
	genFloor         int
	endedFloor       int
	sentGen          int

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

func (r Runner) resolveSuccession(ctx context.Context, state *runState) bool {
	if enabled, ok := state.successionMode(); ok {
		return enabled
	}
	enabled := r.SuccessionEnabled != nil && r.SuccessionEnabled(ctx)
	state.setSuccessionMode(enabled)
	return enabled
}

func (r Runner) newRegistrationAttempt(ctx context.Context, state *runState, ev Event, stage string) *registrationAttempt {
	m := r.memory()
	a := &registrationAttempt{
		state:             state,
		ev:                ev,
		mem:               m,
		client:            string(ev.Client),
		session:           ev.SessionID,
		stage:             stage,
		successionEnabled: r.resolveSuccession(ctx, state),
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

func readEndMarkerAndCleanOFF(files fileMemory, prefix, endedPath string, deadline time.Time) (ended int, damaged, confirmed bool) {
	val, expired, err := files.readEndMarker(endedPath)
	if err != nil {
		return 0, true, false
	}
	if expired {
		_, _ = files.cleanupExpiredEnd(prefix, val, deadline, false)
		return 0, false, false
	}
	return val, false, val > 0
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

	if !a.successionEnabled {
		return a.preInspectOFF(ctx, retained)
	}

	a.state.enterMandatory(a.stage)
	cleanExpired := !a.cleanedExpired
	a.cleanedExpired = true
	snap, inspectErr := a.files.inspectEvidenceDeadline(a.client, a.session, lockDeadlineFromCtx(ctx), true, cleanExpired)
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

	prefix := a.files.name(a.client, a.session)
	endedPath := endedMarkerPath(prefix)
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
		a.state.observe(damagedGenerationObservation(damagedPath))
		a.damaged = true
		a.recordLocalVeto()
		if a.confirmedEnd {
			a.state.failMandatory(errDamagedAfterEnd)
			return 0, a.endedFloor, errDamagedAfterEnd
		}
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
		a.state.observe(endedProof(endedPath).Error())
		a.recordLocalVeto()
		return 0, a.endedFloor, errEndedGeneration
	}

	return max(candidateGen, retained), a.endedFloor, nil
}

func (a *registrationAttempt) preInspectOFF(ctx context.Context, retained int) (generation, endedThrough int, err error) {
	prefix := a.files.name(a.client, a.session)
	endedPath := endedMarkerPath(prefix)
	cleanExpired := !a.cleanedExpired
	a.cleanedExpired = true

	var endDamaged bool
	var confirmed bool
	if cleanExpired {
		endedThrough, endDamaged, confirmed = readEndMarkerAndCleanOFF(a.files, prefix, endedPath, lockDeadlineFromCtx(ctx))
	} else {
		val, expired, rerr := a.files.readEndMarker(endedPath)
		if rerr != nil {
			endDamaged = true
		} else if !expired && val > 0 {
			endedThrough, confirmed = val, true
		}
	}
	if endDamaged {
		a.state.observe(damagedGenerationObservation(endedPath))
		a.damaged = true
		a.recordLocalVeto()
		return 0, 0, errDamagedEndMarker
	}
	if confirmed {
		a.endedFloor = max(a.endedFloor, endedThrough)
		a.confirmedEnd = true
	}

	generation, err = a.mem.RecallGeneration(a.client, a.session)
	var damaged damagedEndMarker
	if errors.As(err, &damaged) {
		a.state.observe(damaged.Error())
		a.damaged = true
		a.recordLocalVeto()
		return 0, 0, errDamagedEndMarker
	}
	var ended endedProof
	if errors.As(err, &ended) {
		if retained == 0 {
			if a.ev.Kind == KindSessionStart {
				return 0, a.endedFloor, nil
			}
			a.state.observe(ended.Error())
			a.recordLocalVeto()
			return 0, 0, errEndedGeneration
		}
		return max(retained, generation), a.endedFloor, nil
	}
	if err != nil {
		if a.confirmedEnd && errors.Is(err, ErrInvalidGeneration) {
			a.state.observe(damagedGenerationObservation(prefix))
			a.damaged = true
			a.recordLocalVeto()
			a.state.failMandatory(errDamagedAfterEnd)
			return 0, a.endedFloor, errDamagedAfterEnd
		}
		note := RestoreFloorRefused
		if errors.Is(err, ErrInvalidGeneration) {
			note = damagedGenerationObservation(prefix)
			a.hadDamagedBefore = true
		}
		if retained > 0 {
			a.recordLocalVeto()
		}
		return 0, 0, refuseRestoreFloor(a.state, a.ev, a.mem, retained, note)
	}
	if generation > a.genFloor {
		a.genFloor = generation
	}
	return max(retained, generation), a.endedFloor, nil
}

func (a *registrationAttempt) recheckEndedOnHubError(ctx context.Context, generation int, proofless bool, err error) error {
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
		strictFloorRefusal := proofless && a.endedFloor > 0 && errors.As(err, &hubErr) && hubErr.Code == "lost" && hubErr.IsEndedThroughRefusal(a.endedFloor)
		if a.endedFloor > 0 && (err == nil && generation <= a.endedFloor || strictFloorRefusal) {
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
	strictFloorRefusal := proofless && a.endedFloor > 0 && errors.As(err, &hubErr) && hubErr.Code == "lost" && hubErr.IsEndedThroughRefusal(a.endedFloor)
	if strictFloorRefusal || (a.endedFloor > 0 && err == nil && generation <= a.endedFloor) {
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
		checkedErr := a.recheckEndedOnHubError(ctx, sess.Generation, proofless, err)
		if allowRetry && !a.vetoed && !a.retryUsed && hubclient.IsCode(checkedErr, "lost") && a.sentGen > 0 && a.ev.Kind != KindSessionEnd {
			return true, checkedErr
		}
		return false, checkedErr
	}

	if a.successionEnabled {
		cand := sess.Candidate()
		cls, classErr := cand.Classify()
		if classErr != nil {
			a.recordLocalVeto()
			outErr = fmt.Errorf("%w: %v", errUnusableClassification, classErr)
			a.state.failMandatory(outErr)
			return false, outErr
		}
		if cls.Kind == hubclient.CandidateUnclassified {
			a.recordLocalVeto()
			a.state.failMandatory(errUnusableClassification)
			return false, errUnusableClassification
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
	if !a.admitted || a.vetoed {
		return false, errRestoreFloor
	}
	if a.mem == nil {
		a.published = true
		return true, nil
	}
	callerDeadline := lockDeadlineFromCtx(ctx)
	if !a.hasFiles {
		rememberWithDeadline(a.mem, a.client, a.session, a.admittedGen, callerDeadline)
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
		if !a.hadDamagedBefore {
			a.damaged = true
			a.recordLocalVeto()
			return refuseRestoreFloor(a.state, a.ev, a.mem, 0, damagedGenerationObservation(prefix))
		}
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
	if a.classification.Kind != hubclient.CandidateShimSuccession {
		wantExec := sessionproof.Record{
			Version:     sessionproof.FormatVersion,
			Origin:      sessionproof.OriginHook,
			Generation:  a.admittedGen,
			ExecutionID: a.executionID,
		}
		if recState != sessionproof.FileValid || rec != wantExec {
			if body, err := sessionproof.EncodeRecord(wantExec); err != nil {
				a.state.observe(SessionEvidenceWriteFailed + " at " + execPath)
			} else if err := writeEvidenceFile(execPath, string(body), holdDeadline, a.files); err != nil {
				a.state.observe(SessionEvidenceWriteFailed + " at " + execPath)
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
