package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// `iris admin mark-restored` (ADR 006): the restore marker set offline in a copy that `iris admin
// backup` did not make, so that its next start runs on a new stream.

// fileState is what a start would change in a hub database, read without starting it.
type fileState struct {
	stream               string
	epoch, version       int64
	recovering, active   int
	markers              int
	markedMS             int64
	tables, events, mail int
}

func readFileState(t *testing.T, file string) fileState {
	t.Helper()
	db, err := sql.Open("sqlite", file)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var s fileState
	for _, q := range []struct {
		query string
		dest  []any
	}{
		{"SELECT stream_id,boot_epoch FROM Meta WHERE id=1", []any{&s.stream, &s.epoch}},
		{"PRAGMA user_version", []any{&s.version}},
		{"SELECT COUNT(*) FROM Execution WHERE state='active' AND recovery_deadline IS NOT NULL", []any{&s.recovering}},
		{"SELECT COUNT(*) FROM Execution WHERE state='active'", []any{&s.active}},
		{"SELECT COUNT(*),COALESCE(MAX(made_ms),0) FROM RestoreMarker", []any{&s.markers, &s.markedMS}},
		{"SELECT COUNT(*) FROM sqlite_master", []any{&s.tables}},
		{"SELECT COUNT(*) FROM Event", []any{&s.events}},
		{"SELECT COUNT(*) FROM Message", []any{&s.mail}},
	} {
		if err := db.QueryRow(q.query).Scan(q.dest...); err != nil {
			t.Fatalf("%s: %v", q.query, err)
		}
	}
	return s
}

// liveHub makes a hub database with an active session and mail, as a copy of a running hub's
// file holds them, and closes it.
func liveHub(t *testing.T) string {
	t.Helper()
	file := filepath.Join(t.TempDir(), "hub.db")
	s, err := Open(file, nil)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := session(t, s, 1)
	_, br := session(t, s, 2)
	a.Key, a.To, a.Body = "mail", []string{br["agent"].(map[string]any)["name"].(string)}, "synthetic"
	call(t, s, "messages", a)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	return file
}

// It sets the marker and nothing else: no start runs, so the stream, the epoch, the schema
// version and every session stay as the file had them. Setting it again leaves one row. The next
// start then runs on a new stream, once.
func TestMarkRestoredMarksACopyWithoutStartingIt(t *testing.T) {
	file := liveHub(t)
	before := readFileState(t, file)
	if before.active != 2 || before.recovering != 0 || before.markers != 0 {
		t.Fatalf("the copy is not a stopped live hub: %+v", before)
	}
	made := time.UnixMilli(1800000000123)
	for range 2 {
		if err := MarkRestored(context.Background(), file, made); err != nil {
			t.Fatal(err)
		}
	}
	// Before anything opens the file again, which could checkpoint it: no sidecar is left, and the
	// database file alone carries the marker.
	for _, sidecar := range []string{"-wal", "-shm", "-journal"} {
		if _, err := os.Stat(file + sidecar); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("marking left %s beside the file: %v", sidecar, err)
		}
	}
	alone := filepath.Join(t.TempDir(), "alone.db")
	copyFile(t, file, alone)
	want := before
	want.markers, want.markedMS = 1, made.UnixMilli()
	if got := readFileState(t, alone); got != want {
		t.Fatalf("the database file alone:\nwant %+v\ngot  %+v", want, got)
	}
	if after := readFileState(t, file); after != want {
		t.Fatalf("marking changed more than the marker:\nbefore %+v\nafter  %+v", before, after)
	}
	rotated, restored, left := streamOf(t, file)
	if !restored || rotated == before.stream || left != 0 {
		t.Fatalf("first start: stream %q (was %q), restored %v, %d marker rows left", rotated, before.stream, restored, left)
	}
	if again, restored, _ := streamOf(t, file); restored || again != rotated {
		t.Fatalf("second start: stream %q (was %q), restored %v", again, rotated, restored)
	}
}

// A database from before migration 025 gets the marker's table, and keeps its schema version:
// the next start migrates it and rotates once, as for a backup's copy of such a database.
func TestMarkRestoredMarksADatabaseFromBeforeTheMarker(t *testing.T) {
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
	if err := MarkRestored(context.Background(), file, time.Now()); err != nil {
		t.Fatal(err)
	}
	if s := readFileState(t, file); s.version != 24 || s.markers != 1 {
		t.Fatalf("marked schema-24 file: %+v", s)
	}
	rotated, restored, left := streamOf(t, file)
	if !restored || rotated == original || left != 0 {
		t.Fatalf("first start: stream %q (was %q), restored %v, %d marker rows left", rotated, original, restored, left)
	}
}

// A hub holds its database's lock while it runs, so marking refuses that database under its own
// path and under every path that resolves to it: relative, through "..", or through a link to
// the file or to its directory. Nothing is written. Once the hub stops, the same alias works.
func TestMarkRestoredRefusesADatabaseAHubHasOpen(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "hub.db")
	hub, err := Open(file, nil)
	if err != nil {
		t.Fatal(err)
	}
	stopped := false
	t.Cleanup(func() {
		if !stopped {
			_ = hub.Close()
		}
	})
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	// filepath.Join would clean "sub/.." away, so that alias is spelled out.
	sep := string(filepath.Separator)
	aliases := []string{file, dir + sep + "sub" + sep + ".." + sep + "hub.db", "hub.db"}
	links := t.TempDir()
	fileLink, dirLink := filepath.Join(links, "file.db"), filepath.Join(links, "dir")
	if os.Symlink(file, fileLink) == nil && os.Symlink(dir, dirLink) == nil {
		aliases = append(aliases, fileLink, filepath.Join(dirLink, "hub.db"))
	} else {
		t.Log("this system makes no symbolic links here, so only the other aliases are tried")
	}
	t.Chdir(dir)
	for _, alias := range aliases {
		err := MarkRestored(context.Background(), alias, time.Now())
		if err == nil || !strings.Contains(err.Error(), "database already owned by another hub") {
			t.Fatalf("marking %s while a hub runs: %v", alias, err)
		}
	}
	if err := hub.Close(); err != nil {
		t.Fatal(err)
	}
	stopped = true
	if s := readFileState(t, file); s.markers != 0 {
		t.Fatalf("a refused marking wrote: %+v", s)
	}
	if err := MarkRestored(context.Background(), aliases[len(aliases)-1], time.Now()); err != nil {
		t.Fatal(err)
	}
	if s := readFileState(t, file); s.markers != 1 {
		t.Fatalf("marking through an alias after the hub stopped: %+v", s)
	}
}

// Only an existing regular file that is a hub database is marked. A missing path creates
// nothing, not even its directory; a directory, a file that is not SQLite, and an SQLite database
// without a hub's Meta row are refused, and none of them is changed.
func TestMarkRestoredRefusesWhatIsNotAHubDatabase(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "absent", "hub.db")
	if err := MarkRestored(context.Background(), missing, time.Now()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a missing database: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(missing)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("marking a missing database made its directory: %v", err)
	}
	if err := MarkRestored(context.Background(), dir, time.Now()); err == nil || !strings.Contains(err.Error(), "is not a regular file") {
		t.Fatalf("a directory: %v", err)
	}
	junk := filepath.Join(dir, "junk.db")
	if err := os.WriteFile(junk, []byte(strings.Repeat("not a database ", 100)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := MarkRestored(context.Background(), junk, time.Now()); err == nil {
		t.Fatal("a file that is not SQLite was marked")
	}
	if raw, _ := os.ReadFile(junk); string(raw) != strings.Repeat("not a database ", 100) {
		t.Fatal("marking changed a file that is not SQLite")
	}
	for _, tc := range []struct{ name, schema string }{
		{"no Meta table", "CREATE TABLE Other(x)"},
		{"an empty Meta table", "CREATE TABLE Meta(id INTEGER PRIMARY KEY, stream_id TEXT, boot_epoch INTEGER)"},
	} {
		file := filepath.Join(t.TempDir(), "other.db")
		db, err := sql.Open("sqlite", file)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(tc.schema); err != nil {
			t.Fatal(err)
		}
		_ = db.Close()
		if err := MarkRestored(context.Background(), file, time.Now()); !errors.Is(err, errNotAHub) {
			t.Fatalf("%s: %v", tc.name, err)
		}
		check, _ := sql.Open("sqlite", file)
		var tables int
		if err := check.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE name='RestoreMarker'").Scan(&tables); err != nil || tables != 0 {
			t.Fatalf("%s: a refused marking left the table: %d %v", tc.name, tables, err)
		}
		_ = check.Close()
	}
}

// Marking writes the marker into the database file itself before it reports success, even while
// another process has the file open, which stops the close from checkpointing it. While another
// process holds a read from before the marker, the checkpoint cannot finish: marking says so and
// fails, and a copy of the database file alone does not carry the marker. Once that read is over,
// marking again puts it in the file.
func TestMarkRestoredPutsTheMarkerInTheDatabaseFileItself(t *testing.T) {
	file := liveHub(t)
	other, err := sql.Open("sqlite", file)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = other.Close() }()
	var hubs int
	if err := other.QueryRow("SELECT COUNT(*) FROM Meta").Scan(&hubs); err != nil || hubs != 1 {
		t.Fatal(hubs, err)
	}
	first := time.UnixMilli(1800000000001)
	if err := MarkRestored(context.Background(), file, first); err != nil {
		t.Fatalf("with another connection open, but idle: %v", err)
	}
	alone := filepath.Join(t.TempDir(), "alone.db")
	copyFile(t, file, alone)
	if s := readFileState(t, alone); s.markers != 1 || s.markedMS != first.UnixMilli() {
		t.Fatalf("the database file alone lacks the marker while another connection is open: %+v", s)
	}

	read, err := other.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := read.QueryRow("SELECT COUNT(*) FROM Meta").Scan(&hubs); err != nil {
		t.Fatal(err)
	}
	second := time.UnixMilli(1800000000002)
	if err := MarkRestored(context.Background(), file, second); !errors.Is(err, errNotInFile) {
		t.Fatalf("with another process's read open: %v", err)
	}
	stale := filepath.Join(t.TempDir(), "stale.db")
	copyFile(t, file, stale)
	if s := readFileState(t, stale); s.markedMS != first.UnixMilli() {
		t.Fatalf("the database file alone already held the marker that was reported missing from it: %+v", s)
	}
	_ = read.Rollback()
	third := time.UnixMilli(1800000000003)
	if err := MarkRestored(context.Background(), file, third); err != nil {
		t.Fatalf("after the read ended: %v", err)
	}
	copyFile(t, file, alone)
	if s := readFileState(t, alone); s.markers != 1 || s.markedMS != third.UnixMilli() {
		t.Fatalf("the database file alone after the read ended: %+v", s)
	}
}

// Every step that can fail is reported. A failure up to the commit leaves the file as it was; a
// failure of the checkpoint after it leaves the marker committed and says that the file alone may
// not hold it. Either way the lock is free afterwards. A path that cannot be resolved and a caller
// that has already given up are reported before anything is opened.
func TestMarkRestoredReportsEveryFailureAndChangesNothing(t *testing.T) {
	file := liveHub(t)
	before := readFileState(t, file)
	count := &storageFault{}
	probe := filepath.Join(t.TempDir(), "probe.db")
	copyFile(t, file, probe)
	if err := markRestored(context.Background(), probe, time.Now(), registerFaultDriver(count), filepath.Abs, filepath.EvalSymlinks); err != nil {
		t.Fatal(err)
	}
	steps := strings.Join(count.trace, " | ")
	commit := slices.Index(count.trace, "COMMIT") + 1
	if count.count != 8 || commit != 6 || !strings.HasPrefix(steps, "BEGIN IMMEDIATE | SELECT COUNT(*) FROM Meta") || !strings.HasSuffix(steps, "PRAGMA wal_checkpoint(TRUNCATE) | step result") {
		t.Fatalf("the steps changed, so this test no longer fails each of them: %s", steps)
	}
	faulty := filepath.Join(t.TempDir(), "faulty.db")
	copyFile(t, file, faulty)
	made := time.UnixMilli(1800000000777)
	for at := 1; at <= count.count; at++ {
		f := &storageFault{at: at}
		err := markRestored(context.Background(), faulty, made, registerFaultDriver(f), filepath.Abs, filepath.EvalSymlinks)
		if !errors.Is(err, errStorageFault) {
			t.Fatalf("a failure at step %d of %q was not reported: %v", at, f.trace, err)
		}
		after := readFileState(t, faulty)
		if at <= commit {
			if after != before {
				t.Fatalf("a failure at step %d changed the file:\nbefore %+v\nafter  %+v", at, before, after)
			}
			continue
		}
		if !errors.Is(err, errNotInFile) || after.markers != 1 || after.markedMS != made.UnixMilli() {
			t.Fatalf("a failed checkpoint at step %d: %v, %+v", at, err, after)
		}
	}
	unresolved := func(string) (string, error) { return "", errors.New("no absolute path") }
	if err := markRestored(context.Background(), file, time.Now(), "sqlite", unresolved, filepath.EvalSymlinks); err == nil || err.Error() != "no absolute path" {
		t.Fatalf("an unresolved path: %v", err)
	}
	gone, cancel := context.WithCancel(context.Background())
	cancel()
	if err := MarkRestored(gone, file, time.Now()); !errors.Is(err, context.Canceled) {
		t.Fatalf("a caller that gave up: %v", err)
	}
	if after := readFileState(t, file); after != before {
		t.Fatalf("a failure changed the file:\nbefore %+v\nafter  %+v", before, after)
	}
	if err := MarkRestored(context.Background(), file, time.Now()); err != nil {
		t.Fatalf("the lock or the file was left unusable: %v", err)
	}
}
