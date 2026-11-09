package accountimport

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type sourceArtifactState struct {
	Contents []byte
	Mode     os.FileMode
	Modified int64
}

type sourceDirectoryState struct {
	Files    map[string]sourceArtifactState
	Mode     os.FileMode
	Modified int64
}

// Capturing the directory itself detects creation followed by removal too;
// comparing only the main database would miss WAL read-mark and SHM writes.
func captureSourceDirectory(t *testing.T, dir string) sourceDirectoryState {
	t.Helper()
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal("could not inspect synthetic source directory")
	}
	state := sourceDirectoryState{Files: map[string]sourceArtifactState{}, Mode: info.Mode(), Modified: info.ModTime().UnixNano()}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal("could not list synthetic source directory")
	}
	for _, entry := range entries {
		info, err = entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			t.Fatal("could not inspect synthetic source artifact")
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatal("could not capture synthetic source artifact")
		}
		state.Files[entry.Name()] = sourceArtifactState{Contents: data, Mode: info.Mode(), Modified: info.ModTime().UnixNano()}
	}
	return state
}

func assertSourceDirectoryUnchanged(t *testing.T, dir string, before sourceDirectoryState) {
	t.Helper()
	if !reflect.DeepEqual(before, captureSourceDirectory(t, dir)) {
		t.Fatal("importer changed source DB, WAL, SHM, file metadata or directory entries")
	}
}

func writeSyntheticSourceArtifact(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal("could not create synthetic source artifact")
	}
}

// The first row is checkpointed in the main file; the second committed row
// exists only in WAL. No writer runs while the importer is being exercised.
func uncheckpointedSource(t *testing.T) (string, *sql.DB) {
	t.Helper()
	file := filepath.Join(t.TempDir(), "source.sqlite")
	db, err := sql.Open("sqlite", file)
	if err != nil {
		t.Fatal("could not create synthetic WAL source")
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	for _, query := range []string{"PRAGMA journal_mode=WAL", "PRAGMA wal_autocheckpoint=0", sourceSchema} {
		if _, err = db.Exec(query); err != nil {
			t.Fatal("could not initialize synthetic WAL source")
		}
	}
	insert := func(id int) {
		t.Helper()
		r := testRow()
		_, err := db.Exec(`INSERT INTO accounts VALUES(?,?,?,?,?,?,?,?,?,?)`, id, fmt.Sprintf("Person%d", id), r.PasswordHash, r.Role, 0, r.DisplayName, r.UILocale, r.CorrespondenceLocale, 1, nil)
		if err != nil {
			t.Fatal("could not commit synthetic WAL account")
		}
	}
	insert(1)
	var busy, logFrames, checkpointed int
	if err = db.QueryRow("PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &logFrames, &checkpointed); err != nil || busy != 0 {
		t.Fatal("could not checkpoint synthetic source baseline")
	}
	insert(2)
	walInfo, err := os.Stat(file + "-wal")
	if err != nil || walInfo.Size() <= 32 {
		t.Fatal("synthetic committed row did not remain in WAL")
	}
	var count int
	if err = db.QueryRow("SELECT COUNT(*) FROM accounts").Scan(&count); err != nil || count != 2 {
		t.Fatal("synthetic WAL source is missing its committed account")
	}
	return file, db
}

func assertCheckpointedSourceRequired(t *testing.T, file string) {
	t.Helper()
	before := captureSourceDirectory(t, filepath.Dir(file))
	for range 2 {
		opened := false
		batch, err := read(context.Background(), file, testManifest(), func(string, string) (*sql.DB, error) {
			opened = true
			return nil, errors.New("synthetic opener must remain unused")
		})
		if err == nil || !strings.Contains(err.Error(), "checkpointed") || opened || len(batch.Rows) != 0 {
			t.Fatal("WAL/journal source was not refused before SQLite opened it")
		}
		batch, err = Read(context.Background(), file, testManifest())
		if err == nil || !strings.Contains(err.Error(), "checkpointed") || len(batch.Rows) != 0 {
			t.Fatal("WAL/journal source was accepted or lost committed WAL accounts silently")
		}
		assertSourceDirectoryUnchanged(t, filepath.Dir(file), before)
	}
}

func TestWALSourceRefusalPreservesDatabaseWALSHMAndDirectory(t *testing.T) {
	file, _ := uncheckpointedSource(t)
	before := captureSourceDirectory(t, filepath.Dir(file))
	main := before.Files[filepath.Base(file)].Contents
	if len(main) < 20 || main[18] != 2 || main[19] != 2 {
		t.Fatal("synthetic source is not persistently WAL mode")
	}
	// Independently verify the committed second account cannot be recovered
	// from the main file alone. The importer must never accept that partial view.
	baseCopy := filepath.Join(t.TempDir(), "main-only.sqlite")
	writeSyntheticSourceArtifact(t, baseCopy, main)
	u := url.URL{Path: filepath.ToSlash(baseCopy)}
	db, err := sql.Open("sqlite", "file:"+u.EscapedPath()+"?mode=ro&immutable=1")
	if err != nil {
		t.Fatal("could not inspect synthetic main-only copy")
	}
	var count int
	err = db.QueryRow("SELECT COUNT(*) FROM accounts").Scan(&count)
	_ = db.Close()
	if err != nil || count != 1 {
		t.Fatal("synthetic pending account was already checkpointed")
	}
	assertSourceDirectoryUnchanged(t, filepath.Dir(file), before)

	t.Run("live-shm", func(t *testing.T) { assertCheckpointedSourceRequired(t, file) })
	for _, variant := range []string{"missing-shm", "stale-shm", "missing-wal"} {
		t.Run(variant, func(t *testing.T) {
			copy := filepath.Join(t.TempDir(), "source.sqlite")
			writeSyntheticSourceArtifact(t, copy, main)
			if variant != "missing-wal" {
				writeSyntheticSourceArtifact(t, copy+"-wal", before.Files[filepath.Base(file)+"-wal"].Contents)
			}
			switch variant {
			case "stale-shm":
				writeSyntheticSourceArtifact(t, copy+"-shm", make([]byte, len(before.Files[filepath.Base(file)+"-shm"].Contents)))
			case "missing-wal":
				writeSyntheticSourceArtifact(t, copy+"-shm", before.Files[filepath.Base(file)+"-shm"].Contents)
			}
			assertCheckpointedSourceRequired(t, copy)
		})
	}
	assertSourceDirectoryUnchanged(t, filepath.Dir(file), before)
}

func TestCheckpointedWALHeaderWithoutSidecarsStillRequiresPreparedExport(t *testing.T) {
	file, db := uncheckpointedSource(t)
	if err := db.Close(); err != nil {
		t.Fatal("could not close synthetic source writer")
	}
	entries, err := os.ReadDir(filepath.Dir(file))
	if err != nil || len(entries) != 1 {
		t.Fatal("closed synthetic WAL source did not checkpoint and remove sidecars")
	}
	assertCheckpointedSourceRequired(t, file)
}

func TestStrayJournalArtifactsRefuseBeforeSQLiteOpensRollbackExport(t *testing.T) {
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		t.Run(suffix, func(t *testing.T) {
			file := sourceFile(t, sourceSchema, 1)
			writeSyntheticSourceArtifact(t, file+suffix, nil)
			assertCheckpointedSourceRequired(t, file)
		})
	}
}

func TestPreparedDeleteModeExportIncludesCommittedWALRowsWithoutSourceChanges(t *testing.T) {
	file, writer := uncheckpointedSource(t)
	export := filepath.Join(t.TempDir(), "closed-export.sqlite")
	if _, err := writer.Exec("VACUUM INTO ?", export); err != nil {
		t.Fatal("source owner could not prepare synthetic consistent export")
	}
	db, err := sql.Open("sqlite", export)
	if err != nil {
		t.Fatal("could not open synthetic disposable export")
	}
	var journalMode string
	err = db.QueryRow("PRAGMA journal_mode=DELETE").Scan(&journalMode)
	_ = db.Close()
	if err != nil || journalMode != "delete" {
		t.Fatal("source owner could not finalize synthetic DELETE-mode export")
	}
	sourceBefore := captureSourceDirectory(t, filepath.Dir(file))
	exportBefore := captureSourceDirectory(t, filepath.Dir(export))
	assertCheckpointedSourceRequired(t, file)
	for range 2 {
		batch, err := read(context.Background(), export, testManifest(), func(driverName, dsn string) (*sql.DB, error) {
			u, err := url.Parse(dsn)
			if err != nil || u.Query().Get("mode") != "ro" || u.Query().Get("immutable") != "1" {
				t.Fatal("prepared source connection permits SQLite locking or mutable-file handling")
			}
			return sql.Open(driverName, dsn)
		})
		if err != nil || len(batch.Rows) != 2 || batch.Rows[0].ID != "1" || batch.Rows[1].ID != "2" || batch.Rows[1].PasswordHash != testRow().PasswordHash {
			t.Fatal("prepared export did not preserve every committed WAL account")
		}
		assertSourceDirectoryUnchanged(t, filepath.Dir(file), sourceBefore)
		assertSourceDirectoryUnchanged(t, filepath.Dir(export), exportBefore)
	}
}
