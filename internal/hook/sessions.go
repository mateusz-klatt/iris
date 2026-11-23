package hook

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mateusz-klatt/iris/internal/config"
	"github.com/mateusz-klatt/iris/internal/hubclient"
)

// WakeVersion is the wake document version the prompt hook can consume.
const WakeVersion = wakeVersion

// SecondNoteAfter is the wait before the same pending attention can be announced twice.
const SecondNoteAfter = secondNoteAfter

// ErrSessionsLocked means another session-file operation held the lock past its budget.
var ErrSessionsLocked = failSessionsLock

// ErrInvalidGeneration reports that a session's generation file exists but could not be read
// or did not hold a positive integer generation.
var ErrInvalidGeneration = errors.New("invalid remembered generation")

// DamagedGenerationProof is the fixed kind for observations about unreadable generation files.
const DamagedGenerationProof = "damaged generation proof"

func damagedGenerationObservation(path string) string { return DamagedGenerationProof + " at " + path }

// Sessions shares the hooks' session paths and lock with a wake runner. Only hooks write told
// state and consume wakes; a runner may read told state to decide whether a wake is still due.
type Sessions struct {
	memory    fileMemory
	tokenPath string
}

// NewSessions locates the session files beside tokenPath without reading the token or creating
// any files. It uses the same session names as the hooks.
func NewSessions(tokenPath string) *Sessions {
	return &Sessions{tokenPath: tokenPath, memory: fileMemory{
		dir: filepath.Join(filepath.Dir(tokenPath), "sessions"), now: time.Now, restrict: config.OwnerOnly,
	}}
}

// Dir returns the sessions directory containing the shared stable .lock file.
func (s *Sessions) Dir() string { return s.memory.dir }

// WakePath returns the wake document path for this client and session.
func (s *Sessions) WakePath(client, session string) string { return s.memory.wakePath(client, session) }

// Protect creates and protects the sessions directory using the hooks' protection. Call it
// before Lock: on Windows protecting a directory can also visit the files below it.
func (s *Sessions) Protect() error { return s.memory.directory() }

// Lock takes the hooks' shared sessions lock within budget. A caller must protect directories
// before locking and release the lock before directory protection, network calls or commands.
func (s *Sessions) Lock(budget time.Duration) (unlock func(), err error) {
	return s.memory.lock(budget)
}

// ToldSnapshot preserves a told file's complete bytes and presence alongside the parsed told
// identity. Comparing both detects changes that reuse a revision, including empty-file changes.
type ToldSnapshot struct {
	Present  bool
	Raw      string
	Identity hubclient.Identity
	ToldMS   int64
	Twice    bool
	Revision int64
}

// Told reads a told snapshot while the caller holds Lock. Only a missing file is fresh state;
// other read errors refuse the snapshot. Parsing uses the same first-line format as the hooks,
// from these exact bytes, and never changes the told file.
func (s *Sessions) Told(client, session string) (ToldSnapshot, error) {
	raw, err := os.ReadFile(s.memory.counts(client, session))
	if errors.Is(err, os.ErrNotExist) {
		return ToldSnapshot{}, nil
	}
	if err != nil {
		return ToldSnapshot{}, err
	}
	first, _, _ := strings.Cut(string(raw), "\n")
	told := parseTold(strings.TrimSpace(first))
	return ToldSnapshot{
		Present: true, Raw: string(raw),
		Identity: hubclient.Identity{Known: true, Pending: told.Stream != "", Stream: told.Stream, Cursor: told.Cursor},
		ToldMS:   told.At, Twice: told.Twice, Revision: told.Rev,
	}, nil
}

// RecallGeneration reads the generation last remembered by a hook for client and session,
// without creating any directory or file. A missing entry reports present=false and nil error;
// a readable entry holding a positive integer reports (gen, true, nil); any other read error
// or malformed content reports present=true and ErrInvalidGeneration.
func (s *Sessions) RecallGeneration(client, session string) (int, bool, error) {
	if client == "" || session == "" {
		return 0, false, nil
	}
	generation, err := readGeneration(s.memory.name(client, session))
	return generation, generation > 0 || err != nil, err
}

// RecallBindingGeneration returns live proof and the last recorded end separately. A zero
// value means that file was absent. Damage to either file is recorded for doctor.
func (s *Sessions) RecallBindingGeneration(client, session string) (generation, endedThrough int, err error) {
	if client == "" || session == "" {
		return 0, 0, nil
	}
	path := s.memory.name(client, session)
	generation, err = readGeneration(path)
	if err == nil {
		path += ".ended"
		endedThrough, err = readGeneration(path)
	}
	if err != nil {
		env := map[string]string{"IRIS_TOKEN_FILE": s.tokenPath}
		recorder := Runner{ClientName: client, EventName: "bind", Getenv: func(key string) string { return env[key] }}
		recorder.record(stageObserved, damagedGenerationObservation(path), nil)
	}
	return generation, endedThrough, err
}
