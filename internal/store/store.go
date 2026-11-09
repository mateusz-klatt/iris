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

//go:embed migrations/006.sql
var pathKeySchema string

//go:embed migrations/007.sql
var waiterSchema string

//go:embed migrations/008.sql
var attentionSchema string

//go:embed migrations/009.sql
var contactSchema string

//go:embed migrations/010.sql
var watchSeenSchema string

//go:embed migrations/011.sql
var receiptsSchema string

//go:embed migrations/012.sql
var hookSeenSchema string

//go:embed migrations/013.sql
var retentionSchema string

//go:embed migrations/014.sql
var hostReportSchema string

//go:embed migrations/015.sql
var worktreeAliasSchema string

//go:embed migrations/016.sql
var receiptDueSchema string

//go:embed migrations/017.sql
var consoleAccountsSchema string

//go:embed migrations/018.sql
var consoleRevisionSchema string

//go:embed migrations/019.sql
var consoleOwnerActionsSchema string

//go:embed migrations/020.sql
var consoleAssignmentImportSchema string

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

// missing is a lookup's answer when it finds nothing: not_found for sql.ErrNoRows, and any other
// error as it is. A storage failure is not an unknown id: it must fail the transaction, so the
// client retries, instead of reaching the client as a final not_found.
func missing(err error, message string) error {
	return absent(err, "not_found", message)
}

// absent maps an empty lookup to its domain refusal while preserving storage failures.
func absent(err error, code, message string) error {
	if errors.Is(err, sql.ErrNoRows) {
		return fail(code, message)
	}
	return err
}

type Store struct {
	mu      sync.Mutex
	db      *sql.DB
	lock    *os.File
	clock   Clock
	epoch   int64
	stream  string
	changed chan struct{}
	// cross counts cross-repository recipient attempts per credential, per minute. It is
	// guarded by mu, which every transaction holds, and lost on restart, which is fine
	// for a rate.
	cross map[string]crossWindow
}
type transaction struct {
	// console is set only after transactional account/session authorization.
	console *ConsoleAccount
	conn    *sql.Conn
	ctx     context.Context
	s       *Store
	notify  bool
	// refused is a refusal whose side effects must still commit, such as the
	// waiter a conflicting claim leaves. Call returns it after the commit.
	refused *Error
	// A diagnostic becomes observable only after its replacement generation commits.
	generationChange *GenerationChange
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
	abs, err := resolveDatabasePath(filename, filepath.Abs, filepath.EvalSymlinks)
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
	// temp_store=MEMORY: the hub's image is FROM scratch, with no temporary directory, and a large
	// delete's statement journal or a sort would otherwise need one. A sweep of 17,153 events made
	// the hub crash-loop with IOERR_GETTEMPPATH before this (ADR 006's first deploy).
	for _, q := range []string{"PRAGMA foreign_keys=ON", "PRAGMA journal_mode=WAL", "PRAGMA synchronous=FULL", "PRAGMA busy_timeout=5000", "PRAGMA temp_store=MEMORY"} {
		if _, err = db.Exec(q); err != nil {
			_ = s.Close()
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
		if version < 6 {
			if _, err := t.exec(pathKeySchema); err != nil {
				return err
			}
		}
		if version < 7 {
			if _, err := t.exec(waiterSchema); err != nil {
				return err
			}
		}
		if version < 8 {
			if _, err := t.exec(attentionSchema); err != nil {
				return err
			}
		}
		if version < 9 {
			if _, err := t.exec(contactSchema); err != nil {
				return err
			}
		}
		if version < 10 {
			if _, err := t.exec(watchSeenSchema); err != nil {
				return err
			}
		}
		if version < 11 {
			if _, err := t.exec(receiptsSchema); err != nil {
				return err
			}
		}
		if version < 12 {
			if _, err := t.exec(hookSeenSchema); err != nil {
				return err
			}
		}
		if version < 13 {
			if _, err := t.exec(retentionSchema); err != nil {
				return err
			}
		}
		if version < 14 {
			if _, err := t.exec(hostReportSchema); err != nil {
				return err
			}
		}
		if version < 15 {
			if _, err := t.exec(worktreeAliasSchema); err != nil {
				return err
			}
		}
		if version < 16 {
			if _, err := t.exec(receiptDueSchema); err != nil {
				return err
			}
		}
		if version < 17 {
			if _, err := t.exec(consoleAccountsSchema); err != nil {
				return err
			}
		}
		if version < 18 {
			if _, err := t.exec(consoleRevisionSchema); err != nil {
				return err
			}
		}
		if version < 19 {
			if _, err := t.exec(consoleOwnerActionsSchema); err != nil {
				return err
			}
		}
		if version < 20 {
			if _, err := t.exec(consoleAssignmentImportSchema); err != nil {
				return err
			}
		}
		if err := t.fillMissingKeys(); err != nil {
			return err
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
		// A recovering Execution keeps the deliveries it fetched, so a restart wakes no one for
		// mail already read. One that does not recover loses them in expire, which requeues them.
		// An acknowledgement from before the crash still cannot settle a later fetch: a fetch
		// raises the attempt, and an acknowledgement must name the current one.
		return t.expire()
	})
	if err != nil {
		_ = s.Close()
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
	defer func() { _ = c.Close() }()
	// Cancellation can be reported after SQLite has executed BEGIN. Install cleanup
	// before starting it, so that error cannot leak an open transaction into the pool.
	defer func() { _, _ = c.ExecContext(context.Background(), "ROLLBACK") }() // also covers cancellation and panics
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
	res, err := t.exec(`UPDATE Execution SET state='lost' WHERE state='active' AND
 ((recovery_deadline IS NOT NULL AND recovery_deadline<=?) OR
 (recovery_deadline IS NULL AND (boot_epoch<>? OR deadline_tick_ms<=?)))`, now, t.s.epoch, tick)
	if err != nil {
		return err
	}
	// A lost session changes what the console shows even when it held no claims, so it moves
	// the event cursor the console refreshes on. Its claims, if any, are announced below.
	if n, _ := res.RowsAffected(); n > 0 {
		if err = t.event("", "", "changed", ""); err != nil {
			return err
		}
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
	_ = rows.Close()
	if err != nil {
		return err
	}
	for _, x := range old {
		if _, err = t.exec("UPDATE Claim SET revoked_ms=? WHERE fence=?", now, x.f); err != nil {
			return err
		}
		if err = t.released(x.r, fmt.Sprint(x.f)); err != nil {
			return err
		}
	}
	if err = t.requeue(); err != nil {
		return err
	}
	return t.wakeMissedReceipts(now)
}

// wakeMissedReceipts wakes, once, each delivery whose fetch asked for a receipt that has not come
// by its due time (ADR 010): the response was likely lost, and the mail is still the fetcher's.
// A delivery acked or fetched again since is not woken for the older fetch: an ack settles it,
// and a newer fetch has set a new due time.
func (t *transaction) wakeMissedReceipts(now int64) error {
	rows, err := t.rows("SELECT id,recipient_agent_id FROM Delivery WHERE receipt_due_ms IS NOT NULL AND receipt_due_ms<=? AND id NOT IN (SELECT delivery_id FROM Ack)", now)
	if err != nil {
		return err
	}
	type missed struct{ id, agent string }
	var all []missed
	for rows.Next() {
		var m missed
		rows.Scan(&m.id, &m.agent)
		all = append(all, m)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return err
	}
	for _, m := range all {
		if _, err = t.exec("UPDATE Delivery SET receipt_due_ms=NULL WHERE id=?", m.id); err != nil {
			return err
		}
		if err = t.event(m.agent, "", "inbox_changed", m.id); err != nil {
			return err
		}
	}
	return nil
}

// requeue frees the unacknowledged deliveries of Executions that are no longer active, and
// wakes their recipient: the mail was fetched, but whoever fetched it is gone, so nobody may
// have read it. Each is announced once, because a freed delivery no longer matches.
func (t *transaction) requeue() error {
	rows, err := t.rows(`SELECT id,recipient_agent_id FROM Delivery WHERE execution_id IN (SELECT id FROM Execution WHERE state<>'active') AND id NOT IN (SELECT delivery_id FROM Ack)`)
	if err != nil {
		return err
	}
	type freed struct{ id, agent string }
	var all []freed
	for rows.Next() {
		var f freed
		rows.Scan(&f.id, &f.agent)
		all = append(all, f)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return err
	}
	for _, f := range all {
		if _, err = t.exec("UPDATE Delivery SET execution_id=NULL,attention=1,receipt_due_ms=NULL WHERE id=?", f.id); err != nil {
			return err
		}
		if err = t.event(f.agent, "", "inbox_changed", f.id); err != nil {
			return err
		}
	}
	return nil
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
	_, err := t.exec("INSERT INTO Operation(principal_agent_id,key,request_hash,result_json,created_ms) VALUES(?,?,?,?,?)", e.AgentID, key, hash[:], string(body), t.s.clock.Now().UnixMilli())
	return err
}

func createDatabaseFile(filename string) error {
	file, err := os.OpenFile(filename, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	return file.Close()
}

func resolveDatabasePath(filename string, absolute, evalLinks func(string) (string, error)) (string, error) {
	abs, err := absolute(filename)
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
