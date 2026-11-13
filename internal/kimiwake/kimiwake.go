// Package kimiwake keeps the local state of Kimi's one-shot wake (ADR 030): the wake preference,
// the native session each Kimi process runs now, and one record for each Kimi process and native
// session. The state lives under the OS account's pinned Iris directory, which a model-started
// task cannot redirect through HOME, XDG_CONFIG_HOME or APPDATA, and every change to it is made
// under one lock file there.
package kimiwake

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/mateusz-klatt/iris/internal/config"
	"github.com/mateusz-klatt/iris/internal/proc"
)

// The latch states. Each stops the prompt hook from asking until SessionStart or an explicit start
// clears it.
const (
	LatchDisabled = "disabled"
	LatchFailed   = "failed"
	LatchExpired  = "expired"
)

const (
	dirName        = "kimi-wake"
	preferenceName = "preference"
	sessionsName   = "sessions"
	watchersName   = "watchers"
	lockName       = ".lock"
	recordVersion  = 1
	lockRetry      = 2 * time.Millisecond
	// pruneLimit bounds how many entries one SessionStart checks for an ended process.
	pruneLimit = 64
)

// The operating-system calls a real directory does not fail. Tests replace them.
var (
	createTemp = os.CreateTemp
	protect    = config.OwnerOnly
	rename     = os.Rename
)

// ErrLocked means another process held the state's lock past the caller's budget.
var ErrLocked = errors.New("the Kimi wake state is locked")

var (
	errKey     = errors.New("a Kimi wake key needs a positive pid and start time and a valid session id")
	errVersion = errors.New("a Kimi wake record has an unknown version")
)

// Root is the state directory inside the pinned Iris configuration directory.
func Root(pinned string) string { return filepath.Join(pinned, dirName) }

// Enabled reports whether wake is on. Only a preference file whose trimmed text is exactly "on"
// enables it. A missing, unreadable or any other file means off, which is the default until the
// native acceptance passes: "default", a person's lack of an explicit choice, is off for now.
func Enabled(root string) bool {
	raw, err := os.ReadFile(filepath.Join(root, preferenceName))
	return err == nil && strings.TrimSpace(string(raw)) == "on"
}

// The preference's values. The file records the choice, as Antigravity's marker does: "default"
// when the person made none, "on" or "off" when they did. A missing file means not integrated.
const (
	PreferenceDefault = "default"
	PreferenceOn      = "on"
	PreferenceOff     = "off"
)

// ErrPreference is a preference file that holds none of the three values.
var ErrPreference = errors.New("the Kimi wake preference is not default, on or off")

// Preference reads the recorded choice: "" when there is none yet.
func Preference(root string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(root, preferenceName))
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	switch value := strings.TrimSpace(string(raw)); value {
	case PreferenceDefault, PreferenceOn, PreferenceOff:
		return value, nil
	default:
		return "", ErrPreference
	}
}

// SetPreference records the choice, making and protecting the state directory first.
func SetPreference(root, value string) error {
	switch value {
	case PreferenceDefault, PreferenceOn, PreferenceOff:
	default:
		return ErrPreference
	}
	s := &Store{Root: root}
	if err := s.prepare(); err != nil {
		return err
	}
	return replaceText(filepath.Join(root, preferenceName), value)
}

// terminate stops a process only while its pid and start time still match. Tests replace it.
var terminate = proc.Terminate

// StopWatchers stops every watcher a record shows alive, as turning wake off or removing the
// integration requires, and latches each record it touched disabled once no live watcher holds
// it. A watcher that ends on its own first is skipped. A failed stop is returned, joined, and
// does not keep the others from stopping.
func (s *Store) StopWatchers(budget time.Duration) error {
	var live []Key
	var attempts []Attempt
	err := s.locked(budget, func() error {
		entries, _ := os.ReadDir(filepath.Join(s.Root, watchersName))
		for _, e := range entries {
			k, ok := recordKey(e.Name())
			if !ok {
				continue
			}
			r, _, err := readRecord(s.recordPath(k))
			if err == nil && r.Attempt != nil && s.alive(r.Attempt.PID, r.Attempt.StartMS) {
				live, attempts = append(live, k), append(attempts, *r.Attempt)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	var failed []error
	for i, k := range live {
		a := attempts[i]
		if err := terminate(a.PID, a.StartMS); err != nil && !errors.Is(err, proc.ErrNotFound) {
			failed = append(failed, fmt.Errorf("watcher %d: %w", a.PID, err))
			continue
		}
		failed = append(failed, s.LatchIfFree(k, LatchDisabled, budget))
	}
	return errors.Join(failed...)
}

// recordKey reads the key a record file is named after: "<pid>-<start>-<session>.json".
func recordKey(name string) (Key, bool) {
	base, ok := strings.CutSuffix(name, ".json")
	if !ok {
		return Key{}, false
	}
	fields := strings.SplitN(base, "-", 3)
	if len(fields) != 3 {
		return Key{}, false
	}
	pid, startMS, ok := entryProcess(fields[0] + "-" + fields[1] + ".json")
	k := Key{PID: pid, StartMS: startMS, Session: fields[2]}
	return k, ok && k.valid()
}

// ValidSession accepts the native session ids Kimi gives, "session_" and a UUID. It rejects
// anything a path separator, a drive letter or a second file name could be made from, because the
// id is part of a record's file name.
func ValidSession(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}

// Key names one record: a Kimi process, by its pid and start time, and its native session.
type Key struct {
	PID     int
	StartMS int64
	Session string
}

func (k Key) valid() bool { return k.PID > 0 && k.StartMS > 0 && ValidSession(k.Session) }

// Attempt is a live watcher's claim on its record.
type Attempt struct {
	PID     int    `json:"pid"`
	StartMS int64  `json:"start_ms"`
	ID      string `json:"id"`
}

// Latch stops further requests until SessionStart or an explicit start clears it.
type Latch struct {
	State string `json:"state"`
	AtMS  int64  `json:"at_ms"`
}

// Checkpoint is the watchers' durable cursor. Initialized stays false until a watcher has set a
// baseline, so a record that a request created holds no cursor yet.
type Checkpoint struct {
	Stream      string `json:"stream"`
	Cursor      int64  `json:"cursor"`
	Initialized bool   `json:"initialized"`
}

// Identity is an attention identity: a stream and a cursor in it.
type Identity struct {
	Stream string `json:"stream"`
	Cursor int64  `json:"cursor"`
}

// Record is one key's wake state. A watcher owns it through Attempt. The cursor, the identities a
// watcher emitted and the coalescing deadlines outlive each watcher.
type Record struct {
	Version        int                 `json:"version"`
	ArmRequestedMS int64               `json:"arm_requested_ms,omitempty"`
	Attempt        *Attempt            `json:"attempt,omitempty"`
	Latch          *Latch              `json:"latch,omitempty"`
	Checkpoint     Checkpoint          `json:"checkpoint"`
	Emitted        map[string]Identity `json:"emitted,omitempty"`
	Coalesce       Delays              `json:"coalesce,omitempty"`
}

// Delays are the coalescing deadlines per attention kind, in Unix milliseconds. They only delay a
// wake and never suppress one (ADR 030). So malformed coalescing data decodes as no delay rather
// than failing the record, whose ownership and cursor must stay usable.
type Delays map[string]int64

// UnmarshalJSON keeps every well-formed deadline and drops the rest. It never fails.
func (d *Delays) UnmarshalJSON(raw []byte) error {
	*d = Delays{}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(raw, &fields) // a value that is not an object holds no deadline
	for kind, value := range fields {
		var ms int64
		if json.Unmarshal(value, &ms) == nil {
			(*d)[kind] = ms
		}
	}
	return nil
}

// sessionMap is a Kimi process's current native session, as its SessionStart published it.
type sessionMap struct {
	SessionID string `json:"session_id"`
}

// Store is the state directory, with the clock and the process evidence it decides by.
type Store struct {
	Root string
	// Now reads the clock. Nil means time.Now.
	Now func() time.Time
	// Alive reports whether a process with this pid and start time is running. Nil means the
	// process table, which does not count a zombie as running.
	Alive func(pid int, startMS int64) bool
}

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Store) alive(pid int, startMS int64) bool {
	if s.Alive != nil {
		return s.Alive(pid, startMS)
	}
	return running(pid, startMS)
}

// running checks a process by its pid and start time together, so a reused pid does not count.
func running(pid int, startMS int64) bool {
	if pid <= 0 || startMS <= 0 {
		return false
	}
	p, ok := proc.Lookup(pid)
	return ok && p.StartMS == startMS
}

func (s *Store) sessionPath(pid int, startMS int64) string {
	return filepath.Join(s.Root, sessionsName, fmt.Sprintf("%d-%d.json", pid, startMS))
}

func (s *Store) recordPath(k Key) string {
	return filepath.Join(s.Root, watchersName, fmt.Sprintf("%d-%d-%s.json", k.PID, k.StartMS, k.Session))
}

// prepare makes and protects the directories before the lock is taken. On Windows, protecting a
// directory also visits the files in it, which must not happen while other hooks wait for the
// lock; pruning keeps those files few. Each call protects them again, so a directory that was made
// but not protected, because that step failed, is protected by the next caller.
func (s *Store) prepare() error {
	for _, dir := range []string{s.Root, filepath.Join(s.Root, sessionsName), filepath.Join(s.Root, watchersName)} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		if err := protect(dir); err != nil {
			return err
		}
	}
	return nil
}

// lock takes the state's lock within budget and returns its release. The lock file is opened for
// reading only: a lock needs no write access, so even a restrictive umask leaves it usable.
func (s *Store) lock(budget time.Duration) (func(), error) {
	f, err := os.OpenFile(filepath.Join(s.Root, lockName), os.O_RDONLY|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(budget)
	for tryLock(f) != nil {
		if !time.Now().Before(deadline) {
			_ = f.Close()
			return nil, ErrLocked
		}
		time.Sleep(lockRetry)
	}
	return func() {
		unlock(f)
		_ = f.Close()
	}, nil
}

// locked runs f with the directories made and the lock held.
func (s *Store) locked(budget time.Duration, f func() error) error {
	if err := s.prepare(); err != nil {
		return err
	}
	unlock, err := s.lock(budget)
	if err != nil {
		return err
	}
	defer unlock()
	return f()
}

// readRecord reads a record. A missing one is an empty record of the current version; a malformed
// one, or one of another version, is an error, and the caller writes nothing.
func readRecord(path string) (Record, bool, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Record{Version: recordVersion}, false, nil
	}
	if err != nil {
		return Record{}, false, err
	}
	var r Record
	if err := json.Unmarshal(raw, &r); err != nil {
		return Record{}, false, err
	}
	if r.Version != recordVersion {
		return Record{}, false, errVersion
	}
	return r, true, nil
}

// replace writes a file whole, through a protected temporary file and a rename, so a reader sees
// the old content or the new one and never part of either.
func replace(path string, v any) error {
	raw, _ := json.Marshal(v) // Records and session maps are plain values that always encode.
	return writeWhole(path, raw)
}

// replaceText writes a one-line text file whole, as replace writes a record.
func replaceText(path, text string) error { return writeWhole(path, []byte(text)) }

// writeWhole writes raw and a newline through a protected temporary file and a rename.
func writeWhole(path string, raw []byte) error {
	f, err := createTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	_, err = f.Write(append(raw, '\n'))
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = protect(f.Name())
	}
	if err == nil {
		err = rename(f.Name(), path)
	}
	if err != nil {
		_ = os.Remove(f.Name())
	}
	return err
}

// updateHeld applies change to k's record with the lock held, and writes the record when change
// returns true. change learns whether the record existed.
func (s *Store) updateHeld(k Key, change func(r *Record, exists bool) bool) error {
	path := s.recordPath(k)
	r, exists, err := readRecord(path)
	if err != nil {
		return err
	}
	if !change(&r, exists) {
		return nil
	}
	return replace(path, r)
}

// Session reads, under the lock, the native session the Kimi process (pid, startMS) runs now, as
// its last SessionStart published it. ok is false for a missing or malformed map. The lock
// matters on Windows: a reader holding the map open while a SessionStart renames a new one over it
// would fail that publication.
// A read makes and protects nothing: without the state directory no SessionStart has published a
// map, so there is none.
func (s *Store) Session(pid int, startMS int64, budget time.Duration) (string, bool, error) {
	unlock, err := s.lock(budget)
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	defer unlock()
	session, ok := s.mappedSession(pid, startMS)
	return session, ok, nil
}

// mappedSession reads the map with the lock held. A missing or malformed map is none.
func (s *Store) mappedSession(pid int, startMS int64) (string, bool) {
	raw, err := os.ReadFile(s.sessionPath(pid, startMS))
	if err != nil {
		return "", false
	}
	var m sessionMap
	if json.Unmarshal(raw, &m) != nil || !ValidSession(m.SessionID) {
		return "", false
	}
	return m.SessionID, true
}

// MapSession publishes session as the native session of the Kimi process (pid, startMS),
// replacing any earlier one: after /clear the process runs a new session, which must not inherit
// the old one's watcher or cursor. SessionStart is the point where ADR 030 re-arms, so the new
// key's latch and pending request are cleared, while its cursor, emitted identities and
// coalescing stay. It also removes a bounded number of entries whose process has ended.
func (s *Store) MapSession(pid int, startMS int64, session string, budget time.Duration) error {
	k := Key{PID: pid, StartMS: startMS, Session: session}
	if !k.valid() {
		return errKey
	}
	return s.locked(budget, func() error {
		if err := replace(s.sessionPath(pid, startMS), sessionMap{SessionID: session}); err != nil {
			return err
		}
		s.prune(pid, startMS)
		return s.updateHeld(k, func(r *Record, exists bool) bool {
			if !exists || (r.Latch == nil && r.ArmRequestedMS == 0) {
				return false
			}
			r.Latch, r.ArmRequestedMS = nil, 0
			return true
		})
	})
}

// RequestArm decides, at a user prompt that starts a turn of k's session, whether to ask the model
// for a watcher, and records the request. It asks only when all of these hold: the process's map
// still names this session, no latch is set, no watcher is live, and no request is pending. A
// request still pending here was not answered by the turn that received it, which has ended, so it
// latches failed instead of asking at every turn. ask is never true unless the request was written,
// so a failed write cannot ask twice.
func (s *Store) RequestArm(k Key, budget time.Duration) (ask bool, err error) {
	if !k.valid() {
		return false, errKey
	}
	err = s.locked(budget, func() error {
		if current, ok := s.mappedSession(k.PID, k.StartMS); !ok || current != k.Session {
			return nil
		}
		return s.updateHeld(k, func(r *Record, _ bool) bool {
			if r.Latch != nil || (r.Attempt != nil && s.alive(r.Attempt.PID, r.Attempt.StartMS)) {
				return false
			}
			now := s.now().UnixMilli()
			if r.ArmRequestedMS != 0 {
				r.ArmRequestedMS, r.Latch = 0, &Latch{State: LatchFailed, AtMS: now}
				return true
			}
			r.ArmRequestedMS, ask = now, true
			return true
		})
	})
	return ask && err == nil, err
}

// Own claims k's record for the watcher self. It is a watcher's first step, before any hub call, so
// the request it answers is cleared at once. It refuses, writing nothing, while another live
// watcher holds the record: that start is a duplicate. A watcher's start is an explicit start, so it
// also clears a latch, including one a prompt set because it was judged before this publication.
func (s *Store) Own(k Key, self Attempt, budget time.Duration) (owned bool, err error) {
	if !k.valid() || self.PID <= 0 || self.StartMS <= 0 || self.ID == "" {
		return false, errKey
	}
	err = s.locked(budget, func() error {
		return s.updateHeld(k, func(r *Record, _ bool) bool {
			if r.Attempt != nil && r.Attempt.ID != self.ID && s.alive(r.Attempt.PID, r.Attempt.StartMS) {
				return false
			}
			claim := self
			r.Attempt, r.ArmRequestedMS, r.Latch, owned = &claim, 0, nil, true
			return true
		})
	})
	return owned && err == nil, err
}

// UpdateOwned applies change to k's record and writes it, but only while the watcher id still holds
// the record. owned is false, with nothing written, once another watcher or nobody holds it.
func (s *Store) UpdateOwned(k Key, id string, budget time.Duration, change func(*Record)) (owned bool, err error) {
	if !k.valid() || id == "" {
		return false, errKey
	}
	err = s.locked(budget, func() error {
		return s.updateHeld(k, func(r *Record, _ bool) bool {
			if r.Attempt == nil || r.Attempt.ID != id {
				return false
			}
			change(r)
			owned = true
			return true
		})
	})
	return owned && err == nil, err
}

// LatchIfFree latches k in state unless a live watcher holds it: a failure or a disabled wake never
// replaces a live watcher. It clears a pending request, which the latch supersedes.
func (s *Store) LatchIfFree(k Key, state string, budget time.Duration) error {
	if !k.valid() {
		return errKey
	}
	return s.locked(budget, func() error {
		return s.updateHeld(k, func(r *Record, _ bool) bool {
			if r.Attempt != nil && s.alive(r.Attempt.PID, r.Attempt.StartMS) {
				return false
			}
			r.Latch, r.ArmRequestedMS = &Latch{State: state, AtMS: s.now().UnixMilli()}, 0
			return true
		})
	})
}

// Read reads k's record under the lock. exists is false for a key that has none. Like Session, it
// makes and protects nothing.
func (s *Store) Read(k Key, budget time.Duration) (r Record, exists bool, err error) {
	if !k.valid() {
		return Record{}, false, errKey
	}
	unlock, err := s.lock(budget)
	if errors.Is(err, fs.ErrNotExist) {
		return Record{Version: recordVersion}, false, nil
	}
	if err != nil {
		return Record{}, false, err
	}
	defer unlock()
	return readRecord(s.recordPath(k))
}

// pruneStart picks where in the listing a pruning pass begins, from 0 to n-1. Starting at a random
// place means the live entries early in the listing cannot use up every pass's limit and keep the
// ended entries after them from ever being checked. Tests replace it.
var pruneStart = rand.IntN

// prune removes up to pruneLimit entries whose Kimi process has ended, with the lock held. Once its
// process ends, a record's cursor serves nobody: a new process starts new keys. The process whose
// SessionStart is pruning is running, whatever a process lookup says, and a name that is not a
// state file's is left alone.
func (s *Store) prune(selfPID int, selfStartMS int64) {
	var entries []string
	for _, dir := range []string{sessionsName, watchersName} {
		listed, _ := os.ReadDir(filepath.Join(s.Root, dir))
		for _, e := range listed {
			entries = append(entries, filepath.Join(dir, e.Name()))
		}
	}
	// One more than the count, so an empty listing needs no case of its own; a start equal to
	// the count is the same place as zero.
	start := pruneStart(len(entries) + 1)
	checked := 0
	for i := range entries {
		entry := entries[(start+i)%len(entries)]
		pid, startMS, ok := entryProcess(filepath.Base(entry))
		if !ok || (pid == selfPID && startMS == selfStartMS) {
			continue
		}
		if checked == pruneLimit {
			return
		}
		checked++
		if !s.alive(pid, startMS) {
			_ = os.Remove(filepath.Join(s.Root, entry))
		}
	}
}

// entryProcess reads the process a state file is named after: "<pid>-<start>.json" for a map and
// "<pid>-<start>-<session>.json" for a record.
func entryProcess(name string) (int, int64, bool) {
	base, ok := strings.CutSuffix(name, ".json")
	if !ok {
		return 0, 0, false
	}
	fields := strings.SplitN(base, "-", 3)
	if len(fields) < 2 {
		return 0, 0, false
	}
	pid, pidErr := strconv.Atoi(fields[0])
	startMS, startErr := strconv.ParseInt(fields[1], 10, 64)
	if pidErr != nil || startErr != nil || pid <= 0 || startMS <= 0 {
		return 0, 0, false
	}
	return pid, startMS, true
}
