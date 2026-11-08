package main

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mateusz-klatt/iris/internal/store"
)

// The database oldhub writes is one the hub upgrades and then sweeps of its whole backlog, as
// the first deploy of ADR 006 met it.
func TestTheOldDatabaseMigratesAndSweeps(t *testing.T) {
	fromHere(t)
	path := filepath.Join(t.TempDir(), "iris.db")
	var out bytes.Buffer
	if code := run([]string{path}, &out); code != 0 {
		t.Fatalf("%d: %s", code, out.String())
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 12 {
		t.Fatalf("version %d: %v", version, err)
	}
	_ = db.Close()
	s, err := store.Open(path, nil)
	if err != nil {
		t.Fatalf("the hub could not upgrade it: %v", err)
	}
	defer func() { _ = s.Close() }()
	removed, err := s.Sweep(context.Background())
	if err != nil || removed["events"] < events {
		t.Fatalf("removed %v: %v", removed, err)
	}
}

// It refuses to write over a database, needs one path, and reports what failed.
func TestOldhubRefusesAndReports(t *testing.T) {
	fromHere(t)
	var out bytes.Buffer
	if code := run(nil, &out); code != 2 || !strings.Contains(out.String(), "usage") {
		t.Fatal(code, out.String())
	}
	existing := filepath.Join(t.TempDir(), "iris.db")
	if err := os.WriteFile(existing, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if code := run([]string{existing}, &out); code != 1 || !strings.Contains(out.String(), "exists") {
		t.Fatal(code, out.String())
	}
	out.Reset()
	if code := run([]string{filepath.Join(existing, "under-a-file.db")}, &out); code != 1 {
		t.Fatal("a database under a file was written", out.String())
	}
	broken := t.TempDir()
	for n := 1; n <= version; n++ {
		if err := os.WriteFile(filepath.Join(broken, fmt.Sprintf("%03d.sql", n)), []byte("NOT SQL"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	migrations = broken
	out.Reset()
	if code := run([]string{filepath.Join(t.TempDir(), "iris.db")}, &out); code != 1 || !strings.Contains(out.String(), "step 1:") {
		t.Fatal(code, out.String())
	}
	migrations = t.TempDir()
	out.Reset()
	if code := run([]string{filepath.Join(t.TempDir(), "iris.db")}, &out); code != 1 || !strings.Contains(out.String(), "001.sql") {
		t.Fatal("a missing migration was not reported", code, out.String())
	}
}

// fromHere points oldhub at the store's migrations from this package's directory, where go test
// runs it, and restores the path afterwards.
func fromHere(t *testing.T) {
	real := migrations
	migrations = filepath.Join("..", "..", "internal", "store", "migrations")
	t.Cleanup(func() { migrations = real })
}
