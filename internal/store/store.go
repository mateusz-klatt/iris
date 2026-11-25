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
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite"
	"modernc.org/sqlite/vfs"
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

//go:embed migrations/021.sql
var idleExpirySchema string

//go:embed migrations/022.sql
var reducedShellSchema string

//go:embed migrations/023.sql
var attentionIdentitySchema string

//go:embed migrations/024.sql
var removeConsoleImportSchema string

//go:embed migrations/025.sql
var restoreMarkerSchema string

//go:embed migrations/026.sql
var presenceCursorSchema string

//go:embed migrations/027.sql
var coordinationCursorSchema string

//go:embed migrations/028.sql
var leaseRenewedTickSchema string

//go:embed migrations/029.sql
var successionProvenanceSchema string

//go:embed migrations/030.sql
var processHookIndexSchema string

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
	mu     sync.Mutex
	db     *sql.DB
	readDB *sql.DB
	// reads is cancelled when Close begins. readMu guards readsClosed and admission
	// onto readWG. Close waits out the reads it cancelled before it closes either
	// pool or releases the database lock: sql.DB.Close does not wait for a Conn
	// that readTransact has checked out, and those reads do not hold Store.mu.
	reads       context.Context
	stopReads   context.CancelFunc
	readMu      sync.Mutex
	readWG      sync.WaitGroup
	readsClosed bool
	lock        *os.File
	clock       Clock
	epoch       int64
	stream      string
	changed     chan struct{}
	// restored says this start consumed a restore marker and so runs on a new stream (ADR 006).
	restored bool
	// transactions is process-local diagnostic state, never part of a durable cursor.
	transactions atomic.Uint64
	// cross counts cross-repository recipient attempts per credential, per minute. It is
	// guarded by mu, which every transaction holds, and lost on restart, which is fine
	// for a rate.
	cross map[string]crossWindow
	// sweepArmed tracks whether the hub's stream expiry sweep is armed.
	// lastExpireTick holds the monotonic tick of the last committed expire or arming.
	sweepArmed     bool
	lastExpireTick int64
}
type transaction struct {
	// console is set only after transactional account/session authorization.
	console *ConsoleAccount
	conn    *sql.Conn
	ctx     context.Context
	s       *Store
	notify  bool
	// expireTick records the monotonic tick of expire calls in this transaction,
	// published to Store.lastExpireTick only after COMMIT.
	expireTick    int64
	hasExpireTick bool
	// refused is a refusal whose side effects must still commit, such as the
	// waiter a conflicting claim leaves. Call returns it after the commit.
	refused *Error
	// A diagnostic becomes observable only after its replacement generation commits.
	generationChange *GenerationChange
	// A combined shell registration can fail after it inserts an Execution.
	insertedExecution string
	// replayed is set when t.succeed returns an already-committed G+1 Execution.
	replayed bool
}

// exec runs a write that streams can observe. A commit in which an exec changed a row
// wakes every connected stream: the hub coalesces commits for 10 ms from the first change
// into one wake, and each woken stream reads the store in its own transaction. Even a
// heartbeat of another Execution can make every connected stream read again (ADR 016).
// execQuiet keeps cursor and bearer-use bookkeeping from waking streams.
func (t *transaction) exec(q string, a ...any) (sql.Result, error) {
	result, err := t.execQuiet(q, a...)
	if err == nil {
		// SQLite reports actual affected rows. Publish only after the surrounding
		// transaction commits, including writes whose scoped revision is trigger-driven.
		if n, _ := result.RowsAffected(); n > 0 {
			t.notify = true
		}
	}
	return result, err
}

// execQuiet persists bookkeeping that cannot change a stream's projection.
// Consumer cursors and bearer-use counters must not create observer feedback.
func (t *transaction) execQuiet(q string, a ...any) (sql.Result, error) {
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
	// Probe reverted ADR 037 databases through a read-only VFS before opening a writable
	// handle, so pending -wal frames are read without touching db, -wal, or -shm on refusal.
	if err = probeRevertedADR037Schema(context.Background(), abs, driverName, newReadOnlyVFS); err != nil {
		unlockDatabase(lock)
		return nil, err
	}
	db, err := openSQLite(driverName, abs, 1, "PRAGMA foreign_keys=ON", "PRAGMA synchronous=FULL", "PRAGMA busy_timeout=5000", "PRAGMA temp_store=MEMORY")
	if err != nil {
		unlockDatabase(lock)
		return nil, err
	}
	reads, stopReads := context.WithCancel(context.Background())
	s := &Store{db: db, lock: lock, clock: clock, changed: make(chan struct{}), reads: reads, stopReads: stopReads}
	if _, err = db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		_ = s.Close()
		return nil, err
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
		if version < 21 {
			if _, err := t.exec(idleExpirySchema); err != nil {
				return err
			}
		}
		if version < 22 {
			if _, err := t.exec(reducedShellSchema); err != nil {
				return err
			}
		}
		if version < 23 {
			for _, q := range []string{attentionIdentitySchema, attentionHighWaterSQL, attentionNewestEventSQL} {
				if _, err := t.exec(q); err != nil {
					return err
				}
			}
		}
		if version < 24 {
			if _, err := t.exec(removeConsoleImportSchema); err != nil {
				return err
			}
		}
		if version < 25 {
			if _, err := t.exec(restoreMarkerSchema); err != nil {
				return err
			}
		}
		if version < 26 {
			if _, err := t.exec(presenceCursorSchema); err != nil {
				return err
			}
		}
		if version < 27 {
			if _, err := t.exec(coordinationCursorSchema); err != nil {
				return err
			}
		}
		if version < 28 {
			if _, err := t.exec(leaseRenewedTickSchema); err != nil {
				return err
			}
		}
		if version < 29 {
			if _, err := t.exec(successionProvenanceSchema); err != nil {
				return err
			}
		}
		if version < 30 {
			if _, err := t.exec(processHookIndexSchema); err != nil {
				return err
			}
		}
		// No shell baseline crosses a hub start. An older hub that ran in between could not keep
		// a reduced one current (ADR 018), and one writes full ones without naming the checkout
		// they came from, so they may be another checkout's (ADR 033). The first snapshot after a
		// start only sets a baseline.
		for _, table := range []string{"ReducedShellSnapshot", "ShellSnapshot"} {
			if _, err := t.exec("DELETE FROM " + table); err != nil {
				return err
			}
		}
		if err := t.fillMissingKeys(); err != nil {
			return err
		}
		if _, err := t.exec("INSERT OR IGNORE INTO Meta VALUES(1,?,0)", newStreamID()); err != nil {
			return err
		}
		// A copy made by `iris admin backup` carries a restore marker (ADR 006). Its first start
		// runs on a new stream: a client that remembers a cursor of the copied stream would
		// otherwise take attention committed after the restore, at or below that cursor, for
		// attention it was already told. The marker goes in the same transaction, so a start
		// that fails keeps it for the next one.
		var marked int
		if err := t.row("SELECT COUNT(*) FROM RestoreMarker").Scan(&marked); err != nil {
			return err
		}
		s.restored = marked > 0
		if s.restored {
			if _, err := t.exec("UPDATE Meta SET stream_id=? WHERE id=1", newStreamID()); err != nil {
				return err
			}
			if _, err := t.exec("DELETE FROM RestoreMarker"); err != nil {
				return err
			}
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
	// Console reads share a dedicated query_only pool outside Store.mu so slow snapshots never
	// delay heartbeats, claims, or lease expiry on the single serialized writer connection.
	readDB, err := openSQLite(driverName, readOnlyDSN(abs), 4, "PRAGMA foreign_keys=ON", "PRAGMA query_only=ON", "PRAGMA busy_timeout=5000", "PRAGMA temp_store=MEMORY")
	if err != nil {
		_ = s.Close()
		return nil, err
	}
	s.readDB = readDB
	return s, nil
}
func readOnlyDSN(abs string) string {
	return "file:" + (&url.URL{Path: filepath.ToSlash(abs)}).EscapedPath() + "?_pragma=foreign_keys(ON)&_pragma=query_only(ON)&_pragma=busy_timeout(5000)&_pragma=temp_store(2)"
}

// temp_store=MEMORY: the hub's image is FROM scratch, with no temporary directory, and a large
// delete's statement journal or a sort would otherwise need one. A sweep of 17,153 events made
// the hub crash-loop with IOERR_GETTEMPPATH before this (ADR 006's first deploy).
func openSQLite(driverName, dsn string, maxConns int, pragmas ...string) (*sql.DB, error) {
	db, err := sql.Open(driverName, dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(maxConns)
	for _, q := range pragmas {
		if _, err = db.Exec(q); err != nil {
			_ = db.Close()
			return nil, err
		}
	}
	return db, nil
}

func newReadOnlyVFS(fsys fs.FS) (string, io.Closer, error) {
	return vfs.New(fsys)
}

func probeRevertedADR037Schema(ctx context.Context, abs, driverName string, newVFS func(fs.FS) (string, io.Closer, error)) error {
	dsn := "file:" + (&url.URL{Path: filepath.ToSlash(abs)}).EscapedPath() + "?immutable=1&_pragma=query_only(ON)&_pragma=busy_timeout(5000)&_pragma=temp_store(2)"
	if _, err := os.Stat(abs + "-wal"); err == nil {
		id, fsCloser, err := newVFS(os.DirFS(filepath.Dir(abs)))
		if err != nil {
			return err
		}
		defer func() { _ = fsCloser.Close() }()
		dsn = "file:" + (&url.URL{Path: filepath.Base(abs)}).EscapedPath() + "?vfs=" + id + "&_pragma=locking_mode(EXCLUSIVE)&_pragma=query_only(ON)&_pragma=busy_timeout(5000)&_pragma=temp_store(2)"
	}
	db, err := openSQLite(driverName, dsn, 1)
	if err != nil {
		return err
	}
	err = rejectRevertedADR037Schema(ctx, db)
	_ = db.Close()
	return err
}

// rejectRevertedADR037Schema refuses databases from the reverted ADR 037 branch before any writes.
func rejectRevertedADR037Schema(ctx context.Context, db *sql.DB) error {
	var version int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	var worktreeIdentity, autoClaimScope, priorExecutionID int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM sqlite_schema WHERE type='table' AND name='WorktreeIdentity'").Scan(&worktreeIdentity); err != nil {
		return err
	}
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM pragma_table_info('Repository') WHERE name='auto_claim_scope'").Scan(&autoClaimScope); err != nil {
		return err
	}
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM pragma_table_info('Execution') WHERE name='prior_execution_id'").Scan(&priorExecutionID); err != nil {
		return err
	}
	if worktreeIdentity > 0 || autoClaimScope > 0 || ((version == 29 || version == 30) && priorExecutionID == 0) {
		return fmt.Errorf("cannot open database with reverted ADR 037 schema (user_version=%d); restore the pre-ADR-037 backup", version)
	}
	return nil
}

// errStoreClosed is what a console read receives once Close has begun. The
// database lock is still held until every read that started earlier returns.
var errStoreClosed = errors.New("store closed")

func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Admit no further console read, then cancel the ones already in readTransact.
	// The flag and the WaitGroup share readMu, so a read cannot pass the gate
	// after Close has decided to wait and then be missed by that wait.
	s.readMu.Lock()
	s.readsClosed = true
	s.readMu.Unlock()
	// Historical fixtures build a Store with no read pool, so there is no cancel.
	if s.stopReads != nil {
		s.stopReads()
	}
	// sql.DB.Close does not wait for a Conn readTransact has checked out, and
	// those reads do not hold Store.mu. Wait while still holding mu, so no writer
	// begins, then close the pools and only then drop the database lock.
	s.readWG.Wait()
	var readErr error
	if s.readDB != nil {
		readErr = s.readDB.Close()
	}
	err := s.db.Close()
	unlockDatabase(s.lock)
	s.lock = nil
	return errors.Join(readErr, err)
}

func (s *Store) beginConsoleRead() error {
	s.readMu.Lock()
	defer s.readMu.Unlock()
	if s.readsClosed {
		return errStoreClosed
	}
	s.readWG.Add(1)
	return nil
}

func (s *Store) endConsoleRead() { s.readWG.Done() }

func (s *Store) Changes() <-chan struct{} { s.mu.Lock(); defer s.mu.Unlock(); return s.changed }

// SetStreamSweep records whether the hub's stream expiry sweep is armed.
// Arming snapshots the monotonic tick so an earlier idle gap is not credited.
func (s *Store) SetStreamSweep(armed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if armed && !s.sweepArmed {
		s.lastExpireTick = s.clock.Tick()
	}
	s.sweepArmed = armed
}

// Restored reports whether this start consumed a restore marker, and so runs on a new stream
// (ADR 006). It never changes after Open.
func (s *Store) Restored() bool { return s.restored }

// TransactionCount reports how many transactions successfully began in this Store.
// It includes transactions that later rolled back and resets when the Store is reopened.
func (s *Store) TransactionCount() uint64 { return s.transactions.Load() }

type lockObserver struct {
	beforeLock  func()
	afterUnlock func(wait, held time.Duration)
}

type lockObserverKey struct{}

func withLockObserver(ctx context.Context, observer lockObserver) context.Context {
	return context.WithValue(ctx, lockObserverKey{}, observer)
}

func (s *Store) transact(ctx context.Context, f func(*transaction) error) error {
	observer, hasObserver := ctx.Value(lockObserverKey{}).(lockObserver)
	var wait time.Duration
	var acquired time.Time
	if hasObserver {
		if observer.beforeLock != nil {
			observer.beforeLock()
		}
		requested := time.Now()
		s.mu.Lock()
		acquired = time.Now()
		wait = acquired.Sub(requested)
	} else {
		s.mu.Lock()
	}
	defer func() {
		var held time.Duration
		if hasObserver {
			held = time.Since(acquired)
		}
		s.mu.Unlock()
		if hasObserver {
			observer.afterUnlock(wait, held)
		}
	}()
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
	s.transactions.Add(1)
	tx := &transaction{conn: c, ctx: ctx, s: s}
	if err = f(tx); err != nil {
		return err
	}
	if _, err = c.ExecContext(ctx, "COMMIT"); err != nil {
		return err
	}
	if tx.hasExpireTick {
		s.lastExpireTick = tx.expireTick
	}
	if tx.notify {
		close(s.changed)
		s.changed = make(chan struct{})
	}
	return nil
}

func (s *Store) readTransact(ctx context.Context, f func(*transaction) error) error {
	if err := s.beginConsoleRead(); err != nil {
		return err
	}
	defer s.endConsoleRead()
	// The caller's deadline and Close both end the snapshot. Close waits until
	// this returns, so a read cannot still hold SQLite after the lock is released.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(s.reads, cancel)
	defer stop()
	c, err := s.readDB.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	// Cancellation can be reported after SQLite has executed BEGIN DEFERRED. Install cleanup
	// before starting it, so an aborted call never leaves a read snapshot pinned on the connection.
	defer func() { _, _ = c.ExecContext(context.Background(), "ROLLBACK") }()
	if _, err = c.ExecContext(ctx, "BEGIN DEFERRED"); err != nil {
		return err
	}
	s.transactions.Add(1)
	tx := &transaction{conn: c, ctx: ctx, s: s}
	if err = f(tx); err != nil {
		return err
	}
	_, err = c.ExecContext(ctx, "COMMIT")
	return err
}

const (
	stallThresholdMS = 5000
	stallStepMS      = 20000
	stallCapMS       = 60000
)

// stallCapSQL returns the SQL expression for an Execution's stall-credit deadline ceiling.
func stallCapSQL(prefix string) string {
	return prefix + `lease_renewed_tick_ms+CASE WHEN ` + prefix + `heartbeat_mode<>0 THEN 60000 ELSE 900000 END+?`
}

// stallCreditWhereSQL returns the shared SQL predicate identifying non-recovering active
// Executions in the current epoch whose deadline fell due in (prev, tick] and remains below cap.
func stallCreditWhereSQL(prefix string) string {
	return prefix + `recovery_deadline IS NULL AND ` + prefix + `boot_epoch=? AND ` +
		prefix + `deadline_tick_ms>? AND ` + prefix + `deadline_tick_ms<=? AND ` +
		stallCapSQL(prefix) + `>?`
}

var creditStallSQL = `UPDATE Execution SET deadline_tick_ms=MIN(?+?,` + stallCapSQL("") + `) WHERE state='active' AND ` + stallCreditWhereSQL("")

// stallCreditGap reports the previous committed expire tick in this transaction or store
// and whether a monotonic gap past 5 s with the stream sweep armed qualifies as a hub stall.
func (t *transaction) stallCreditGap(tick int64) (prev int64, stalled bool) {
	prev = t.s.lastExpireTick
	if t.hasExpireTick {
		prev = t.expireTick
	}
	return prev, t.s.sweepArmed && tick-prev > stallThresholdMS
}

// stallCreditArgs returns the bound parameters matching stallCreditWhereSQL.
func (t *transaction) stallCreditArgs(prev, tick int64) []any {
	return []any{t.s.epoch, prev, tick, stallCapMS, tick}
}

const expireExecutionsSQL = `UPDATE Execution SET state='lost' WHERE state='active' AND
 ((recovery_deadline IS NOT NULL AND recovery_deadline<=?) OR
 (recovery_deadline IS NULL AND (boot_epoch<>? OR deadline_tick_ms<=?))) RETURNING id`

const expiredClaimsSQL = `SELECT c.fence,c.repository_id FROM Claim c JOIN Execution e ON e.id=c.execution_id
 WHERE c.revoked_ms IS NULL AND (e.state<>'active' OR (c.expires_ms IS NOT NULL AND c.expires_ms<=?))`

const missedReceiptsSQL = `SELECT id,recipient_agent_id FROM Delivery WHERE receipt_due_ms IS NOT NULL AND receipt_due_ms<=? AND id NOT IN (SELECT delivery_id FROM Ack)`

const requeueExecutionSQL = `SELECT id,recipient_agent_id FROM Delivery WHERE execution_id=? AND NOT EXISTS (SELECT 1 FROM Ack WHERE delivery_id=Delivery.id)`

const expireConsoleSessionsSQL = `DELETE FROM ConsoleSession WHERE expires_ms<=?`

func (t *transaction) expire() error {
	now, tick := t.s.clock.Now().UnixMilli(), t.s.clock.Tick()
	if prev, stalled := t.stallCreditGap(tick); stalled {
		args := append([]any{tick, stallStepMS, stallCapMS}, t.stallCreditArgs(prev, tick)...)
		if _, err := t.execQuiet(creditStallSQL, args...); err != nil {
			return err
		}
	}
	t.expireTick, t.hasExpireTick = tick, true
	rows, err := t.rows(expireExecutionsSQL, now, t.s.epoch, tick)
	if err != nil {
		return err
	}
	var lost []string
	for rows.Next() {
		var ex string
		rows.Scan(&ex)
		lost = append(lost, ex)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return err
	}
	// A lost session changes what the console shows even when it held no claims, so it moves
	// the event cursor the console refreshes on. Its claims, if any, are announced below.
	// This one event, not one per Execution, is also how a stream learns that its
	// Execution is lost. The UPDATE above runs through rows, which does not notify.
	// The event wakes the stream; its next read fails the liveness check and closes
	// the socket.
	if len(lost) > 0 {
		if err = t.event("", "", "changed", ""); err != nil {
			return err
		}
	}
	rows, err = t.rows(expiredClaimsSQL, now)
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
	// Requeue only the Executions that changed state in this transaction. A scan
	// of every ended Execution would grow with history on every maintenance wake.
	for _, ex := range lost {
		if err = t.requeue(ex); err != nil {
			return err
		}
	}
	return t.wakeMissedReceipts(now)
}

// wakeMissedReceipts wakes, once, each delivery whose fetch asked for a receipt that has not come
// by its due time (ADR 010): the response was likely lost, and the mail is still the fetcher's.
// A delivery acked or fetched again since is not woken for the older fetch: an ack settles it,
// and a newer fetch has set a new due time.
func (t *transaction) wakeMissedReceipts(now int64) error {
	rows, err := t.rows(missedReceiptsSQL, now)
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

// requeue frees the unacknowledged deliveries of an Execution that just ended or was lost, and
// wakes their recipient: the mail was fetched, but whoever fetched it is gone, so nobody may
// have read it. Each is announced once, because a freed delivery no longer matches.
func (t *transaction) requeue(ex string) error {
	rows, err := t.rows(requeueExecutionSQL, ex)
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
