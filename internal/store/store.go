// Package store owns Iris's serialized, transactional domain state.
package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed migrations/001.sql
var schema string

//go:embed migrations/002.sql
var threadsSchema string

//go:embed migrations/003.sql
var namesSchema string

//go:embed migrations/004.sql
var shellSchema string

//go:embed migrations/005.sql
var tokensSchema string

// Clock separates lease ticks from persisted wall-clock expiries. Tick must be monotonic
// within a Store's lifetime; client timestamps never participate in lease decisions.
type Clock interface {
	Now() time.Time
	Tick() int64
}
type systemClock struct{ start time.Time }

func (c systemClock) Now() time.Time { return time.Now() }
func (c systemClock) Tick() int64    { return time.Since(c.start).Milliseconds() }

type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Details any    `json:"details"`
}

func (e *Error) Error() string        { return e.Message }
func fail(code, message string) error { return &Error{code, message, map[string]any{}} }

type Store struct {
	mu      sync.Mutex
	db      *sql.DB
	lock    *os.File
	clock   Clock
	epoch   int64
	stream  string
	changed chan struct{}
}
type transaction struct {
	conn   *sql.Conn
	ctx    context.Context
	s      *Store
	notify bool
}

func (t *transaction) exec(q string, a ...any) (sql.Result, error) {
	return t.conn.ExecContext(t.ctx, q, a...)
}
func (t *transaction) row(q string, a ...any) *sql.Row { return t.conn.QueryRowContext(t.ctx, q, a...) }

// queryRows records both decoding and iteration failures. Callers check Err after
// iteration, so malformed rows cannot escape as a partially successful result.
type queryRows struct {
	*sql.Rows
	scanErr error
}

func (r *queryRows) Scan(dst ...any) { r.scanErr = r.Rows.Scan(dst...) }
func (r *queryRows) Next() bool      { return r.scanErr == nil && r.Rows.Next() }
func (r *queryRows) Err() error {
	if r.scanErr != nil {
		return r.scanErr
	}
	return r.Rows.Err()
}
func (t *transaction) rows(q string, a ...any) (*queryRows, error) {
	rows, err := t.conn.QueryContext(t.ctx, q, a...)
	if err != nil {
		return nil, err
	}
	return &queryRows{Rows: rows}, nil
}
func id(prefix string) string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never returns an error
	return prefix + hex.EncodeToString(b[:])
}

func newStreamID() string {
	raw := id("")
	return raw[:8] + "-" + raw[8:12] + "-4" + raw[13:16] + "-a" + raw[17:20] + "-" + raw[20:]
}

func Open(filename string, clock Clock) (*Store, error) {
	return openDatabase(filename, clock, "sqlite")
}

// openDatabase permits tests to wrap the real driver at its I/O boundary.
func openDatabase(filename string, clock Clock, driverName string) (*Store, error) {
	if clock == nil {
		clock = systemClock{time.Now()}
	}
	abs, err := resolveDatabasePath(filename, filepath.EvalSymlinks)
	if err != nil {
		return nil, err
	}
	lock, err := lockDatabase(abs + ".lock")
	if err != nil {
		return nil, err
	}
	if err = createDatabaseFile(abs); err != nil {
		unlockDatabase(lock)
		return nil, err
	}
	db, err := sql.Open(driverName, abs)
	if err != nil {
		unlockDatabase(lock)
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db, lock: lock, clock: clock, changed: make(chan struct{})}
	for _, q := range []string{"PRAGMA foreign_keys=ON", "PRAGMA journal_mode=WAL", "PRAGMA synchronous=FULL", "PRAGMA busy_timeout=5000"} {
		if _, err = db.Exec(q); err != nil {
			s.Close()
			return nil, err
		}
	}
	err = s.transact(context.Background(), func(t *transaction) error {
		if _, err := t.exec(schema); err != nil {
			return err
		}
		var version int
		if err := t.row("PRAGMA user_version").Scan(&version); err != nil {
			return err
		}
		if version < 2 {
			if _, err := t.exec(threadsSchema); err != nil {
				return err
			}
		}
		if version < 3 {
			if _, err := t.exec(namesSchema); err != nil {
				return err
			}
			if err := t.backfillRepositoryNames(); err != nil {
				return err
			}
		}
		if version < 4 {
			if _, err := t.exec(shellSchema); err != nil {
				return err
			}
		}
		if version < 5 {
			if _, err := t.exec(tokensSchema); err != nil {
				return err
			}
		}
		if _, err := t.exec("INSERT OR IGNORE INTO Meta VALUES(1,?,0)", newStreamID()); err != nil {
			return err
		}
		if _, err := t.exec("UPDATE Meta SET boot_epoch=boot_epoch+1 WHERE id=1"); err != nil {
			return err
		}
		if err := t.row("SELECT stream_id,boot_epoch FROM Meta WHERE id=1").Scan(&s.stream, &s.epoch); err != nil {
			return err
		}
		if _, err := t.exec("UPDATE Execution SET recovery_deadline=? WHERE state='active' AND recovery_deadline IS NULL", clock.Now().Add(120*time.Second).UnixMilli()); err != nil {
			return err
		}
		// Attempts survive reassignment, so a pre-crash acknowledgement cannot match a new fetch.
		if _, err := t.exec("UPDATE Delivery SET execution_id=NULL WHERE id NOT IN (SELECT delivery_id FROM Ack)"); err != nil {
			return err
		}
		return t.expire()
	})
	if err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	err := s.db.Close()
	unlockDatabase(s.lock)
	return err
}
func (s *Store) Changes() <-chan struct{} { s.mu.Lock(); defer s.mu.Unlock(); return s.changed }
func (s *Store) transact(ctx context.Context, f func(*transaction) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	// Cancellation can be reported after SQLite has executed BEGIN. Install cleanup
	// before starting it, so that error cannot leak an open transaction into the pool.
	defer c.ExecContext(context.Background(), "ROLLBACK") // also covers cancellation and panics
	if _, err = c.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	tx := &transaction{conn: c, ctx: ctx, s: s}
	if err = f(tx); err != nil {
		return err
	}
	if _, err = c.ExecContext(ctx, "COMMIT"); err != nil {
		return err
	}
	if tx.notify {
		close(s.changed)
		s.changed = make(chan struct{})
	}
	return nil
}
func (t *transaction) expire() error {
	now, tick := t.s.clock.Now().UnixMilli(), t.s.clock.Tick()
	_, err := t.exec(`UPDATE Execution SET state='lost' WHERE state='active' AND
 ((recovery_deadline IS NOT NULL AND recovery_deadline<=?) OR
 (recovery_deadline IS NULL AND (boot_epoch<>? OR deadline_tick_ms<=?)))`, now, t.s.epoch, tick)
	if err != nil {
		return err
	}
	rows, err := t.rows(`SELECT c.fence,c.repository_id FROM Claim c JOIN Execution e ON e.id=c.execution_id
 WHERE c.revoked_ms IS NULL AND (e.state<>'active' OR (c.expires_ms IS NOT NULL AND c.expires_ms<=?))`, now)
	if err != nil {
		return err
	}
	type expired struct {
		f int64
		r string
	}
	var old []expired
	for rows.Next() {
		var x expired
		rows.Scan(&x.f, &x.r)
		old = append(old, x)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, x := range old {
		if _, err = t.exec("UPDATE Claim SET revoked_ms=? WHERE fence=?", now, x.f); err != nil {
			return err
		}
		if err = t.repoEvent(x.r, "claim_released", fmt.Sprint(x.f)); err != nil {
			return err
		}
	}
	_, err = t.exec(`UPDATE Delivery SET execution_id=NULL WHERE execution_id IN (SELECT id FROM Execution WHERE state<>'active') AND id NOT IN (SELECT delivery_id FROM Ack)`)
	return err
}
func (t *transaction) event(agent, ex, kind, object string) error {
	_, err := t.exec("INSERT INTO Event(kind,agent_id,execution_id,object_id,created_ms) VALUES(?,NULLIF(?,''),NULLIF(?,''),?,?)", kind, agent, ex, object, t.s.clock.Now().UnixMilli())
	if err == nil {
		t.notify = true
	}
	return err
}
func (t *transaction) repoEvent(repo, kind, object string) error {
	_, err := t.exec(`INSERT INTO Event(kind,agent_id,object_id,created_ms) SELECT ?,id,?,? FROM Agent WHERE repository_id=?`, kind, object, t.s.clock.Now().UnixMilli(), repo)
	if err == nil {
		t.notify = true
	}
	return err
}
func (t *transaction) cached(e execution, key, route string, request any) (map[string]any, bool, error) {
	if key == "" {
		return nil, false, fail("bad_request", "a retry key is required")
	}
	raw, _ := json.Marshal(struct {
		Route   string
		Request any
	}{route, request})
	hash := sha256.Sum256(raw)
	var old []byte
	var result string
	err := t.row("SELECT request_hash,result_json FROM Operation WHERE principal_agent_id=? AND key=?", e.AgentID, key).Scan(&old, &result)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if hex.EncodeToString(old) != hex.EncodeToString(hash[:]) {
		return nil, false, fail("key_reused", "retry key was used for a different request")
	}
	var out map[string]any
	decoder := json.NewDecoder(strings.NewReader(result))
	decoder.UseNumber()
	err = decoder.Decode(&out)
	return out, true, err
}
func (t *transaction) remember(e execution, key, route string, request any, result any) error {
	raw, _ := json.Marshal(struct {
		Route   string
		Request any
	}{route, request})
	hash := sha256.Sum256(raw)
	body, _ := json.Marshal(result) // results are hub-owned JSON maps, never client metadata
	_, err := t.exec("INSERT INTO Operation VALUES(?,?,?,?)", e.AgentID, key, hash[:], string(body))
	return err
}

func createDatabaseFile(filename string) error {
	file, err := os.OpenFile(filename, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	return file.Close()
}

func resolveDatabasePath(filename string, evalLinks func(string) (string, error)) (string, error) {
	abs, err := filepath.Abs(filename)
	if err != nil {
		return "", err
	}
	if err = os.MkdirAll(filepath.Dir(abs), 0700); err != nil {
		return "", err
	}
	if real, evalErr := evalLinks(abs); evalErr == nil {
		abs = real
	} else if !errors.Is(evalErr, os.ErrNotExist) {
		return "", evalErr
	} else {
		parent, evalErr := evalLinks(filepath.Dir(abs))
		if evalErr != nil {
			return "", evalErr
		}
		abs = filepath.Join(parent, filepath.Base(abs))
	}
	return abs, nil
}
