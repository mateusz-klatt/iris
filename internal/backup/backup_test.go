package backup

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/mateusz-klatt/iris/internal/store"
)

// live opens a hub database and sends one message, leaving the store open as a running hub
// does, with the mail still in the WAL.
func live(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hub.db")
	s, err := store.Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	register := func(session string) map[string]any {
		r, err := s.Call(ctx, "sessions", store.Request{Client: "codex", Host: "h", SessionID: session, Cwd: "/repo", Worktree: "/repo", Source: "hook"})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	a, b := register("a"), register("b")
	if _, err := s.Call(ctx, "messages", store.Request{ExecutionID: a["execution_id"].(string), Generation: a["generation"].(int64), Key: "k", To: []string{b["agent"].(map[string]any)["name"].(string)}, Body: "kept"}); err != nil {
		t.Fatal(err)
	}
	return path
}

// A backup of a running hub holds its latest mail, passes the integrity check, is owner-only,
// leaves the live database as it was, and says how to test a restore.
func TestABackupOfARunningHub(t *testing.T) {
	src := live(t)
	before, _ := os.ReadFile(src)
	var version int
	live, _ := sql.Open("sqlite", readOnly(src))
	if err := live.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	_ = live.Close()
	dest := filepath.Join(t.TempDir(), "backup #%20.db") // characters an SQLite URI would misread, and Windows allows
	var out bytes.Buffer
	if err := Run(context.Background(), src, dest, &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"integrity ok", fmt.Sprintf("schema %d,", version), "1 messages", "iris admin restore-check " + dest} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("no %q in\n%s", want, out.String())
		}
	}
	db, _ := sql.Open("sqlite", readOnly(dest))
	defer func() { _ = db.Close() }()
	var body string
	if err := db.QueryRow("SELECT body FROM Message").Scan(&body); err != nil || body != "kept" {
		t.Fatalf("the backup lacks the mail in the WAL: %q %v", body, err)
	}
	if after, _ := os.ReadFile(src); !bytes.Equal(before, after) {
		t.Fatal("the backup changed the live database")
	}
	if _, err := os.Stat(dest + ".partial"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the partial copy was left behind", err)
	}
	if info, _ := os.Stat(dest); runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("the backup is %v, not owner-only", info.Mode().Perm())
	}
}

// A backup never writes over a file, the live database included, nor over a partial copy left
// by a backup that was stopped, and it needs a database that is a regular file.
func TestABackupRefuses(t *testing.T) {
	src := live(t)
	dir := t.TempDir()
	stale := filepath.Join(dir, "stale.db")
	if err := os.WriteFile(stale+".partial", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	garbage := filepath.Join(dir, "garbage")
	if err := os.WriteFile(garbage, bytes.Repeat([]byte("not sqlite "), 100), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct{ source, dest, want string }{
		"the live database":         {src, src, "never writes over"},
		"a stopped backup":          {src, stale, "remove it first"},
		"a missing database":        {filepath.Join(dir, "none.db"), filepath.Join(dir, "x.db"), notExist},
		"a directory":               {dir, filepath.Join(dir, "y.db"), "not a regular file"},
		"a missing directory":       {src, filepath.Join(dir, "none", "z.db"), notExist},
		"a file that is not SQLite": {garbage, filepath.Join(dir, "v.db"), "copy:"},
		"a file that is no hub":     {stale + ".partial", filepath.Join(dir, "w.db"), "read the copy"},
	} {
		var out bytes.Buffer
		err := Run(context.Background(), c.source, c.dest, &out)
		// The operating system words a missing file its own way, so that case is matched by kind.
		if err == nil || (c.want == notExist && !errors.Is(err, fs.ErrNotExist)) || (c.want != notExist && !strings.Contains(err.Error(), c.want)) {
			t.Errorf("%s: %v", name, err)
		}
		if c.dest != src && c.dest != stale {
			if _, err := os.Lstat(c.dest); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("%s: a backup was left at %s", name, c.dest)
			}
			if _, err := os.Lstat(c.dest + ".partial"); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("%s: a partial copy was left", name)
			}
		}
	}
}

// notExist stands for an error that is fs.ErrNotExist.
const notExist = "<not exist>"

// A file that appears at the destination while the copy is checked is kept, and the backup
// fails rather than replace it.
func TestABackupDoesNotReplaceAFileThatAppears(t *testing.T) {
	src := live(t)
	dest := filepath.Join(t.TempDir(), "backup.db")
	real := verify
	verify = func(path string) (summary, error) {
		if err := os.WriteFile(dest, []byte("someone else's"), 0o600); err != nil {
			t.Fatal(err)
		}
		return real(path)
	}
	t.Cleanup(func() { verify = real })
	if err := Run(context.Background(), src, dest, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "in place") {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(dest); string(raw) != "someone else's" {
		t.Fatal("the file that appeared was replaced")
	}
}

// A copy that fails SQLite's integrity check, or cannot be read as a database, is refused.
func TestACorruptCopyIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "corrupt.db")
	db, _ := sql.Open("sqlite", path)
	for _, q := range []string{"PRAGMA page_size=512", "CREATE TABLE t(x)", "CREATE INDEX i ON t(x)", "INSERT INTO t SELECT randomblob(400) FROM (WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i<50) SELECT i FROM n)"} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	_ = db.Close()
	raw, _ := os.ReadFile(path)
	for i := 512 * 3; i < 512*4; i++ { // a page in the middle of the table and its index
		raw[i] = 0x5a
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := summarise(path); err == nil || !strings.Contains(err.Error(), "integrity") {
		t.Fatal(err)
	}
	garbage := filepath.Join(t.TempDir(), "garbage.db")
	if err := os.WriteFile(garbage, bytes.Repeat([]byte("not sqlite "), 100), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := summarise(garbage); err == nil || !strings.Contains(err.Error(), "check the copy") {
		t.Fatal(err)
	}
}

// The copy keeps SQLite's temporary storage in memory, as the hub does, so a backup works in
// the image, which has no temporary directory.
func TestABackupNeedsNoTemporaryDirectory(t *testing.T) {
	src := live(t)
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "missing"))
	db, _ := sql.Open("sqlite", readOnly(src))
	defer func() { _ = db.Close() }()
	var mode int
	if err := db.QueryRow("PRAGMA temp_store").Scan(&mode); err != nil || mode != 2 {
		t.Fatalf("temp_store %d: %v", mode, err)
	}
	if err := Run(context.Background(), src, filepath.Join(t.TempDir(), "b.db"), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
}
