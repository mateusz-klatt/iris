package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
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
	defer s.Close()
	for id, want := range map[string]string{"remote": "iris", "local": "local", "empty": "."} {
		var got string
		if err = s.db.QueryRow("SELECT name FROM Repository WHERE id=?", id).Scan(&got); err != nil || got != want {
			t.Fatal(id, got, want, err)
		}
	}
	var version int
	if err = s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 6 {
		t.Fatal(version, err)
	}
	if _, err = s.AddToken(context.Background(), "after-upgrade"); err != nil {
		t.Fatal(err)
	}
}
func TestOpenRejectsInvalidFilesystemAndSchema(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, []byte("not SQLite"), 0600); err != nil {
		t.Fatal(err)
	}
	if s, err := Open(filepath.Join(file, "db"), nil); err == nil {
		s.Close()
		t.Fatal("regular-file parent accepted")
	}
	if s, err := Open(file, nil); err == nil {
		s.Close()
		t.Fatal("corrupt SQLite accepted")
	}
	loop := filepath.Join(dir, "loop")
	if err := os.Symlink(loop, loop); err != nil {
		t.Fatal(err)
	}
	if s, err := Open(loop, nil); err == nil {
		s.Close()
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
	db.Close()
	for i := 0; i < 2; i++ {
		if s, err := Open(bad, nil); err == nil {
			s.Close()
			t.Fatal("malformed existing schema accepted")
		}
	}
}
