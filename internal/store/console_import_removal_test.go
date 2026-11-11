package store

import (
	"bytes"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// historicalStore builds the fixture from the embedded migrations through its
// requested version. In particular it never runs 024 and then invents old ledger
// tables: schema tests for 017 and 020 still exercise their original SQL.
func historicalStore(t *testing.T, version int) (*Store, *fakeClock, string) {
	t.Helper()
	file := filepath.Join(t.TempDir(), "historical.db")
	db, err := sql.Open("sqlite", file)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	for _, q := range []string{"PRAGMA foreign_keys=ON", "PRAGMA journal_mode=WAL", "PRAGMA synchronous=FULL", "BEGIN"} {
		if _, err = db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	migrations := []string{schema, threadsSchema, namesSchema, shellSchema, tokensSchema, pathKeySchema, waiterSchema, attentionSchema, contactSchema, watchSeenSchema, receiptsSchema, hookSeenSchema, retentionSchema, hostReportSchema, worktreeAliasSchema, receiptDueSchema, consoleAccountsSchema, consoleRevisionSchema, consoleOwnerActionsSchema, consoleAssignmentImportSchema, idleExpirySchema, reducedShellSchema, attentionIdentitySchema}
	for _, migration := range migrations[:version] {
		if _, err = db.Exec(migration); err != nil {
			t.Fatal("could not build historical schema", err)
		}
	}
	stream := newStreamID()
	if _, err = db.Exec("INSERT INTO Meta VALUES(1,?,1)", stream); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("COMMIT"); err != nil {
		t.Fatal(err)
	}
	lock, err := lockDatabase(file + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	clock := &fakeClock{wall: time.Unix(1800000000, 0)}
	s := &Store{db: db, lock: lock, clock: clock, epoch: 1, stream: stream, changed: make(chan struct{})}
	t.Cleanup(func() {
		if s.lock != nil {
			_ = s.Close()
		}
	})
	return s, clock, file
}

func TestFreshDatabaseHasVersion24WithoutImportLedgers(t *testing.T) {
	s, _, _ := testStore(t)
	assertImportRemoval(t, s.db)
}

func assertImportRemoval(t *testing.T, db *sql.DB) {
	t.Helper()
	var version, ledgers int
	if err := db.QueryRow("SELECT (SELECT user_version FROM pragma_user_version),(SELECT COUNT(*) FROM sqlite_schema WHERE name IN ('ConsoleImport','ConsoleAssignmentImport'))").Scan(&version, &ledgers); err != nil || version != 24 || ledgers != 0 {
		t.Fatalf("migration did not reach version 24 without import ledgers: version=%d ledgers=%d error=%v", version, ledgers, err)
	}
}

func populateVersion23(t *testing.T) (*Store, *fakeClock, string) {
	t.Helper()
	s, clock, file := historicalStore(t, 23)
	_, registered := session(t, s, 1)
	repository := registered["repository_id"].(string)
	operator := createTestConsoleAccount(t, s, "Historical operator", "operator")
	member := createTestConsoleAccount(t, s, "Historical member", "member")
	disabled, err := s.CreateConsoleAccount(t.Context(), ConsoleAccountInput{Username: "Historical disabled", PasswordHash: consoleTestHash, Role: "member", Disabled: true})
	if err != nil {
		t.Fatal(err)
	}
	for i, account := range []ConsoleAccount{operator, member, disabled} {
		if _, err = s.db.Exec("UPDATE ConsoleAccount SET created_ms=1000,last_login_ms=2000,auth_version=3,edit_revision=7 WHERE id=?", account.ID); err != nil {
			t.Fatal(err)
		}
		now := clock.Now().UnixMilli()
		if _, err = s.db.Exec("INSERT INTO ConsoleSession VALUES(?,?,3,?,?)", bytes.Repeat([]byte{byte(i + 1)}, 32), account.ID, now, now+time.Hour.Milliseconds()); err != nil {
			t.Fatal(err)
		}
		if _, err = s.db.Exec("INSERT INTO ConsoleImport VALUES('historical-source',?,?,1,zeroblob(32),?)", i, account.ID, now); err != nil {
			t.Fatal(err)
		}
		if _, err = s.db.Exec("INSERT INTO ConsoleAction(actor_kind,target_account_id,action,outcome,created_ms) VALUES('owner',?,'owner_import','accepted',?)", account.ID, now); err != nil {
			t.Fatal(err)
		}
	}
	for _, account := range []ConsoleAccount{member, disabled} {
		if _, err = s.db.Exec("INSERT INTO ConsoleRepositoryAccess VALUES(?,?,'ro')", account.ID, repository); err != nil {
			t.Fatal(err)
		}
		if _, err = s.db.Exec("INSERT INTO ConsoleAssignmentImport VALUES('historical-source',?,?,?,1,zeroblob(32),0)", account.ID, account.ID, repository); err != nil {
			t.Fatal(err)
		}
		if _, err = s.db.Exec("INSERT INTO ConsoleAction(actor_kind,target_account_id,action,outcome,created_ms) VALUES('owner',?,'owner_assignment_import','accepted',0)", account.ID); err != nil {
			t.Fatal(err)
		}
	}
	return s, clock, file
}

func retainedConsoleRows(t *testing.T, db *sql.DB) map[string][][]any {
	t.Helper()
	result := map[string][][]any{}
	for _, table := range []string{"ConsoleAccount", "ConsoleRepositoryAccess", "ConsoleSession", "ConsoleAction", "ConsoleRevision"} {
		result[table] = consoleMigrationRows(t, db, "SELECT * FROM "+table+" ORDER BY rowid")
	}
	return result
}

func TestVersion24PreservesAllAccountsGrantsSessionsAndAuditAcrossReopen(t *testing.T) {
	s, clock, file := populateVersion23(t)
	before := retainedConsoleRows(t, s.db)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s.lock = nil
	for range 2 {
		reopened, err := Open(file, clock)
		if err != nil {
			t.Fatal("migration or reopen failed", err)
		}
		assertImportRemoval(t, reopened.db)
		if !reflect.DeepEqual(retainedConsoleRows(t, reopened.db), before) {
			t.Error("migration or reopen changed retained console records")
		}
		if err = reopened.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestVersion24SecondDropAndCommitFailuresPreserveVersion23AndRetry(t *testing.T) {
	for _, failure := range []string{"second-drop", "commit"} {
		t.Run(failure, func(t *testing.T) {
			s, clock, file := populateVersion23(t)
			if failure == "second-drop" {
				// SQLite refuses dropping a referenced parent with live children. The
				// first DROP has already run; the migration transaction must restore it.
				if _, err := s.db.Exec("CREATE TABLE HistoricalReference(account_id TEXT REFERENCES ConsoleImport(account_id)); INSERT INTO HistoricalReference SELECT account_id FROM ConsoleImport"); err != nil {
					t.Fatal(err)
				}
			}
			before, ledgers := retainedConsoleRows(t, s.db), map[string][][]any{}
			for _, table := range []string{"ConsoleImport", "ConsoleAssignmentImport"} {
				ledgers[table] = consoleMigrationRows(t, s.db, "SELECT * FROM "+table+" ORDER BY rowid")
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			s.lock = nil
			fault := &storageFault{discover: failure == "commit"}
			name := registerFaultDriver(fault)
			failed, err := openDatabase(file, clock, name)
			if failed != nil || err == nil || (failure == "commit" && !errors.Is(err, errStorageFault)) {
				t.Fatal("failed migration returned a store or lost its failure", err)
			}
			raw, err := sql.Open("sqlite", file)
			if err != nil {
				t.Fatal(err)
			}
			var version int
			if err = raw.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 23 {
				t.Fatal("failed migration changed schema version", err)
			}
			if !reflect.DeepEqual(retainedConsoleRows(t, raw), before) {
				t.Fatal("failed migration changed retained console records")
			}
			for table, rows := range ledgers {
				if !reflect.DeepEqual(consoleMigrationRows(t, raw, "SELECT * FROM "+table+" ORDER BY rowid"), rows) {
					t.Fatal("failed migration lost ledger rows", table)
				}
			}
			if failure == "second-drop" {
				if _, err = raw.Exec("DROP TABLE HistoricalReference"); err != nil {
					t.Fatal(err)
				}
			}
			if err = raw.Close(); err != nil {
				t.Fatal(err)
			}
			fault.discover = false
			retried, err := openDatabase(file, clock, name)
			if err != nil {
				t.Fatal("migration retry failed", err)
			}
			defer func() { _ = retried.Close() }()
			assertImportRemoval(t, retried.db)
			if !reflect.DeepEqual(retainedConsoleRows(t, retried.db), before) {
				t.Fatal("retry changed retained console records")
			}
		})
	}
}
