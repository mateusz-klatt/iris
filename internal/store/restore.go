package store

import (
	"context"
	"database/sql"
	"time"
)

// restoreMarkerTable is the restore marker's table, as migration 025 creates it. A backup of a
// database from before 025 creates it in the copy, so the copy carries the marker whatever its
// schema version (ADR 006).
const restoreMarkerTable = `CREATE TABLE IF NOT EXISTS RestoreMarker (
 id INTEGER PRIMARY KEY CHECK(id=1), made_ms INTEGER NOT NULL
)`

// MarkRestore sets the restore marker in db, a hub database that no hub has open, such as a
// backup's copy before it is published (ADR 006). The first start on a file that carries the
// marker runs on a new stream and consumes it. Setting it again leaves one row.
func MarkRestore(ctx context.Context, db *sql.DB, made time.Time) error {
	if _, err := db.ExecContext(ctx, restoreMarkerTable); err != nil {
		return err
	}
	_, err := db.ExecContext(ctx, "INSERT OR REPLACE INTO RestoreMarker VALUES(1,?)", made.UnixMilli())
	return err
}
