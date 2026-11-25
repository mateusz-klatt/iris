package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
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
func TestOpenLatestSchemaLeavesTablesUnchanged(t *testing.T) {
	file := filepath.Join(t.TempDir(), "latest.db")
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
		coordinationCursorSchema, leaseRenewedTickSchema, successionProvenanceSchema,
	}
	for i, migration := range migrations {
		if _, err = db.Exec(migration); err != nil {
			t.Fatalf("schema migration %d: %v", i+1, err)
		}
	}
	var version int
	if err = db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 29 {
		t.Fatalf("schema user_version=%d, err=%v", version, err)
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
	if err = s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != latestVersion || latestVersion != 29 {
		t.Fatalf("opened user_version=%d, latest=%d, err=%v", version, latestVersion, err)
	}
	if after := databaseTables(t, s.db); !slices.Equal(after, before) {
		t.Fatalf("Open changed latest schema tables\nbefore: %q\nafter:  %q", before, after)
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

func TestMigration029AddsSuccessionProvenanceAndRollsBack(t *testing.T) {
	s, clock, file := testStore(t)
	first := call(t, s, "sessions", Request{Client: "codex", Host: "host", SessionID: "S", Cwd: "/repo/w", Worktree: "/repo/w", Source: "hook"})
	if _, err := s.db.Exec(removeSuccessionProvenanceSchema + "PRAGMA user_version=28"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s.lock = nil

	f := &storageFault{discover: true}
	name := registerFaultDriver(f)
	if failed, err := openDatabase(file, clock, name); !errors.Is(err, errStorageFault) {
		if failed != nil {
			_ = failed.Close()
		}
		t.Fatal("discovery", err)
	}
	trace := append([]string(nil), f.trace...)
	f.discover = false
	if !strings.Contains(strings.Join(trace, "\n"), "prior_execution_id") {
		t.Fatalf("migration 029 missing from fault trace: %v", trace)
	}
	for i, label := range trace {
		f.count, f.at, f.trace = 0, i+1, nil
		failed, err := openDatabase(file, clock, name)
		f.at = 0
		if !errors.Is(err, errStorageFault) {
			if failed != nil {
				_ = failed.Close()
			}
			t.Fatalf("fault %d %s: %v", i+1, label, err)
		}
		raw, err := sql.Open("sqlite", file)
		if err != nil {
			t.Fatal(err)
		}
		var version int
		if err = raw.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 28 {
			_ = raw.Close()
			t.Fatalf("fault %d %s changed version to %d: %v", i+1, label, version, err)
		}
		_ = raw.Close()
	}

	retried, err := openDatabase(file, clock, name)
	if err != nil {
		t.Fatal("migration 029 retry failed", err)
	}
	defer func() { _ = retried.Close() }()
	var version, idx int
	if err = retried.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != latestVersion {
		t.Fatalf("version=%d want=%d err=%v", version, latestVersion, err)
	}
	if err = retried.db.QueryRow("SELECT COUNT(*) FROM sqlite_schema WHERE type='index' AND name='execution_prior'").Scan(&idx); err != nil || idx != 1 {
		t.Fatalf("execution_prior index missing: count=%d err=%v", idx, err)
	}
	var prior, source, proof sql.NullString
	if err = retried.db.QueryRow("SELECT prior_execution_id,creation_source,creation_proof FROM Execution WHERE id=?", first["execution_id"]).Scan(&prior, &source, &proof); err != nil || prior.Valid || source.Valid || proof.Valid {
		t.Fatalf("migrated existing row had non-null provenance: prior=%+v source=%+v proof=%+v err=%v", prior, source, proof, err)
	}
}

const adr037Migration029SQL = `
ALTER TABLE Repository ADD COLUMN auto_claim_scope TEXT NOT NULL DEFAULT 'repository' CHECK(auto_claim_scope IN ('repository','worktree'));
ALTER TABLE Execution ADD COLUMN worktree_key TEXT;
ALTER TABLE Claim ADD COLUMN worktree_key TEXT;
ALTER TABLE Claim ADD COLUMN worktree_path TEXT;
ALTER TABLE Claim ADD COLUMN branch TEXT;
ALTER TABLE Waiter ADD COLUMN worktree_key TEXT;
ALTER TABLE Event ADD COLUMN worktree_key TEXT;
CREATE TABLE WorktreeIdentity (
 host_id TEXT NOT NULL, worktree_id TEXT NOT NULL, worktree_key TEXT NOT NULL UNIQUE,
 PRIMARY KEY (host_id, worktree_id)
);
DROP TRIGGER console_revision_claim_update;
CREATE TRIGGER console_revision_claim_update AFTER UPDATE OF execution_id,repository_id,kind,path,origin,revoked_ms,worktree_key,worktree_path,branch ON Claim
WHEN OLD.execution_id IS NOT NEW.execution_id OR OLD.repository_id IS NOT NEW.repository_id OR OLD.kind IS NOT NEW.kind OR OLD.path IS NOT NEW.path OR OLD.origin IS NOT NEW.origin OR OLD.revoked_ms IS NOT NEW.revoked_ms OR OLD.worktree_key IS NOT NEW.worktree_key OR OLD.worktree_path IS NOT NEW.worktree_path OR OLD.branch IS NOT NEW.branch BEGIN
 INSERT INTO ConsoleRepositoryChange SELECT OLD.repository_id UNION SELECT NEW.repository_id;
END;
PRAGMA user_version=29;
`

const adr037Migration030SQL = `
ALTER TABLE Event ADD COLUMN repository_id TEXT;
PRAGMA user_version=30;
`

var v28Schemas = []string{
	schema, threadsSchema, namesSchema, shellSchema, tokensSchema, pathKeySchema,
	waiterSchema, attentionSchema, contactSchema, watchSeenSchema, receiptsSchema,
	hookSeenSchema, retentionSchema, hostReportSchema, worktreeAliasSchema,
	receiptDueSchema, consoleAccountsSchema, consoleRevisionSchema,
	consoleOwnerActionsSchema, consoleAssignmentImportSchema, idleExpirySchema,
	reducedShellSchema, attentionIdentitySchema, attentionHighWaterSQL, attentionNewestEventSQL,
	removeConsoleImportSchema, restoreMarkerSchema, presenceCursorSchema,
	coordinationCursorSchema, leaseRenewedTickSchema,
}

func TestRefusesRevertedADR037DatabaseWithoutWritesAndUpgrades028(t *testing.T) {
	// Genuine v028 upgrade still succeeds cleanly alongside ADR 037 refusal tests.
	t.Run("genuine v28 upgrades to 29", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "v28.db")
		db, err := sql.Open("sqlite", file)
		if err != nil {
			t.Fatal(err)
		}
		for i, q := range v28Schemas {
			if _, err := db.Exec(q); err != nil {
				t.Fatalf("v28 migration %d: %v", i+1, err)
			}
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		s, err := Open(file, nil)
		if err != nil {
			t.Fatalf("v28 upgrade failed: %v", err)
		}
		defer func() { _ = s.Close() }()
		var version, priorCol int
		if err := s.db.QueryRow("SELECT (SELECT user_version FROM pragma_user_version),(SELECT COUNT(*) FROM pragma_table_info('Execution') WHERE name='prior_execution_id')").Scan(&version, &priorCol); err != nil || version != latestVersion || priorCol != 1 {
			t.Fatalf("v28 did not reach v%d with prior_execution_id: version=%d priorCol=%d err=%v", latestVersion, version, priorCol, err)
		}
	})

	for _, version := range []int{29, 30} {
		t.Run("reverted ADR 037 version "+string(rune('0'+version/10))+string(rune('0'+version%10)), func(t *testing.T) {
			s, _, file := testStore(t)
			call(t, s, "sessions", Request{Client: "codex", Host: "host", SessionID: "S", Cwd: "/repo/w", Worktree: "/repo/w", Source: "hook"})
			if _, err := s.db.Exec(removeSuccessionProvenanceSchema + "PRAGMA user_version=28"); err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			raw, err := sql.Open("sqlite", file)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := raw.Exec(adr037Migration029SQL); err != nil {
				_ = raw.Close()
				t.Fatal(err)
			}
			if version == 30 {
				if _, err := raw.Exec(adr037Migration030SQL); err != nil {
					_ = raw.Close()
					t.Fatal(err)
				}
			}
			if _, err := raw.Exec("PRAGMA wal_checkpoint(TRUNCATE); PRAGMA journal_mode=DELETE"); err != nil {
				_ = raw.Close()
				t.Fatal(err)
			}
			if err := raw.Close(); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			opened, err := Open(file, nil)
			if err == nil {
				_ = opened.Close()
				t.Fatalf("opened reverted ADR 037 version %d database", version)
			}
			if !strings.Contains(err.Error(), "reverted ADR 037") || !strings.Contains(err.Error(), "pre-ADR-037 backup") {
				t.Fatalf("unexpected refusal error: %v", err)
			}
			after, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatalf("Open modified reverted ADR 037 version %d database before refusing", version)
			}
			for _, suffix := range []string{"-wal", "-shm"} {
				if _, err := os.Stat(file + suffix); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("Open left sidecar %s on refused ADR 037 DB: %v", suffix, err)
				}
			}
		})
	}
}

const (
	envADR037WALCrashDB   = "IRIS_TEST_ADR037_WAL_CRASH_DB"
	envADR037WALCrashMode = "IRIS_TEST_ADR037_WAL_CRASH_MODE"
)

func TestADR037WALCrashHelper(t *testing.T) {
	file := os.Getenv(envADR037WALCrashDB)
	if file == "" {
		return
	}
	db, err := sql.Open("sqlite", file)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		t.Fatal(err)
	}
	for i, q := range v28Schemas {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("v28 migration %d: %v", i+1, err)
		}
	}
	if _, err := db.Exec(`INSERT INTO Repository(id,name,root_commit) VALUES('rp_crash','repo','root')`); err != nil {
		t.Fatal(err)
	}
	mode := os.Getenv(envADR037WALCrashMode)
	if mode != "wal_only_029" {
		if _, err := db.Exec(adr037Migration029SQL); err != nil {
			t.Fatal(err)
		}
		if mode == "migrated_030" {
			if _, err := db.Exec(adr037Migration030SQL); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := db.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		t.Fatal(err)
	}
	if mode == "wal_only_029" {
		if _, err := db.Exec(adr037Migration029SQL); err != nil {
			t.Fatal(err)
		}
	} else {
		if _, err := db.Exec(`INSERT INTO WorktreeIdentity(host_id,worktree_id,worktree_key) VALUES('host','wt1','wk_crash')`); err != nil {
			t.Fatal(err)
		}
	}
	// Exit without closing so SQLite leaves pending WAL frames and -shm sidecars on disk.
	os.Exit(0)
}

func TestRefusesRevertedADR037WALUncleanShutdownPreservesBytes(t *testing.T) {
	hashFile := func(path string) [sha256.Size]byte {
		t.Helper()
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if len(raw) == 0 {
			t.Fatalf("expected non-empty %s", path)
		}
		return sha256.Sum256(raw)
	}

	for _, mode := range []string{"wal_only_029", "migrated_029", "migrated_030"} {
		t.Run(mode, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), mode+".db")
			cmd := exec.Command(os.Args[0], "-test.run=^TestADR037WALCrashHelper$")
			cmd.Env = append(os.Environ(), envADR037WALCrashDB+"="+file, envADR037WALCrashMode+"="+mode)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("wal crash helper %s: %v\n%s", mode, err, out)
			}

			paths := []string{file, file + "-wal", file + "-shm"}
			before := make(map[string][sha256.Size]byte, len(paths))
			for _, p := range paths {
				before[p] = hashFile(p)
			}

			opened, err := Open(file, nil)
			if err == nil {
				_ = opened.Close()
				t.Fatalf("opened reverted ADR 037 WAL database in mode %s", mode)
			}
			if !strings.Contains(err.Error(), "reverted ADR 037") || !strings.Contains(err.Error(), "pre-ADR-037 backup") {
				t.Fatalf("unexpected refusal error: %v", err)
			}

			for _, p := range paths {
				after := hashFile(p)
				if after != before[p] {
					t.Fatalf("refused Open modified %s: before=%x after=%x", p, before[p], after)
				}
			}
		})
	}

	t.Run("cleanly closed WAL leaves main unchanged and creates no sidecars", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "clean-wal-029.db")
		db, err := sql.Open("sqlite", file)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
			t.Fatal(err)
		}
		for i, q := range v28Schemas {
			if _, err := db.Exec(q); err != nil {
				t.Fatalf("v28 migration %d: %v", i+1, err)
			}
		}
		if _, err := db.Exec(adr037Migration029SQL); err != nil {
			t.Fatal(err)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		before := hashFile(file)
		opened, err := Open(file, nil)
		if err == nil {
			_ = opened.Close()
			t.Fatal("opened cleanly closed reverted ADR 037 WAL database")
		}
		if after := hashFile(file); after != before {
			t.Fatalf("refused Open modified clean WAL main db: before=%x after=%x", before, after)
		}
		for _, suffix := range []string{"-wal", "-shm"} {
			if _, err := os.Stat(file + suffix); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("refused Open created %s on clean WAL DB: %v", suffix, err)
			}
		}
	})

	t.Run("probe VFS failure returns error", func(t *testing.T) {
		wantErr := errors.New("vfs registration unavailable")
		file := filepath.Join(t.TempDir(), "probe.db")
		if err := os.WriteFile(file, nil, 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file+"-wal", nil, 0600); err != nil {
			t.Fatal(err)
		}
		err := probeRevertedADR037Schema(context.Background(), file, "sqlite", func(fs.FS) (string, io.Closer, error) {
			return "", nil, wantErr
		})
		if !errors.Is(err, wantErr) {
			t.Fatalf("got %v want %v", err, wantErr)
		}
	})
}
