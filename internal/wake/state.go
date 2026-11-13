package wake

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/mateusz-klatt/iris/internal/config"
	"github.com/mateusz-klatt/iris/internal/hook"
	"github.com/mateusz-klatt/iris/internal/hubclient"
)

var (
	errStateUnusable  = errors.New("wake state is unusable")
	errStateChanged   = errors.New("wake state changed")
	errAttemptMissing = errors.New("wake attempt is missing")
)

type binaryInfo struct {
	Path          string `json:"path,omitempty"`
	Version       string `json:"version,omitempty"`
	ClientVersion string `json:"client_version,omitempty"`
	CheckedMS     int64  `json:"checked_ms,omitempty"`
}

type failure struct {
	AtMS   int64  `json:"at_ms"`
	Detail string `json:"detail"`
}

type attempt struct {
	ID         string                      `json:"id"`
	Kind       string                      `json:"kind"`
	Stream     string                      `json:"stream"`
	Cursor     int64                       `json:"cursor"`
	Slot       int                         `json:"slot"`
	StartedMS  int64                       `json:"started_ms"`
	Token      string                      `json:"token,omitempty"`
	Message    string                      `json:"message,omitempty"`
	Scan       *hook.AntigravityScanCursor `json:"scan,omitempty"`
	Answer     string                      `json:"answer,omitempty"`
	AnsweredMS int64                       `json:"answered_ms,omitempty"`
	Detail     string                      `json:"detail,omitempty"`
	ConsumedMS int64                       `json:"consumed_ms,omitempty"`
}

type document struct {
	Version     int        `json:"version"`
	Rev         int64      `json:"rev"`
	Binary      binaryInfo `json:"binary,omitempty"`
	LastFailure *failure   `json:"last_failure,omitempty"`
	Attempts    []attempt  `json:"attempts"`
}

func (a attempt) outstanding() bool {
	return a.Kind == "queue" && a.ConsumedMS == 0 && a.Answer != "failed" && a.Answer != "aborted"
}

func (a attempt) spent() bool { return a.ConsumedMS != 0 || a.Answer != "aborted" }

func (d document) outstanding() *attempt {
	for i := range d.Attempts {
		if d.Attempts[i].outstanding() {
			return &d.Attempts[i]
		}
	}
	return nil
}

func (d document) mark() hubclient.Identity {
	if len(d.Attempts) == 0 {
		return hubclient.Identity{}
	}
	a := d.Attempts[len(d.Attempts)-1]
	return hubclient.Identity{Known: true, Pending: true, Stream: a.Stream, Cursor: a.Cursor}
}

func (d document) quota(slot int) int {
	n := 0
	for _, a := range d.Attempts {
		if a.Slot == slot && a.spent() {
			if a.ConsumedMS != 0 || a.Answer != "failed" {
				return 2
			}
			n++
		}
	}
	if n > 2 {
		return 2
	}
	return n
}

func (d document) tries(slot int) int {
	n := 0
	for _, a := range d.Attempts {
		if a.Slot == slot && a.spent() {
			n++
		}
	}
	return n
}

// appendAttempt drops superseded unconsumed aborts only as their replacement is prepared.
// The old file continues to retain its mark until publish succeeds.
func (d document) appendAttempt(a attempt) document {
	next := make([]attempt, 0, len(d.Attempts)+1)
	for _, old := range d.Attempts {
		if old.spent() {
			next = append(next, old)
		}
	}
	d.Attempts = append(next, a)
	return d
}

func (d document) valid() bool {
	if d.Version != hook.WakeVersion || d.Rev < 0 || len(d.Attempts) > 5 || d.Binary.CheckedMS < 0 ||
		(d.LastFailure != nil && (d.LastFailure.AtMS <= 0 || d.LastFailure.Detail == "")) {
		return false
	}
	seen := make(map[string]bool, len(d.Attempts))
	var stream string
	var cursor int64
	for i, a := range d.Attempts {
		id, err := hex.DecodeString(a.ID)
		if err != nil || len(id) != 16 || hex.EncodeToString(id) != a.ID || seen[a.ID] || a.Stream == "" || a.Cursor <= 0 ||
			(a.Slot != 0 && a.Slot != 1) || a.StartedMS <= 0 || a.AnsweredMS < 0 || a.ConsumedMS < 0 {
			return false
		}
		seen[a.ID] = true
		if i == 0 {
			stream, cursor = a.Stream, a.Cursor
		}
		if a.Stream != stream || a.Cursor != cursor {
			return false
		}
		switch a.Kind {
		case "binary_check":
			if a.Token != "" || a.Message != "" || a.Scan != nil || a.ConsumedMS != 0 ||
				(a.Answer != "failed" && a.Answer != "aborted") || a.AnsweredMS <= 0 {
				return false
			}
		case "queue":
			if (a.Token == "") != (a.Message == "") || (a.ConsumedMS == 0 && a.Token == "") ||
				(a.Answer != "" && a.AnsweredMS <= 0) || (a.Scan != nil && a.Scan.ScannedOffset < 0) {
				return false
			}
		default:
			return false
		}
		if a.outstanding() && i != len(d.Attempts)-1 {
			return false
		}
	}
	return d.tries(0) <= 2 && d.tries(1) <= 2
}

type stateSnapshot struct {
	Told     hook.ToldSnapshot
	Present  bool
	Raw      string
	Document document
}

func sameWake(a, b stateSnapshot) bool { return a.Present == b.Present && a.Raw == b.Raw }
func sameState(a, b stateSnapshot) bool {
	return sameWake(a, b) && a.Told.Present == b.Told.Present && a.Told.Raw == b.Told.Raw
}

// Published is true after rename, including a read-back or directory-sync failure. In that
// case the caller must abort ID if its command certainly did not start; it must never guess
// another reservation by token or position.
type publication struct {
	Published bool
	ID        string
}

type stateFile interface {
	Name() string
	Write([]byte) (int, error)
	Sync() error
	Close() error
}

type stateIO struct {
	readFile    func(string) ([]byte, error)
	createTemp  func(string, string) (stateFile, error)
	rename      func(string, string) error
	remove      func(string) error
	protectFile func(string) error
	protectDir  func(string) error
	syncDir     func(string) error
}

type stateStore struct {
	sessions *hook.Sessions
	client   string
	session  string
	io       stateIO
}

func newStateStore(sessions *hook.Sessions, session string) *stateStore {
	return newClientStateStore(sessions, "codex", session)
}

func newClientStateStore(sessions *hook.Sessions, client, session string) *stateStore {
	if client == "" {
		client = "codex"
	}
	return &stateStore{sessions: sessions, client: client, session: session, io: stateIO{
		readFile:   os.ReadFile,
		createTemp: func(dir, pattern string) (stateFile, error) { return os.CreateTemp(dir, pattern) },
		rename:     os.Rename, remove: os.Remove, protectFile: func(path string) error {
			if err := os.Chmod(path, 0600); err != nil {
				return err
			}
			return config.OwnerOnly(path)
		}, protectDir: privateDirectory, syncDir: syncDirectory,
	}}
}

func (s *stateStore) path() string { return s.sessions.WakePath(s.client, s.session) }

func (s *stateStore) snapshot() (stateSnapshot, error) {
	if err := s.protect(); err != nil {
		return stateSnapshot{}, err
	}
	unlock, err := s.sessions.Lock(time.Second)
	if err != nil {
		return stateSnapshot{}, err
	}
	defer unlock()
	return s.snapshotLocked()
}

func (s *stateStore) snapshotLocked() (stateSnapshot, error) {
	told, err := s.sessions.Told(s.client, s.session)
	if err != nil {
		return stateSnapshot{}, err
	}
	result := stateSnapshot{Told: told, Document: document{Version: hook.WakeVersion}}
	raw, err := s.io.readFile(s.path())
	if errors.Is(err, os.ErrNotExist) {
		return result, nil
	}
	if err != nil {
		return stateSnapshot{}, err
	}
	result.Present, result.Raw = true, string(raw)
	result.Document = document{}
	var required struct {
		Version  *int               `json:"version"`
		Rev      *int64             `json:"rev"`
		Attempts *[]json.RawMessage `json:"attempts"`
	}
	if json.Unmarshal(raw, &result.Document) != nil || json.Unmarshal(raw, &required) != nil ||
		required.Version == nil || required.Rev == nil || required.Attempts == nil || !result.Document.valid() {
		return stateSnapshot{}, errStateUnusable
	}
	return result, nil
}

// prepare does directory protection and every potentially slow file sync outside the shared
// sessions lock. The temporary file is private before its first byte is written.
func (s *stateStore) prepare(d document) (string, []byte, error) {
	if !d.valid() {
		return "", nil, errStateUnusable
	}
	if d.Attempts == nil {
		d.Attempts = []attempt{}
	}
	if err := s.protect(); err != nil {
		return "", nil, err
	}
	dir := filepath.Dir(s.path())
	body, _ := json.Marshal(d)
	body = append(body, '\n')
	f, err := s.io.createTemp(dir, ".tmp-*")
	if err != nil {
		return "", nil, err
	}
	name := f.Name()
	err = s.io.protectFile(name)
	if err == nil {
		var n int
		n, err = f.Write(body)
		if err == nil && n != len(body) {
			err = io.ErrShortWrite
		}
	}
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		_ = s.io.remove(name)
		return "", nil, err
	}
	return name, body, nil
}

func (s *stateStore) protect() error {
	if err := s.io.protectDir(s.sessions.Dir()); err != nil {
		return err
	}
	return s.io.protectDir(filepath.Dir(s.path()))
}

func (s *stateStore) publish(expected stateSnapshot, next document) (publication, error) {
	next.Version, next.Rev = hook.WakeVersion, expected.Document.Rev+1
	return s.replace(expected, next, true)
}

func (s *stateStore) replace(expected stateSnapshot, next document, compareTold bool) (publication, error) {
	result := publication{}
	if len(next.Attempts) != 0 {
		result.ID = next.Attempts[len(next.Attempts)-1].ID
	}
	name, body, err := s.prepare(next)
	if err != nil {
		return result, err
	}
	defer func() { _ = s.io.remove(name) }()
	unlock, err := s.sessions.Lock(time.Second)
	if err != nil {
		return result, err
	}
	current, err := s.snapshotLocked()
	if err == nil && (!sameWake(expected, current) || (compareTold && !sameState(expected, current))) {
		err = errStateChanged
	}
	if err == nil {
		err = s.io.rename(name, s.path())
		result.Published = err == nil
	}
	if err == nil {
		var got []byte
		got, err = s.io.readFile(s.path())
		if err == nil && string(got) != string(body) {
			err = errStateUnusable
		}
	}
	// os.ReadFile closes its read-back handle before releasing the shared lock, required by
	// Windows replacement. Directory sync is deliberately after unlock.
	unlock()
	if err == nil {
		err = s.io.syncDir(filepath.Dir(s.path()))
	}
	return result, err
}

// answer merges by immutable ID. A concurrent hook or publisher causes a changed-snapshot
// error, so retrying reads its consumed mark and every newer attempt before preparing again.
func (s *stateStore) answer(id, outcome string, now int64, detail string) error {
	if id == "" || now <= 0 || (outcome != "submitted" && outcome != "stored" && outcome != "failed" && outcome != "unknown" && outcome != "aborted") {
		return errStateUnusable
	}
	before, err := s.snapshot()
	if err != nil {
		return err
	}
	next := before.Document
	next.Attempts = append([]attempt(nil), next.Attempts...)
	found := false
	for i := range next.Attempts {
		if next.Attempts[i].ID == id {
			next.Attempts[i].Answer, next.Attempts[i].AnsweredMS, next.Attempts[i].Detail = outcome, now, detail
			if outcome == "failed" || outcome == "aborted" {
				next.Attempts[i].Scan = nil
			}
			if outcome == "failed" && i == len(next.Attempts)-1 {
				next.LastFailure = &failure{AtMS: now, Detail: detail}
			}
			found = true
		}
	}
	if !found {
		return errAttemptMissing
	}
	next.Rev++
	_, err = s.replace(before, next, false)
	return err
}
