package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// restoreMarkerTable is the restore marker's table, as migration 025 creates it. A backup of a
// database from before 025 creates it in the copy, so the copy carries the marker whatever its
// schema version (ADR 006).
const restoreMarkerTable = `CREATE TABLE IF NOT EXISTS RestoreMarker (
 id INTEGER PRIMARY KEY CHECK(id=1), made_ms INTEGER NOT NULL
)`

// execer is what the marker is written through: a database, or one connection of it.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// MarkRestore sets the restore marker in db, a hub database that no hub has open, such as a
// backup's copy before it is published (ADR 006). The first start on a file that carries the
// marker runs on a new stream and consumes it. Setting it again leaves one row.
func MarkRestore(ctx context.Context, db *sql.DB, made time.Time) error {
	return markRestore(ctx, db, made)
}

func markRestore(ctx context.Context, db execer, made time.Time) error {
	if _, err := db.ExecContext(ctx, restoreMarkerTable); err != nil {
		return err
	}
	_, err := db.ExecContext(ctx, "INSERT OR REPLACE INTO RestoreMarker VALUES(1,?)", made.UnixMilli())
	return err
}

// errNotAHub is MarkRestored's answer for an SQLite database without a hub's Meta row.
var errNotAHub = errors.New("not an Iris hub database")

// errNotInFile is MarkRestored's answer when the marker is committed but its checkpoint into the
// database file did not complete, as when another process reads the file. The marker can then be
// only in the -wal file beside it, which a copy of the database file alone would lose.
var errNotInFile = errors.New("the restore marker is set, but it may not be in the database file itself yet, only in the -wal file beside it; close any other process that has the database open, then run mark-restored again before copying the file")

// MarkRestored sets the restore marker in the hub database at filename, as `iris admin backup`
// sets it in its copy, so that the next hub start on the file runs on a new stream (ADR 006). It
// is for a copy made some other way, which otherwise keeps its stream. The file must exist and be
// a hub database. The hub's lock on the path Open resolves is held for the whole operation, so a
// database that a hub has open under that path, or under a link to it, is refused; a hard link or
// another mount of the file is a path of its own, which the lock does not see. No Store is opened:
// no epoch, recovery, expiry or migration runs, and a marker already set stays one row.
func MarkRestored(ctx context.Context, filename string, made time.Time) error {
	return markRestored(ctx, filename, made, "sqlite", filepath.Abs, filepath.EvalSymlinks)
}

// markRestored lets tests give the driver and the path functions that Open uses.
func markRestored(ctx context.Context, filename string, made time.Time, driverName string, absolute, evalLinks func(string) (string, error)) error {
	// The file must exist first, so that a mistyped path creates no directory and no lock file.
	info, err := os.Stat(filename)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", filename)
	}
	abs, err := resolveDatabasePath(filename, absolute, evalLinks)
	if err != nil {
		return err
	}
	lock, err := lockDatabase(abs + ".lock")
	if err != nil {
		return err
	}
	defer unlockDatabase(lock)
	db, _ := sql.Open(driverName, abs) // sql.Open only records the name of a registered driver
	defer func() { _ = db.Close() }()
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	// IMMEDIATE takes SQLite's write lock at once, so a process outside Iris that is writing the
	// file makes this fail instead of waiting.
	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()
	var hubs int
	if err = conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM Meta WHERE id=1").Scan(&hubs); err != nil {
		return fmt.Errorf("%w, or it cannot be read: %w", errNotAHub, err)
	}
	if hubs != 1 {
		return errNotAHub
	}
	if err = markRestore(ctx, conn, made); err != nil {
		return err
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return err
	}
	committed = true
	// A copy of the database file alone must carry the marker, so it is checkpointed into the file
	// now, under the lock. Closing the last connection would checkpoint too, but a reader outside
	// Iris that keeps the file open stops that, and the close reports nothing. A file not in WAL
	// mode answers 0, -1, -1.
	var busy, logged, moved int
	if err = conn.QueryRowContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &logged, &moved); err != nil {
		return fmt.Errorf("%w: %w", errNotInFile, err)
	}
	if busy != 0 || logged != moved {
		return errNotInFile
	}
	return errors.Join(conn.Close(), db.Close())
}
