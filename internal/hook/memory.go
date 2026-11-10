package hook

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/mateusz-klatt/iris/internal/config"
	"github.com/mateusz-klatt/iris/internal/hubclient"
)

// Memory keeps the generation a hook last saw for a client session. After a hub restart a
// session's Execution waits for proof that it is the same session (D9), and a hook is a new
// process for every tool call, so without this the first edits of a recovering session had
// none to give.
type Memory interface {
	Recall(client, session string) int
	Remember(client, session string, generation int)
	Forget(client, session string)
}

// memoryAge is how long an untouched entry survives. A session that ends normally removes its
// own; this only clears the entries of sessions that never reached SessionEnd.
const memoryAge = 30 * 24 * time.Hour

// fileMemory stores one small file per session beside the token, protected as the token is:
// mode 0600 in a 0700 directory, and on Windows the token's owner-only DACL. So anyone who
// can read it can read the token, which already grants everything the generation would.
type fileMemory struct {
	dir      string
	now      func() time.Time
	restrict func(string) error
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

func (m fileMemory) Recall(client, session string) int {
	first := m.read(m.name(client, session))
	generation, err := strconv.Atoi(first)
	if err != nil || generation < 0 {
		return 0
	}
	return generation
}

// Remember is best effort. A failed write only means the next restart costs this session
// its proof, which is what every session had before.
func (m fileMemory) Remember(client, session string, generation int) {
	_ = m.write(m.name(client, session), strconv.Itoa(generation))
}

// Forget removes the session's generation. The told state goes when the session ends
// (forgetAttention) or when pruning finds it untouched for memoryAge; the wake file stays.
func (m fileMemory) Forget(client, session string) {
	_ = os.Remove(m.name(client, session))
}

// sessionsLock is the sessions directory's lock. Every read and every change of an attention file
// or a wake file, and every removal by pruning, holds it (ADR 017). Reads hold it too because on
// Windows the rename used here fails when another process holds the destination file open. It is
// created once and never pruned, renamed or replaced, so every process locks the same file.
const sessionsLock = ".lock"

// hookLockBudget is how long a hook tries for the lock. Nobody holds it across a network call or
// while a client runs, or while the directory is protected; a hold is file reads and writes only.
const hookLockBudget = 50 * time.Millisecond

// lockRetry is the pause between two tries for the lock.
const lockRetry = 2 * time.Millisecond

// lock takes the sessions directory's lock within budget and returns its release. A lock held
// past the budget is failSessionsLock, and the caller then reads and writes nothing.
func (m fileMemory) lock(budget time.Duration) (func(), error) {
	// A caller that writes protects the directory before it takes the lock: that costs Windows
	// time in proportion to the files in it, about 80 ms for a thousand sessions' files, because
	// Windows applies the directory's inheritable protection to each. The lock file holds nothing,
	// so the lock protects the directory only where it has to make it.
	if _, err := os.Stat(m.dir); err != nil {
		if err := m.directory(); err != nil {
			return nil, err
		}
	}
	path := filepath.Join(m.dir, sessionsLock)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(budget)
	for tryLockFile(f) != nil {
		if !time.Now().Before(deadline) {
			_ = f.Close()
			return nil, failSessionsLock
		}
		time.Sleep(lockRetry)
	}
	// An Iris from before ADR 017 on the same host prunes any file untouched for 30 days, and a
	// .lock it removed would leave two hooks locking two files. Each lock keeps the file young.
	now := time.Now()
	_ = os.Chtimes(path, now, now)
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

func (m fileMemory) toldRevision(client, session string) (int64, error) {
	rev := fromReplay
	err := m.underLock(func() { rev = parseTold(m.read(m.counts(client, session))).Rev })
	return rev, err
}

// underLock runs f under the lock, or returns the error from taking it.
func (m fileMemory) underLock(f func()) error {
	unlock, err := m.lock(hookLockBudget)
	if err != nil {
		return err
	}
	defer unlock()
	f()
	return nil
}

func (m fileMemory) updateAttention(client, session string, f func(*toldState) bool) error {
	// The directory is protected before the lock: on Windows that costs time in proportion to the
	// files in it, and the lock is held only for a few small reads and writes.
	if err := m.directory(); err != nil {
		return err
	}
	unlock, err := m.lock(hookLockBudget)
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

func (m fileMemory) forgetAttention(client, session string) error {
	unlock, err := m.lock(hookLockBudget)
	if err != nil {
		return err
	}
	defer unlock()
	_ = os.Remove(m.counts(client, session))
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
	unlock, err := m.lock(hookLockBudget)
	if err != nil {
		return err
	}
	defer unlock()
	d, ok := readWake(path)
	if !ok || !d.awaits(prompt) {
		return nil // the file is unusable now or no longer awaits this prompt
	}
	now := m.now()
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
// before taking the lock.
func (m fileMemory) replace(path, body string) error {
	f, err := os.CreateTemp(m.dir, ".tmp-*")
	if err != nil {
		return err
	}
	_, err = f.WriteString(strings.TrimRight(body, "\n") + "\n")
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	// The file is protected before it gets its name, so it is never readable as an entry.
	if err == nil {
		err = m.restrict(f.Name())
	}
	if err == nil {
		err = os.Rename(f.Name(), path)
	}
	if err != nil {
		_ = os.Remove(f.Name())
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
// rechecks and removals.
func (m fileMemory) prune() error {
	// What the directory cannot list, prune cannot judge, so it keeps it.
	entries, _ := os.ReadDir(m.dir)
	cutoff := m.now().Add(-memoryAge)
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
	unlock, err := m.lock(hookLockBudget)
	if err != nil {
		return err
	}
	defer unlock()
	for _, path := range stale {
		if info, err := os.Lstat(path); err == nil && info.ModTime().Before(cutoff) {
			_ = os.Remove(path)
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
