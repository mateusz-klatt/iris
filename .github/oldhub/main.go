// Command oldhub writes a hub database as an Iris from before ADR 006 left it, with a backlog
// of old events, for CI to start the release image on. The first deploy of ADR 006 met exactly
// such a database: its first retention sweep deleted 17,153 events at once, needed temporary
// storage, and the image, which has no temporary directory, crash-looped.
//
// It builds the schema from the store's own migrations 001 to 012, so a migration added later
// runs against it as it would against that production database.
//
// Usage, from the repository root: go run ./.github/oldhub <database path>
package main

import (
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

// version is the last migration before retention (013).
const version = 12

// events is a backlog larger than the one production had.
const events = 20000

// migrations is where the store keeps its migrations, from the repository root.
var migrations = filepath.Join("internal", "store", "migrations")

// backlog is the old events, inserted once the schema is at version 12.
var backlog = fmt.Sprintf(`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i<%d)
INSERT INTO Event(kind,agent_id,execution_id,object_id,created_ms) SELECT 'changed',NULL,NULL,'old-'||i,0 FROM n`, events)

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
	var steps []string
	for n := 1; n <= version; n++ {
		name := filepath.Join(migrations, fmt.Sprintf("%03d.sql", n))
		q, err := os.ReadFile(name)
		if err != nil {
			return err
		}
		steps = append(steps, string(q))
	}
	db, _ := sql.Open("sqlite", path) // the driver only records the name; Exec reports errors
	defer func() { _ = db.Close() }()
	for i, q := range append(steps, backlog) {
		if _, err := db.Exec(q); err != nil {
			return fmt.Errorf("step %d: %w", i+1, err)
		}
	}
	return nil
}
