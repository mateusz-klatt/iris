package hook

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/mateusz-klatt/iris/internal/config"
	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/sessionproof"
)

// Memory keeps the generation a hook last saw for a client session. After a hub restart a
// session's Execution waits for proof that it is the same session (D9), and a hook is a new
// process for every tool call, so without this the first edits of a recovering session had
// none to give.
type Memory interface {
	// RecallGeneration returns zero with no error only when proof is absent.
	RecallGeneration(client, session string) (int, error)
	Remember(client, session string, generation int)
	Forget(client, session string, generation int)
}

// memoryAge is how long an untouched entry survives, including the marker left when a
// session ends normally.
const memoryAge = 30 * time.Hour * 24

type endedProof string

func (p endedProof) Error() string { return EndedGenerationRefused + " at " + string(p) }

type damagedEndMarker string

func (p damagedEndMarker) Error() string { return damagedGenerationObservation(string(p)) }

// fileMemory stores one small file per session beside the token, protected as the token is:
// mode 0600 in a 0700 directory, and on Windows the token's owner-only DACL. So anyone who
// can read it can read the token, which already grants everything the generation would.
type fileMemory struct {
	dir      string
	now      func() time.Time
	mono     func() time.Time
	restrict func(string) error
	pause    func(time.Duration)
}

func (m fileMemory) clock() func() time.Time {
	if m.now != nil {
		return m.now
	}
	return time.Now
}

func (m fileMemory) monoClock() func() time.Time {
	if m.mono != nil {
		return m.mono
	}
	return time.Now
}

func (m fileMemory) sleep(d time.Duration) {
	if m.pause != nil {
		m.pause(d)
		return
	}
	time.Sleep(d)
}

func (m fileMemory) lockBudgetDeadline(callerDeadline time.Time) time.Time {
	limit := m.monoClock()().Add(hookLockBudget)
	if !callerDeadline.IsZero() && callerDeadline.Before(limit) {
		return callerDeadline
	}
	return limit
}

func (m fileMemory) name(client, session string) string {
	sum := sha256.Sum256([]byte(client + "\x00" + session))
	return filepath.Join(m.dir, hex.EncodeToString(sum[:16]))
}

// read returns a file's first line.
func (m fileMemory) read(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	first, _, _ := strings.Cut(string(raw), "\n")
	return strings.TrimSpace(first)
}

func readGeneration(path string) (int, error) {
	return readGenerationDeadline(path, nil, time.Time{})
}

func readGenerationDeadline(path string, now func() time.Time, deadline time.Time) (int, error) {
	raw, err := readRegularBoundedBytesDeadline(path, GenerationReadLimit, now, deadline)
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return 0, err
	}
	if errors.Is(err, os.ErrNotExist) {
		if deadlineExpired(now, deadline) {
			return 0, os.ErrDeadlineExceeded
		}
		// A dangling link occupies the generation path even if its target is missing.
		if _, lerr := lstatSessionFile(path); sessionPathAbsent(path, lerr) {
			return 0, nil
		}
	}
	if err != nil {
		return 0, ErrInvalidGeneration
	}
	generation, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || generation <= 0 {
		return 0, ErrInvalidGeneration
	}
	return generation, nil
}

// RecallGeneration keeps known proof fresh before a request can race idle pruning. Absent proof
// needs no lock, so unrelated session-file work cannot suppress a fresh SessionStart.
// Non-succession (v0.1.15) read: lock uses acquireOnly(hookLockBudget) with no hold deadline.
func (m fileMemory) RecallGeneration(client, session string) (int, error) {
	path := m.name(client, session)
	generation, readErr := readGeneration(path)
	if generation == 0 {
		return 0, readErr
	}
	if err := m.underLock(func() {
		var current int
		current, readErr = readGeneration(path)
		if readErr != nil {
			return
		}
		// Another hook can write newer proof before we take the lock. A prune can also
		// win the lock, in which case the proof we already read still belongs in the request.
		generation = max(generation, current)
		ended, err := readGeneration(path + ".ended")
		if err != nil {
			readErr = damagedEndMarker(path + ".ended")
			return
		}
		if generation <= ended {
			readErr = endedProof(path + ".ended")
			return
		}
		if current == generation {
			m.touchGeneration(path)
		}
	}); err != nil {
		return 0, err
	}
	return generation, readErr
}

// readEndMarker reads the marker and reports whether it has aged past local retention.
func (m fileMemory) readEndMarker(marker string) (generation int, expired bool, err error) {
	return m.readEndMarkerDeadline(marker, time.Time{})
}

func (m fileMemory) readEndMarkerDeadline(marker string, deadline time.Time) (generation int, expired bool, err error) {
	mono := m.monoClock()
	generation, err = readGenerationDeadline(marker, mono, deadline)
	if generation == 0 {
		return 0, false, err
	}
	if deadlineExpired(mono, deadline) {
		return 0, false, os.ErrDeadlineExceeded
	}
	info, statErr := lstatSessionFile(marker)
	return generation, statErr == nil && info.ModTime().Before(m.clock()().Add(-memoryAge)), nil
}

// Remember keeps newer live proof and ignores replies for a generation another hook ended.
// An accepted registration can replace an expired marker even if a sweep missed it.
func (m fileMemory) Remember(client, session string, generation int) {
	m.rememberWithSuccession(client, session, generation, time.Time{}, false)
}

func rememberWithDeadline(m Memory, client, session string, generation int, deadline time.Time) {
	if fm, ok := m.(fileMemory); ok {
		fm.rememberWithSuccession(client, session, generation, deadline, false)
		return
	}
	m.Remember(client, session, generation)
}

func (m fileMemory) rememberWithSuccession(client, session string, generation int, callerDeadline time.Time, successionEnabled bool) {
	if generation <= 0 {
		return
	}
	// M2 succession-evidence holds when session_succession is ON use ADR 039's fullHold deadline;
	// when OFF, keep v0.1.15's acquisition-only budget (no post-lock hold deadline).
	mode := m.lockModeForSuccession(callerDeadline, successionEnabled)
	holdDeadline := mode.holdDeadline
	mono := m.monoClock()
	if err := m.directory(); err != nil {
		return
	}
	tmp, err := prepareSessionFile(m.dir, strconv.Itoa(generation), m.restrict)
	if err != nil {
		return
	}
	defer func() {
		if tmp != "" {
			_ = os.Remove(tmp)
		}
	}()
	unlock, err := m.lockAcquire(mode)
	if err != nil {
		return
	}
	defer unlock()

	path := m.name(client, session)
	marker := path + ".ended"
	ended, expired, markerErr := m.readEndMarkerDeadline(marker, holdDeadline)
	if errors.Is(markerErr, os.ErrDeadlineExceeded) {
		return
	}
	current, err := readGenerationDeadline(path, mono, holdDeadline)
	if err != nil {
		return
	}
	// Under OFF (zero holdDeadline), optional=false preserves v0.1.15's fresh per-rename retry.
	// Under ON (non-zero holdDeadline), ADR 039 treats <hash> publication as optional (no retry sleep).
	requiredRename := holdDeadline.IsZero()
	switch {
	case markerErr == nil && expired && current <= ended:
		// Clear stale proof and exact evidence before their veto, and clear the expired veto
		// before publishing the accepted generation to lock-free binding readers.
		if cleaned, _ := m.cleanupExpiredEnd(path, ended, holdDeadline, successionEnabled); cleaned {
			if replaceSessionFileDeadline(tmp, path, holdDeadline, requiredRename, mono, m.pause) == nil {
				tmp = ""
			}
		}
	case generation > ended && generation >= current:
		if replaceSessionFileDeadline(tmp, path, holdDeadline, requiredRename, mono, m.pause) == nil {
			tmp = ""
		}
	}
}

// Forget removes proof at or below the generation that successfully ended.
func (m fileMemory) Forget(client, session string, generation int) {
	_ = m.forget(client, session, generation)
}

func (m fileMemory) forget(client, session string, generation int) string {
	return m.forgetEvidence(client, session, generation, time.Time{}, false)
}

func (m fileMemory) forgetEvidence(client, session string, generation int, callerDeadline time.Time, successionEnabled bool) string {
	if generation <= 0 {
		return ""
	}
	path := m.name(client, session)
	// The marker is the path doctor can name when the end is not recorded. A missed lock
	// never enters the callback, so the path has to be known out here.
	marker := path + ".ended"
	// M2 succession-evidence retirement when session_succession is ON uses ADR 039's fullHold
	// deadline; when OFF, keep v0.1.15's acquisition-only budget with no hold deadline.
	mode := m.lockModeForSuccession(callerDeadline, successionEnabled)
	holdDeadline := mode.holdDeadline
	mono := m.monoClock()
	if err := m.directory(); err != nil {
		return marker
	}
	tmp, err := prepareSessionFile(m.dir, strconv.Itoa(generation), m.restrict)
	if err != nil {
		return marker
	}
	defer func() {
		if tmp != "" {
			_ = os.Remove(tmp)
		}
	}()
	unlock, err := m.lockAcquire(mode)
	if err != nil {
		return marker
	}
	defer unlock()

	// Record the end first, even if pruning already removed the proof. The same lock
	// prevents a delayed refusal from treating a cleared file as merely pruned.
	ended, readErr := readGenerationDeadline(marker, mono, holdDeadline)
	if readErr != nil {
		return marker
	}
	floor := max(ended, generation)
	if floor != generation {
		if deadlineExpired(mono, holdDeadline) || writePreparedSessionFile(tmp, strconv.Itoa(floor)) != nil {
			return marker
		}
	}
	if replaceSessionFileDeadline(tmp, marker, holdDeadline, true, mono, m.pause) != nil {
		// The proof stays: deleting it would forget the generation with no record of the end.
		return marker
	}
	tmp = ""
	current, err := readGenerationDeadline(path, mono, holdDeadline)
	if err != nil {
		return ""
	}
	if current > 0 && current <= floor && !deadlineExpired(mono, holdDeadline) {
		_ = removeSessionFile(path)
	}
	execPath := executionPath(path)
	if !successionEnabled {
		if current <= floor {
			if _, lerr := lstatSessionFile(execPath); lerr == nil {
				_ = removeSessionFile(execPath)
			}
		}
		return ""
	}
	rec, state, recErr := readExactRecordDeadline(execPath, mono, holdDeadline)
	if recErr == nil && state == sessionproof.FileValid && recordEndedAt(rec, floor) && !deadlineExpired(mono, holdDeadline) {
		_ = removeSessionFile(execPath)
	}
	return ""
}

// endMarker is the .ended path a refusal should name. Path is set when the marker
// contributed a generation or could not be read. Damaged is set only when it could
// not be read. An absent marker leaves Path empty.
type endMarker struct {
	Path    string
	Damaged bool
}

// retainGeneration restores proof that pruning removed after it was read. Only an absent
// entry may be recreated, and a successful end rules out that generation and earlier ones.
// The marker is read even when proof is present, so a later refusal can name it; that read
// does not change whether proof is restored, touched or left alone.
// retainGeneration restores proof that pruning removed after it was read (v0.1.15 semantics:
// acquireOnly(hookLockBudget), no hold deadline). Only an absent proof is rewritten: a read
// failure stays damaged, and an older one yields to the newer session.
func (m fileMemory) retainGeneration(client, session string, generation int) endMarker {
	var marker endMarker
	_ = m.underLock(func() {
		path := m.name(client, session)
		endedPath := path + ".ended"
		current, proofErr := readGeneration(path)
		ended, markerErr := readGeneration(endedPath)
		switch {
		case markerErr != nil:
			marker = endMarker{Path: endedPath, Damaged: true}
		case proofErr == nil && ended > 0:
			marker = endMarker{Path: endedPath}
		}
		switch {
		case proofErr != nil:
			return
		case current == 0:
			if markerErr != nil {
				return
			}
			if generation > ended {
				_ = m.replace(path, strconv.Itoa(generation))
			}
		case current == generation:
			m.touchGeneration(path)
		}
	})
	return marker
}

func (m fileMemory) touchGeneration(path string) {
	now := m.clock()()
	_ = os.Chtimes(path, now, now)
}

// sessionsLock protects generation changes, every attention and wake file operation, and
// removals by pruning (ADR 017). Reads of attention and wake files hold it because on Windows
// the rename used here fails when another process holds the destination file open. It is created
// once and never pruned, renamed or replaced, so every process locks the same file.
const sessionsLock = ".lock"

// hookLockBudget is how long a hook tries for the lock (and bounds a full succession-evidence
// hold when session_succession is ON). Nobody holds it across a network call or while a client
// runs, or while the directory is protected; a hold is file reads and writes only.
const hookLockBudget = 50 * time.Millisecond

// lockRetry is the pause between two tries for the lock.
const lockRetry = 2 * time.Millisecond

// lockHoldMode separates v0.1.15's acquisition-only budget from ADR 039's full-hold monotonic
// deadline:
//   - acquireOnly (zero holdDeadline): attention/told writers, wake writers, prune, retainGeneration,
//     and all OFF-mode evidence operations bound only lock acquisition (starting at tryLockFile)
//     and hold no post-lock deadline.
//   - fullHold (non-zero holdDeadline): only M2 succession-evidence operations under ON share one
//     monotonic min(caller, now+50ms) deadline across acquisition, reads, replacements (including
//     Windows CreateFile/SetFileInformationByHandle/MoveFileEx step checks), and cleanup.
type lockHoldMode struct {
	acquireBudget time.Duration
	tryOnce       bool
	holdDeadline  time.Time
}

func acquireOnly(budget time.Duration) lockHoldMode {
	return lockHoldMode{acquireBudget: budget, tryOnce: budget <= 0}
}

func (m fileMemory) acquireOnlyWithCaller(callerDeadline time.Time) lockHoldMode {
	budget := hookLockBudget
	if !callerDeadline.IsZero() {
		rem := callerDeadline.Sub(m.monoClock()())
		if rem < budget {
			budget = max(rem, 0)
		}
	}
	return acquireOnly(budget)
}

func (m fileMemory) fullHold(callerDeadline time.Time) lockHoldMode {
	return lockHoldMode{holdDeadline: m.lockBudgetDeadline(callerDeadline)}
}

func (m fileMemory) lockModeForSuccession(callerDeadline time.Time, successionEnabled bool) lockHoldMode {
	if successionEnabled {
		return m.fullHold(callerDeadline)
	}
	return m.acquireOnlyWithCaller(callerDeadline)
}

// lock takes the sessions directory's lock within budget (acquireOnly) and returns its release.
// A lock held past the budget is failSessionsLock, and the caller then reads and writes nothing.
func (m fileMemory) lock(budget time.Duration) (func(), error) {
	return m.lockAcquire(acquireOnly(budget))
}

func (m fileMemory) lockDeadline(deadline time.Time) (func(), error) {
	return m.lockAcquire(lockHoldMode{holdDeadline: deadline})
}

func (m fileMemory) lockAcquire(mode lockHoldMode) (func(), error) {
	now := m.monoClock()
	fullHold := !mode.holdDeadline.IsZero()
	if fullHold && deadlineExpired(now, mode.holdDeadline) {
		return nil, failSessionsLock
	}
	// A caller that writes protects the directory before it takes the lock: that costs Windows
	// time in proportion to the files in it, about 80 ms for a thousand sessions' files, because
	// Windows applies the directory's inheritable protection to each. The lock file holds nothing,
	// so the lock protects the directory only where it has to make it.
	if _, err := os.Stat(m.dir); err != nil {
		if err := m.directory(); err != nil {
			return nil, err
		}
	}
	if fullHold && deadlineExpired(now, mode.holdDeadline) {
		return nil, failSessionsLock
	}
	path := filepath.Join(m.dir, sessionsLock)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if fullHold && deadlineExpired(now, mode.holdDeadline) {
		_ = f.Close()
		return nil, failSessionsLock
	}
	// A restrictive Unix umask can create this file without owner read/write access. Restore
	// those bits through the open descriptor before locking, so later hooks can reopen it.
	if err := protectSessionsLock(f); err != nil {
		_ = f.Close()
		return nil, err
	}
	acqDeadline := mode.holdDeadline
	if !fullHold {
		acqDeadline = now().Add(mode.acquireBudget)
	}
	for {
		if fullHold && deadlineExpired(now, acqDeadline) {
			_ = f.Close()
			return nil, failSessionsLock
		}
		if tryLockFile(f) == nil {
			break
		}
		if mode.tryOnce {
			_ = f.Close()
			return nil, failSessionsLock
		}
		rem := acqDeadline.Sub(now())
		if rem <= 0 {
			_ = f.Close()
			return nil, failSessionsLock
		}
		m.sleep(min(lockRetry, rem))
	}
	// An Iris from before ADR 017 on the same host prunes any file untouched for 30 days, and a
	// .lock it removed would leave two hooks locking two files. Each lock keeps the file young.
	ts := m.clock()()
	_ = os.Chtimes(path, ts, ts)
	return func() {
		unlockFile(f)
		_ = f.Close()
	}, nil
}

// Counts are a session's attention numbers: unread deliveries and freed paths.
type Counts struct{ Unread, Available int }

// toldState is a session's attention file: the counts the hub last reported and the counts the
// session was last told (gap review A4), and, from a hub that names attention (ADR 017, decision
// 4), the identity last told, when, whether it was said a second time, and a revision that every
// change of the told identity raises.
type toldState struct {
	Latest, Told Counts
	Stream       string // empty when no identity is told
	Cursor       int64
	At           int64 // Unix milliseconds
	Twice        bool
	Rev          int64
}

// parseTold reads the file's one line: four counts, as before ADR 017, then the told identity,
// whose stream is hex, so no stream the hub sends can split the line. A file from before has only
// the counts. Each field is parsed on its own, so damage to one does not invalidate the others.
func parseTold(line string) toldState {
	var n [9]int64
	var stream []byte
	for i, field := range strings.Fields(line) {
		if i == 4 {
			stream, _ = hex.DecodeString(field)
			continue
		}
		if i < len(n) {
			n[i], _ = strconv.ParseInt(field, 10, 64)
		}
	}
	return toldState{
		Latest: Counts{int(n[0]), int(n[1])}, Told: Counts{int(n[2]), int(n[3])},
		Stream: string(stream), Cursor: n[5], At: n[6], Twice: n[7] == 1, Rev: n[8],
	}
}

// line writes the state so that an iris from before ADR 017 still reads the counts first. No
// stream is "-", which is not hex.
func (t toldState) line() string {
	stream := hex.EncodeToString([]byte(t.Stream))
	if stream == "" {
		stream = "-"
	}
	twice := 0
	if t.Twice {
		twice = 1
	}
	return strings.Join([]string{
		strconv.Itoa(t.Latest.Unread), strconv.Itoa(t.Latest.Available), strconv.Itoa(t.Told.Unread), strconv.Itoa(t.Told.Available),
		stream, strconv.FormatInt(t.Cursor, 10), strconv.FormatInt(t.At, 10), strconv.Itoa(twice), strconv.FormatInt(t.Rev, 10),
	}, " ")
}

// attentionMemory keeps, per session, what the session was told of its attention (gap review A4,
// ADR 017), changed only under the sessions directory's lock.
type attentionMemory interface {
	// toldRevision reads the told state's revision under the lock. When the lock is not taken in
	// budget it gives fromReplay, which no told state has, with the error.
	toldRevision(client, session string) (int64, error)
	// updateAttention lets f change the session's told state under the lock, and writes it when f
	// returns true. It writes nothing when the lock is not taken in budget.
	updateAttention(client, session string, f func(*toldState) bool) error
	// forgetAttention removes the session's told state under the lock.
	forgetAttention(client, session string) error
}

// counts is the attention file beside a session's generation. It is separate, so a counts
// write can never race a reattach's generation write and roll the proof back (D9).
func (m fileMemory) counts(client, session string) string {
	return m.name(client, session) + ".attention"
}

// toldRevision uses acquireOnly(hookLockBudget) (v0.1.15 semantics, no post-lock hold deadline).
func (m fileMemory) toldRevision(client, session string) (int64, error) {
	rev := fromReplay
	err := m.underLock(func() {
		rev = parseTold(m.read(m.counts(client, session))).Rev
	})
	return rev, err
}

func (m fileMemory) underLock(f func()) error {
	return m.underLockMode(acquireOnly(hookLockBudget), f)
}

func (m fileMemory) underLockMode(mode lockHoldMode, f func()) error {
	unlock, err := m.lockAcquire(mode)
	if err != nil {
		return err
	}
	defer unlock()
	f()
	return nil
}

// updateAttention uses acquireOnly(hookLockBudget) (v0.1.15 semantics, no hold deadline).
func (m fileMemory) updateAttention(client, session string, f func(*toldState) bool) error {
	// The directory is protected before the lock: on Windows that costs time in proportion to the
	// files in it, and the lock is held only for a few small reads and writes.
	if err := m.directory(); err != nil {
		return err
	}
	unlock, err := m.lockAcquire(acquireOnly(hookLockBudget))
	if err != nil {
		return err
	}
	defer unlock()
	path := m.counts(client, session)
	t := parseTold(m.read(path))
	if !f(&t) {
		return nil
	}
	return m.replace(path, t.line())
}

// forgetAttention uses acquireOnly(hookLockBudget) (v0.1.15 semantics, no hold deadline).
func (m fileMemory) forgetAttention(client, session string) error {
	unlock, err := m.lockAcquire(acquireOnly(hookLockBudget))
	if err != nil {
		return err
	}
	defer unlock()
	_ = removeSessionFile(m.counts(client, session))
	return nil
}

// wakeMemory marks a session's wake consumed by the prompt hook of the wake's own turn (ADR 017,
// decision 3).
type wakeMemory interface {
	consumeWake(client, session, prompt string) error
}

// wakeVersion is the wake file's format. The hook leaves a file of another version as it is.
const wakeVersion = 1

// wakeDir holds the wake files, apart from the files pruning reads. Nothing removes a wake file,
// because its spent slots must outlast mail nobody fetches (ADR 017, decision 3), and no hook
// lists this directory. On Windows a write protects the sessions directory, and Windows applies
// that to every file below it that inherits it: beside 20,000 wake files a told-state write took
// 2.2 s when the wake directory inherited and 1.9 ms when it had protection of its own. The
// planned runner must therefore make this directory with protection of its own.
const wakeDir = "wake"

// wakePath is a session's wake file. The planned runner in iris mcp will write its attempts.
func (m fileMemory) wakePath(client, session string) string {
	return filepath.Join(m.dir, wakeDir, filepath.Base(m.name(client, session)))
}

// wakeDoc is a wake file as a hook sees it. The planned runner owns every field; a hook sets only
// an attempt's consumed_ms, drops a long-consumed attempt's token, message and answer, and keeps
// everything else exactly as it finds it.
type wakeDoc struct {
	fields   map[string]json.RawMessage
	attempts []map[string]json.RawMessage
}

// readWake reads a wake file of this version, or reports that there is none it can use.
func readWake(path string) (wakeDoc, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return wakeDoc{}, false
	}
	var d wakeDoc
	var version int
	if json.Unmarshal(raw, &d.fields) != nil || json.Unmarshal(d.fields["version"], &version) != nil || version != wakeVersion ||
		json.Unmarshal(d.fields["attempts"], &d.attempts) != nil {
		return wakeDoc{}, false
	}
	return d, true
}

// encode writes the document back. Every value came from Unmarshal or is a number, so Marshal
// cannot fail.
func (d wakeDoc) encode() string {
	d.fields["attempts"], _ = json.Marshal(d.attempts)
	body, _ := json.Marshal(d.fields)
	return string(body)
}

func attemptText(a map[string]json.RawMessage, key string) string {
	var s string
	_ = json.Unmarshal(a[key], &s)
	return s
}

func attemptTime(a map[string]json.RawMessage, key string) int64 {
	var n int64
	_ = json.Unmarshal(a[key], &n)
	return n
}

// awaits says whether prompt is the whole message of the newest attempt, byte for byte, and that
// attempt is not consumed yet. It checks only the newest, the only one that can be outstanding
// once the planned runner writes an attempt only when none is outstanding (ADR 017, decision 3).
func (d wakeDoc) awaits(prompt string) bool {
	if len(d.attempts) == 0 {
		return false
	}
	newest := d.attempts[len(d.attempts)-1]
	message := attemptText(newest, "message")
	return message != "" && message == prompt && attemptTime(newest, "consumed_ms") == 0
}

// consumeWake marks the newest attempt consumed when prompt is its whole message: the prompt
// hook of the wake's own turn. A prompt that differs from the whole message, even by one byte,
// matches nothing, and so does one that only contains the token. The match routes and does not
// authenticate: a person who typed the exact message would consume the wake too (ADR 017).
// Without a wake file it does nothing and takes no lock. Any other prompt takes the lock for one
// read of the wake file; only a match protects the directory, outside the lock, and then takes
// the lock again to read and write. With a wake file it cannot read, it does nothing.
// consumeWake marks the newest attempt consumed when prompt is its whole message: the prompt
// hook of the wake's own turn. A prompt that differs from the whole message, even by one byte,
// matches nothing, and so does one that only contains the token. The match routes and does not
// authenticate: a person who typed the exact message would consume the wake too (ADR 017).
// Without a wake file it does nothing and takes no lock. Any other prompt takes the lock for one
// read of the wake file; only a match protects the directory, outside the lock, and then takes
// the lock again to read and write. With a wake file it cannot read, it does nothing.
// Uses acquireOnly(hookLockBudget) per phase (v0.1.15 semantics, no hold deadline).
func (m fileMemory) consumeWake(client, session, prompt string) error {
	path := m.wakePath(client, session)
	if _, err := os.Stat(path); err != nil {
		return nil
	}
	match := false
	if err := m.underLock(func() {
		d, ok := readWake(path)
		match = ok && d.awaits(prompt)
	}); err != nil || !match {
		return err
	}
	// As for the told state, the directory is protected before the lock.
	if err := m.directory(); err != nil {
		return err
	}
	unlock, err := m.lockAcquire(acquireOnly(hookLockBudget))
	if err != nil {
		return err
	}
	defer unlock()
	d, ok := readWake(path)
	if !ok || !d.awaits(prompt) {
		return nil // the file is unusable now or no longer awaits this prompt
	}
	now := m.clock()()
	d.attempts[len(d.attempts)-1]["consumed_ms"] = json.RawMessage(strconv.FormatInt(now.UnixMilli(), 10))
	d.forget(now.Add(-memoryAge))
	return m.replace(path, d.encode())
}

// forget drops the token, message and answer of every attempt consumed before cutoff. That is all
// that goes when a wake file is written: the attempts and everything else in it stay.
func (d wakeDoc) forget(cutoff time.Time) {
	for _, a := range d.attempts {
		if consumed := attemptTime(a, "consumed_ms"); consumed != 0 && consumed < cutoff.UnixMilli() {
			delete(a, "token")
			delete(a, "message")
			delete(a, "answer")
			delete(a, "scan")
		}
	}
}

// directory makes the sessions directory and protects it.
func (m fileMemory) directory() error {
	if err := os.MkdirAll(m.dir, 0o700); err != nil {
		return err
	}
	return m.restrict(m.dir)
}

// write replaces a file whole, by a protected temporary file and a rename, so a reader sees the
// old content or the new and never part of either. It protects the directory first.
func (m fileMemory) write(path, body string) error {
	if err := m.directory(); err != nil {
		return err
	}
	return m.replace(path, body)
}

// replace is write in a directory already made and protected, for a caller that protected it
// before taking the lock. Uses v0.1.15's fresh per-rename retry budget (zero hold deadline).
func (m fileMemory) replace(path, body string) error {
	tmp, err := prepareSessionFile(m.dir, body, m.restrict)
	if err != nil {
		return err
	}
	err = replaceSessionFileDeadline(tmp, path, time.Time{}, true, m.monoClock(), m.pause)
	if err != nil {
		_ = os.Remove(tmp)
	}
	return err
}

// pruneLimit bounds the number of entries one pruning rechecks and removes under the lock.
// Removing two thousand stale files held it for 305 ms on Windows and 9 ms on Linux, about
// 0.15 ms a file on Windows, against another hook's 50 ms budget. What is left goes at later
// session starts.
const pruneLimit = 64

// prune removes up to pruneLimit entries untouched for memoryAge, leftover temporary files among
// them. It never removes the lock, and it neither lists nor removes the wake files (ADR 017,
// decision 3). It lists and judges the entries before it takes the lock, and judges each stale
// one again under it, since a hook may have written it meanwhile, so the lock is held only for
// rechecks and removals. Uses acquireOnly(hookLockBudget) under v0.1.15/OFF semantics.
func (m fileMemory) prune() error {
	return m.pruneWithSuccession(time.Time{}, false, nil)
}

func (m fileMemory) pruneWithSuccession(callerDeadline time.Time, successionEnabled bool, observe func(string)) error {
	// What the directory cannot list, prune cannot judge, so it keeps it.
	entries, _ := os.ReadDir(m.dir)
	cutoff := m.clock()().Add(-memoryAge)
	var stale []string
	for _, e := range entries {
		if len(stale) == pruneLimit {
			break
		}
		if e.Name() == sessionsLock || e.Name() == wakeDir {
			continue
		}
		if info, err := e.Info(); err == nil && info.ModTime().Before(cutoff) {
			stale = append(stale, filepath.Join(m.dir, e.Name()))
		}
	}
	if len(stale) == 0 {
		return nil
	}
	// ADR 039 fullHold covers succession-evidence pruning when session_succession is ON;
	// when OFF, keep v0.1.15's acquisition-only budget so pruning all pruneLimit entries completes.
	mode := m.lockModeForSuccession(callerDeadline, successionEnabled)
	holdDeadline := mode.holdDeadline
	mono := m.monoClock()
	unlock, err := m.lockAcquire(mode)
	if err != nil {
		return err
	}
	defer unlock()
	for _, path := range stale {
		if deadlineExpired(mono, holdDeadline) {
			return nil
		}
		if strings.HasSuffix(path, ".ended") {
			ended, expired, markerErr := m.readEndMarkerDeadline(path, holdDeadline)
			if errors.Is(markerErr, os.ErrDeadlineExceeded) {
				return nil
			}
			if markerErr != nil {
				// ADR 039 keeps a damaged end marker in place and observes it for doctor.
				if observe != nil {
					observe(damagedGenerationObservation(path))
				}
				continue
			}
			if ended > 0 && expired {
				_, _ = m.cleanupExpiredEnd(strings.TrimSuffix(path, ".ended"), ended, holdDeadline, successionEnabled)
			}
			continue
		}
		if isExecutionFile(path) {
			prefix := strings.TrimSuffix(path, ".execution")
			if hinfo, herr := lstatSessionFile(prefix); herr == nil && !hinfo.ModTime().Before(cutoff) {
				continue
			}
			if _, eerr := lstatSessionFile(prefix + ".ended"); eerr == nil {
				continue
			}
		}
		if info, err := lstatSessionFile(path); err == nil && info.ModTime().Before(cutoff) {
			if deadlineExpired(mono, holdDeadline) {
				return nil
			}
			_ = removeSessionFile(path)
		}
	}
	return nil
}

// memory returns the Runner's Memory, or one beside the configured token file. Without a
// token file there is nowhere private to keep it, and the hook behaves as before.
func (r Runner) memory() Memory {
	if r.Memory != nil {
		return r.Memory
	}
	if r.Getenv == nil {
		return nil
	}
	token, err := (hubclient.EnvConfig{Getenv: r.Getenv}).TokenPath()
	if err != nil {
		return nil
	}
	return fileMemory{dir: filepath.Join(filepath.Dir(token), "sessions"), now: time.Now, restrict: config.OwnerOnly}
}
