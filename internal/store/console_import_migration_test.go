package store

import (
	"bytes"
	"database/sql"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestConsoleAssignmentMigrationFromPopulatedVersion19PreservesHistory(t *testing.T) {
	s, clock, file := testStore(t)
	// Version 20 only adds the assignment ledger. Remove it before populating
	// the fixture so every retained table and record has the actual v19 shape.
	if _, err := s.db.Exec("DROP TABLE ConsoleAssignmentImport; PRAGMA user_version=19"); err != nil {
		t.Fatal("could not reconstruct version 19 schema")
	}
	first, registered := session(t, s, 1)
	second, recipient := session(t, s, 2)
	repository := registered["repository_id"].(string)
	call(t, s, "claim", claimRequest(first, "historical-claim", "historical/file", "exact"))

	accounts := make([]ConsoleAccount, 4)
	for i, input := range []ConsoleAccountInput{
		{Username: "Migration operator", Role: "operator", DisplayName: "Historical operator", UILocale: "en", CorrespondenceLocale: "pl"},
		{Username: "Migration member", Role: "member", DisplayName: "Historical member", UILocale: "pl", CorrespondenceLocale: "en"},
		{Username: "Migration disabled", Role: "member", Disabled: true},
		{Username: "Migration quarantined", Role: "none"},
	} {
		input.PasswordHash = importPassword(byte(i + 1))
		account, err := s.CreateConsoleAccount(t.Context(), input)
		if err != nil {
			t.Fatal("could not populate historical account")
		}
		if _, err = s.db.Exec("UPDATE ConsoleAccount SET created_ms=1000,last_login_ms=2000,auth_version=3,edit_revision=7 WHERE id=?", account.ID); err != nil {
			t.Fatal("could not preserve historical account fields")
		}
		accounts[i], err = s.GetConsoleAccount(t.Context(), account.ID)
		if err != nil {
			t.Fatal("could not read historical account")
		}
	}
	for i, mode := range []string{"rw", "ro"} {
		if _, err := s.db.Exec("INSERT INTO ConsoleRepositoryAccess VALUES(?,?,?)", accounts[i+1].ID, repository, mode); err != nil {
			t.Fatal("could not populate historical grants")
		}
	}
	now := clock.Now().UnixMilli()
	for i, account := range accounts[:2] {
		if _, err := s.db.Exec("INSERT INTO ConsoleSession VALUES(?,?,?,?,?)", bytes.Repeat([]byte{byte(i + 1)}, 32), account.ID, account.AuthVersion, now, now+int64(time.Hour/time.Millisecond)); err != nil {
			t.Fatal("could not populate historical session")
		}
	}
	if _, err := s.db.Exec("INSERT INTO ConsoleImport VALUES(?,?,?,1,?,?)", "synthetic-version19", "42", accounts[1].ID, bytes.Repeat([]byte{3}, 32), now); err != nil {
		t.Fatal("could not populate historical account import ledger")
	}
	request := consoleMessageRequest{RepositoryID: repository, To: []string{recipient["agent"].(map[string]any)["id"].(string)}, Subject: "Historical account message", Body: "Untrusted historical text"}
	accepted := acceptedConsoleSend(t, s, accounts[0], "historical-send", request)
	if _, err := s.db.Exec(`INSERT INTO ConsoleOperation(account_id,key,request_hash,origin_repository_id,message_id,result_json,recipient_ids_json,created_ms,tombstone)
 VALUES(?,?,?, ?,NULL,NULL,'[]',?,1)`, accounts[1].ID, "historical-tombstone", bytes.Repeat([]byte{4}, 32), repository, now); err != nil {
		t.Fatal("could not populate historical retry tombstone")
	}
	if _, err := s.db.Exec(`INSERT INTO ConsoleAction(actor_account_id,target_account_id,action,outcome,created_ms,actor_kind)
 VALUES(?,?,'login','accepted',?,'account'),(NULL,?,'owner_provision','accepted',?,'owner')`, accounts[0].ID, accounts[0].ID, now, accounts[0].ID, now); err != nil {
		t.Fatal("could not populate historical account and owner attribution")
	}
	call(t, s, "inbox", second)
	priorRecovery := clock.Now().Add(time.Minute).UnixMilli()
	if _, err := s.db.Exec("UPDATE Execution SET recovery_deadline=? WHERE id=?", priorRecovery, second.ExecutionID); err != nil {
		t.Fatal("could not populate recovering execution")
	}
	if _, err := s.db.Exec("INSERT INTO Cursor(consumer_id,execution_id,seq) VALUES('historical-monitor',?,0)", first.ExecutionID); err != nil {
		t.Fatal("could not populate historical cursor")
	}
	var stream string
	var epoch, version int64
	if err := s.db.QueryRow("SELECT stream_id,boot_epoch,(SELECT user_version FROM pragma_user_version) FROM Meta").Scan(&stream, &epoch, &version); err != nil || version != 19 {
		t.Fatal("fixture is not version 19")
	}
	before := consoleMigrationState(t, s)
	for _, table := range []string{"ConsoleAccount", "ConsoleRepositoryAccess", "ConsoleSession", "ConsoleImport", "ConsoleOperation", "ConsoleAction", "ConsoleRevision", "Claim", "Message", "Delivery", "Event", "Operation", "Cursor"} {
		if len(before[table]) == 0 {
			t.Fatal("historical fixture lacks required records", table)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal("could not close populated version 19 fixture")
	}
	s.lock = nil
	reopened, err := Open(file, clock)
	if err != nil {
		t.Fatal("could not upgrade populated version 19 fixture")
	}
	defer func() { _ = reopened.Close() }()
	after := consoleMigrationState(t, reopened)
	if !reflect.DeepEqual(before, after) {
		// Never print row contents: credentials, session digests and fingerprints
		// are compared in memory and must not escape through a failure report.
		for table, rows := range before {
			if !reflect.DeepEqual(rows, after[table]) {
				t.Errorf("migration altered historical records in %s", table)
			}
		}
		t.Fatal("version 20 did not preserve version 19 records")
	}
	var nextStream string
	var nextEpoch, assignments int64
	if err = reopened.db.QueryRow("SELECT stream_id,boot_epoch,(SELECT user_version FROM pragma_user_version),(SELECT COUNT(*) FROM ConsoleAssignmentImport) FROM Meta").Scan(&nextStream, &nextEpoch, &version, &assignments); err != nil || version != 20 || assignments != 0 || nextStream != stream || nextEpoch != epoch+1 {
		t.Fatal("upgrade lost stream identity, boot recovery or the new empty ledger")
	}
	for execution, want := range map[string]int64{first.ExecutionID: clock.Now().Add(120 * time.Second).UnixMilli(), second.ExecutionID: priorRecovery} {
		var state string
		var deadline int64
		if err = reopened.db.QueryRow("SELECT state,recovery_deadline FROM Execution WHERE id=?", execution).Scan(&state, &deadline); err != nil || state != "active" || deadline != want {
			t.Fatal("normal startup recovery changed during migration")
		}
	}
	var author string
	if err = reopened.db.QueryRow("SELECT sender_account_id FROM Message WHERE id=?", accepted.MessageID).Scan(&author); err != nil || author != accounts[0].ID {
		t.Fatal("migration lost immutable account authorship")
	}
	for _, statement := range []string{
		`INSERT INTO ConsoleAssignmentImport VALUES('synthetic-v20','1','missing-account','rp_missing',1,zeroblob(32),0)`,
		`INSERT INTO ConsoleAssignmentImport VALUES('synthetic-v20','1',?,'rp_retained',2,zeroblob(32),0)`,
		`INSERT INTO ConsoleAssignmentImport VALUES('synthetic-v20','1',?,'rp_retained',1,zeroblob(31),0)`,
	} {
		args := []any{accounts[1].ID}
		if !strings.Contains(statement, "?") {
			args = nil
		}
		if _, err = reopened.db.Exec(statement, args...); err == nil {
			t.Fatal("new assignment ledger failed to enforce its storage contract")
		}
	}
	if _, err = reopened.db.Exec("INSERT INTO ConsoleAssignmentImport VALUES('synthetic-v20','1',?,'rp_retained',1,zeroblob(32),0)", accounts[1].ID); err != nil {
		t.Fatal("valid assignment ledger record was refused after upgrade")
	}
	if _, err = reopened.db.Exec("INSERT INTO ConsoleAssignmentImport VALUES('synthetic-v20','1',?,'rp_other',1,zeroblob(32),0)", accounts[1].ID); err == nil {
		t.Fatal("new assignment ledger accepted duplicate source identity")
	}
}

// Every v19 table, including credential-bearing and search storage, is compared
// without exposing it. Only normal startup recovery's nullable deadline is
// normalized here; its exact values and the Meta epoch are asserted above.
func consoleMigrationState(t *testing.T, s *Store) map[string][][]any {
	t.Helper()
	names := consoleMigrationRows(t, s.db, "SELECT name FROM sqlite_schema WHERE type='table' AND name NOT IN ('Meta','ConsoleAssignmentImport') ORDER BY name")
	out := map[string][][]any{}
	for _, entry := range names {
		table := entry[0].(string)
		var count int
		if err := s.db.QueryRow("SELECT COUNT(*) FROM pragma_table_info(?)", table).Scan(&count); err != nil {
			t.Fatal("could not inspect historical table shape")
		}
		order := make([]string, count)
		for i := range order {
			order[i] = strconv.Itoa(i + 1)
		}
		out[table] = consoleMigrationRows(t, s.db, `SELECT * FROM "`+strings.ReplaceAll(table, `"`, `""`)+`" ORDER BY `+strings.Join(order, ","))
	}
	out["schema"] = consoleMigrationRows(t, s.db, "SELECT type,name,tbl_name,sql FROM sqlite_schema WHERE tbl_name<>'ConsoleAssignmentImport' ORDER BY type,name")
	return out
}

func consoleMigrationRows(t *testing.T, db *sql.DB, query string) [][]any {
	t.Helper()
	rows, err := db.Query(query)
	if err != nil {
		t.Fatal("could not snapshot historical migration records")
	}
	defer func() { _ = rows.Close() }()
	columns, err := rows.Columns()
	if err != nil {
		t.Fatal("could not inspect historical migration columns")
	}
	var result [][]any
	for rows.Next() {
		values, targets := make([]any, len(columns)), make([]any, len(columns))
		for i := range targets {
			targets[i] = &values[i]
		}
		if err = rows.Scan(targets...); err != nil {
			t.Fatal("could not snapshot historical migration row")
		}
		for i, column := range columns {
			if column == "recovery_deadline" {
				values[i] = nil
			}
		}
		result = append(result, values)
	}
	if rows.Err() != nil {
		t.Fatal("could not complete historical migration snapshot")
	}
	return result
}
