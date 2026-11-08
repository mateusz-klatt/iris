// Command oldhub writes a hub database as an Iris from before ADR 006 left it, with a backlog
// of old events, for CI to start the release image on. The first deploy of ADR 006 met exactly
// such a database: its first retention sweep deleted 17,153 events at once, needed temporary
// storage, and the image, which has no temporary directory, crash-looped.
//
// Usage: go run ./.github/oldhub <database path>
package main

import (
	"database/sql"
	"fmt"
	"io"
	"os"

	"github.com/mateusz-klatt/iris/internal/store"
	_ "modernc.org/sqlite"
)

// events is a backlog larger than the one production had.
const events = 20000

// downgrade turns a new database into one of version 12 with a backlog: the old events first,
// then the schema before retention (013) and per-host reports (014), so the hub runs both
// migrations and then its first sweep.
var downgrade = []string{
	fmt.Sprintf(`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i<%d)
INSERT INTO Event(kind,agent_id,execution_id,object_id,created_ms) SELECT 'changed',NULL,NULL,'old-'||i,0 FROM n`, events),
	"ALTER TABLE Operation DROP COLUMN created_ms",
	"DROP TABLE HostReport",
	"CREATE TABLE DoctorReport(id INTEGER PRIMARY KEY CHECK(id=1),report TEXT NOT NULL)",
	"PRAGMA user_version=12",
}

func main() { os.Exit(run(os.Args[1:], os.Stderr)) }

func run(args []string, out io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(out, "usage: oldhub <database path>")
		return 2
	}
	if err := write(args[0]); err != nil {
		fmt.Fprintln(out, "oldhub:", err)
		return 1
	}
	return 0
}

func write(path string) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("%s exists; oldhub writes only a new database", path)
	}
	s, err := store.Open(path, nil)
	if err != nil {
		return err
	}
	_ = s.Close()                     // a store that opened closes; a failure would show in the next open
	db, _ := sql.Open("sqlite", path) // the driver only records the name; Exec reports errors
	defer func() { _ = db.Close() }()
	for _, q := range downgrade {
		if _, err := db.Exec(q); err != nil {
			return fmt.Errorf("downgrade: %w", err)
		}
	}
	return nil
}
