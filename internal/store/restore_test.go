package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The restore marker (ADR 006). A database that carries it runs on a new stream at its next
// start, which consumes it; one without it keeps its stream, as every database did before.

// streamOf opens file as a hub does and returns the stream it runs on, whether that start
// consumed a marker, and how many marker rows remain after it.
func streamOf(t *testing.T, file string) (stream string, restored bool, left int) {
	t.Helper()
	s, err := Open(file, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if err := s.db.QueryRow("SELECT COUNT(*) FROM RestoreMarker").Scan(&left); err != nil {
		t.Fatal(err)
	}
	return s.stream, s.Restored(), left
}

// mark sets the marker in file with no hub open, as a backup does to its copy.
func mark(t *testing.T, file string) {
	t.Helper()
	raw, err := sql.Open("sqlite", file)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	if err := MarkRestore(context.Background(), raw, time.Unix(1800000000, 0)); err != nil {
		t.Fatal(err)
	}
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	raw, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// closedHub makes a hub database with its stream and closes it, as a stopped hub leaves it.
func closedHub(t *testing.T) (file, stream string) {
	t.Helper()
	file = filepath.Join(t.TempDir(), "hub.db")
	stream, _, _ = streamOf(t, file)
	return file, stream
}

func TestAMarkedDatabaseRunsOnANewStreamAtItsNextStartOnly(t *testing.T) {
	file, original := closedHub(t)
	mark(t, file)
	mark(t, file) // setting it again leaves one row
	rotated, restored, left := streamOf(t, file)
	if !restored || rotated == original || left != 0 {
		t.Fatalf("first start: stream %q (was %q), restored %v, %d marker rows left", rotated, original, restored, left)
	}
	if again, restored, _ := streamOf(t, file); restored || again != rotated {
		t.Fatalf("second start: stream %q (was %q), restored %v", again, rotated, restored)
	}
}

// Two restores of one backup are two histories, so each copy gets a stream of its own; a copy
// without the marker keeps the stream it was copied with.
func TestEveryCopyOfAMarkedFileGetsItsOwnStream(t *testing.T) {
	file, original := closedHub(t)
	plain := filepath.Join(t.TempDir(), "plain.db")
	copyFile(t, file, plain)
	if kept, restored, _ := streamOf(t, plain); restored || kept != original {
		t.Fatalf("an unmarked copy ran on %q, not %q (restored %v)", kept, original, restored)
	}
	mark(t, file)
	one, two := filepath.Join(t.TempDir(), "one.db"), filepath.Join(t.TempDir(), "two.db")
	copyFile(t, file, one)
	copyFile(t, file, two)
	first, _, _ := streamOf(t, one)
	second, _, _ := streamOf(t, two)
	if first == original || second == original || first == second {
		t.Fatalf("streams %q and %q from one backup of %q", first, second, original)
	}
}

// A backup of a schema-24 database creates the marker's table itself. Migration 025 then keeps
// its row, and the first start rotates exactly once.
func TestAMarkedSchema24CopyMigratesAndRotatesOnce(t *testing.T) {
	file, original := closedHub(t)
	raw, err := sql.Open("sqlite", file)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"DROP TABLE RestoreMarker", "PRAGMA user_version=24"} {
		if _, err := raw.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	_ = raw.Close()
	mark(t, file)
	rotated, restored, left := streamOf(t, file)
	if !restored || rotated == original || left != 0 {
		t.Fatalf("stream %q (was %q), restored %v, %d marker rows left", rotated, original, restored, left)
	}
	if again, restored, _ := streamOf(t, file); restored || again != rotated {
		t.Fatalf("second start: stream %q (was %q), restored %v", again, rotated, restored)
	}
	check, _ := sql.Open("sqlite", file)
	defer func() { _ = check.Close() }()
	var version int
	if err := check.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != latestVersion {
		t.Fatal(version, err)
	}
}

// Migration 025 creates the table exactly as a backup does, so a copy made from any schema
// version and a migrated database agree.
func TestMigration025CreatesTheMarkerTableAsABackupDoes(t *testing.T) {
	if !strings.Contains(restoreMarkerSchema, restoreMarkerTable) {
		t.Fatalf("025.sql does not contain %q", restoreMarkerTable)
	}
}

func TestMarkRestoreReportsWhatItCouldNotWrite(t *testing.T) {
	// A hub database opened read-only cannot take the table, as a pre-025 one needs; a different
	// table under that name cannot take the row.
	file, _ := closedHub(t)
	writable, _ := sql.Open("sqlite", file)
	if _, err := writable.Exec("DROP TABLE RestoreMarker"); err != nil {
		t.Fatal(err)
	}
	_ = writable.Close()
	readOnly, _ := sql.Open("sqlite", "file:"+filepath.ToSlash(file)+"?mode=ro")
	defer func() { _ = readOnly.Close() }()
	if err := MarkRestore(context.Background(), readOnly, time.Now()); err == nil {
		t.Fatal("a database that cannot be written took the marker")
	}
	odd := filepath.Join(t.TempDir(), "odd.db")
	raw, _ := sql.Open("sqlite", odd)
	defer func() { _ = raw.Close() }()
	if _, err := raw.Exec("CREATE TABLE RestoreMarker(id INTEGER PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	if err := MarkRestore(context.Background(), raw, time.Now()); err == nil {
		t.Fatal("a marker row went into a table of another shape")
	}
}

// A marked restore changes the stream and nothing else that a restart already decides: the
// epoch rises once, a recovering session keeps and settles what it fetched, a fence from before
// is refused, and the fence its reattachment gives releases on its own.
func TestAMarkedRestoreRecoversAsARestartDoes(t *testing.T) {
	s, c, file := testStore(t)
	_, b, delivery, _ := attentionSetup(t, s)
	claimed := call(t, s, "claim", Request{ExecutionID: b.ExecutionID, Generation: b.Generation, Key: "k", Kind: "exact", Path: "a.go"})
	stream, epoch := s.stream, s.epoch
	_ = s.Close()
	s.lock = nil
	mark(t, file)
	n, err := Open(file, c)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = n.Close() }()
	if !n.Restored() || n.stream == stream || n.epoch != epoch+1 {
		t.Fatalf("stream %q (was %q), epoch %d (was %d), restored %v", n.stream, stream, n.epoch, epoch, n.Restored())
	}
	r := reattach(t, n)
	if out := call(t, n, "events", r); out["pending"] != 0 {
		t.Fatalf("pending after a restore: %v", out["pending"])
	}
	ack := r
	ack.Key, ack.Attempt = delivery, 1
	call(t, n, "ack", ack)
	old := r
	old.Fence, old.Token = number(claimed["fence"]), claimed["token"].(string)
	expectError(t, n, "release", old, "conflict")
	var fresh int64
	if err := n.db.QueryRow("SELECT fence FROM Claim WHERE execution_id=? AND revoked_ms IS NULL", r.ExecutionID).Scan(&fresh); err != nil {
		t.Fatal(err)
	}
	release := r
	release.Fence = fresh
	call(t, n, "release", release)
}

// A start that fails anywhere keeps the marker, the stream and the epoch, so the next start
// still runs on a new stream.
func TestAFailedStartKeepsTheMarker(t *testing.T) {
	file, original := closedHub(t)
	mark(t, file)
	f := &storageFault{discover: true}
	name := registerFaultDriver(f)
	if s, err := openDatabase(file, nil, name); !errors.Is(err, errStorageFault) {
		if s != nil {
			_ = s.Close()
		}
		t.Fatal("start discovery", err)
	}
	trace := append([]string(nil), f.trace...)
	f.discover = false
	var epoch int64
	for i, label := range trace {
		f.count, f.at, f.trace = 0, i+1, nil
		s, err := openDatabase(file, nil, name)
		f.at = 0
		if !errors.Is(err, errStorageFault) {
			if s != nil {
				_ = s.Close()
			}
			t.Fatalf("start fault %d %s: %v", i+1, label, err)
		}
		raw, _ := sql.Open("sqlite", file)
		var stream string
		var left int
		err = raw.QueryRow("SELECT stream_id,boot_epoch,(SELECT COUNT(*) FROM RestoreMarker) FROM Meta").Scan(&stream, &epoch, &left)
		_ = raw.Close()
		if err != nil || stream != original || left != 1 || epoch != 1 {
			t.Fatalf("after fault %d %s: stream %q (was %q), epoch %d, %d marker rows, %v", i+1, label, stream, original, epoch, left, err)
		}
	}
	if rotated, restored, left := streamOf(t, file); !restored || rotated == original || left != 0 {
		t.Fatalf("after the faults: stream %q, restored %v, %d marker rows", rotated, restored, left)
	}
}
