package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestUpgradeBackfillsNamesAndPreservesTokens(t *testing.T) {
	file := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", file)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{schema, threadsSchema, `INSERT INTO Repository(id,root_commit) VALUES('remote','root'),('local','root'),('empty','root')`, `INSERT INTO RepositoryAlias VALUES('remote:example.com/team/iris','remote'),('local:host:/work/local','local')`} {
		if _, err = db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(file, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	for id, want := range map[string]string{"remote": "iris", "local": "local", "empty": "."} {
		var got string
		if err = s.db.QueryRow("SELECT name FROM Repository WHERE id=?", id).Scan(&got); err != nil || got != want {
			t.Fatal(id, got, want, err)
		}
	}
	var version int
	if err = s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != latestVersion {
		t.Fatal(version, err)
	}
	if _, err = s.AddToken(context.Background(), "after-upgrade"); err != nil {
		t.Fatal(err)
	}
}
func TestOpenV013Schema28LeavesTablesUnchanged(t *testing.T) {
	file := filepath.Join(t.TempDir(), "v0.1.13.db")
	db, err := sql.Open("sqlite", file)
	if err != nil {
		t.Fatal(err)
	}
	migrations := []string{
		schema, threadsSchema, namesSchema, shellSchema, tokensSchema, pathKeySchema,
		waiterSchema, attentionSchema, contactSchema, watchSeenSchema, receiptsSchema,
		hookSeenSchema, retentionSchema, hostReportSchema, worktreeAliasSchema,
		receiptDueSchema, consoleAccountsSchema, consoleRevisionSchema,
		consoleOwnerActionsSchema, consoleAssignmentImportSchema, idleExpirySchema,
		reducedShellSchema, attentionIdentitySchema, attentionHighWaterSQL, attentionNewestEventSQL,
		removeConsoleImportSchema, restoreMarkerSchema, presenceCursorSchema,
		coordinationCursorSchema, leaseRenewedTickSchema,
	}
	for i, migration := range migrations {
		if _, err = db.Exec(migration); err != nil {
			t.Fatalf("v0.1.13 migration %d: %v", i+1, err)
		}
	}
	var version int
	if err = db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 28 {
		t.Fatalf("v0.1.13 user_version=%d, err=%v", version, err)
	}
	before := databaseTables(t, db)
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(file, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if err = s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != latestVersion || latestVersion != 28 {
		t.Fatalf("opened user_version=%d, latest=%d, err=%v", version, latestVersion, err)
	}
	if after := databaseTables(t, s.db); !slices.Equal(after, before) {
		t.Fatalf("Open changed v0.1.13 tables\nbefore: %q\nafter:  %q", before, after)
	}
}

func databaseTables(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query("SELECT name,sql FROM sqlite_schema WHERE type='table' ORDER BY name")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var tables []string
	for rows.Next() {
		var name, definition string
		if err := rows.Scan(&name, &definition); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, name+"\x00"+definition)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return tables
}

func TestOpenRejectsInvalidFilesystemAndSchema(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, []byte("not SQLite"), 0600); err != nil {
		t.Fatal(err)
	}
	if s, err := Open(filepath.Join(file, "db"), nil); err == nil {
		_ = s.Close()
		t.Fatal("regular-file parent accepted")
	}
	if s, err := Open(file, nil); err == nil {
		_ = s.Close()
		t.Fatal("corrupt SQLite accepted")
	}
	loop := filepath.Join(dir, "loop")
	if err := os.Symlink(loop, loop); err != nil {
		t.Fatal(err)
	}
	if s, err := Open(loop, nil); err == nil {
		_ = s.Close()
		t.Fatal("symlink loop accepted")
	}
	// A malformed migration must roll back without retaining the database lock.
	bad := filepath.Join(dir, "bad.db")
	db, err := sql.Open("sqlite", bad)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("CREATE TABLE Repository(wrong TEXT)"); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	for i := 0; i < 2; i++ {
		if s, err := Open(bad, nil); err == nil {
			_ = s.Close()
			t.Fatal("malformed existing schema accepted")
		}
	}
}
