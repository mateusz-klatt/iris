package backup

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mateusz-klatt/iris/internal/store"
)

// markers counts the restore marker rows in the database at path, read-only.
func markers(t *testing.T, path string) int {
	t.Helper()
	db, _ := sql.Open("sqlite", readOnly(path))
	defer func() { _ = db.Close() }()
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM RestoreMarker").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// A backup's copy carries the restore marker and the live database never does (ADR 006). The
// published file alone, copied anywhere, is the whole backup: no journal or WAL beside it holds
// the marker, and a hub started on it runs on a new stream.
func TestABackupCarriesTheRestoreMarkerInTheFileAlone(t *testing.T) {
	src := live(t)
	dest := filepath.Join(t.TempDir(), "backup #%20.db")
	var out bytes.Buffer
	if err := Run(context.Background(), src, dest, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "restore marker set") {
		t.Fatalf("no marker in\n%s", out.String())
	}
	if markers(t, src) != 0 || markers(t, dest) != 1 {
		t.Fatalf("live %d, copy %d marker rows", markers(t, src), markers(t, dest))
	}
	for _, suffix := range []string{"-journal", "-wal", "-shm"} {
		if _, err := os.Stat(dest + suffix); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s beside the backup: %v", suffix, err)
		}
	}
	raw, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	restored := filepath.Join(t.TempDir(), "iris.db")
	if err := os.WriteFile(restored, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(restored, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if !s.Restored() {
		t.Fatal("a hub on the backup's bytes did not run on a new stream")
	}
}

// A backup of a database from before migration 025 creates the marker's table in the copy, so the
// copy carries the marker whatever its schema version.
func TestABackupOfAnOlderSchemaCarriesTheMarker(t *testing.T) {
	src := filepath.Join(t.TempDir(), "old.db")
	s, err := store.Open(src, nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	raw, _ := sql.Open("sqlite", src)
	for _, q := range []string{"DROP TABLE RestoreMarker", "PRAGMA user_version=24"} {
		if _, err := raw.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	_ = raw.Close()
	dest := filepath.Join(t.TempDir(), "backup.db")
	var out bytes.Buffer
	if err := Run(context.Background(), src, dest, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "schema 24,") || markers(t, dest) != 1 {
		t.Fatalf("copy has %d marker rows:\n%s", markers(t, dest), out.String())
	}
}

// A copy that cannot be marked is never published, and nothing of it is left behind.
func TestACopyThatCannotBeMarkedIsNotPublished(t *testing.T) {
	src := live(t)
	dest := filepath.Join(t.TempDir(), "backup.db")
	real := markCopy
	t.Cleanup(func() { markCopy = real })
	markCopy = func(_ context.Context, path string) error {
		// SQLite's rollback journal is the file a failed write can leave.
		if err := os.WriteFile(path+"-journal", nil, 0o600); err != nil {
			t.Fatal(err)
		}
		return errors.New("disk full")
	}
	var out bytes.Buffer
	if err := Run(context.Background(), src, dest, &out); err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("marking failed and Run said %v", err)
	}
	for _, path := range []string{dest, dest + ".partial", dest + ".partial-journal"} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s left behind: %v", path, err)
		}
	}
}

// The check before publishing refuses a copy without the marker, so no backup is published
// without it.
func TestTheCheckRefusesACopyWithoutTheMarker(t *testing.T) {
	src := filepath.Join(t.TempDir(), "unmarked.db")
	s, err := store.Open(src, nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	if _, err := summarise(src); err == nil {
		t.Fatal("a copy without the restore marker passed the check")
	}
}

// A marker that cannot be written is reported: here the copy is a file SQLite cannot write.
func TestMarkingAReadOnlyCopyFails(t *testing.T) {
	if err := markCopy(context.Background(), filepath.Join(t.TempDir(), "missing", "copy.db")); err == nil {
		t.Fatal("a copy that does not exist took the marker")
	}
}
