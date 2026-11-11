package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
)

// Synthetic syntax-valid record. These storage tests never run a KDF and never
// inspect a source account database.
const consoleTestHash = "scrypt$16384$8$1$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

// removeAttentionIdentitySchema undoes migration 023, which a fixture that reconstructs an older
// version must do first: SQLite cannot add a column that is already there, nor drop one that a
// trigger names.
const removeAttentionIdentitySchema = "DROP TRIGGER delivery_attention_seq;\nALTER TABLE Delivery DROP COLUMN attention_seq;\n"

// Old-schema fixtures undo only this migration before reconstructing their
// historical version. Fresh-migration tests remain backed by captured old SQL.
const removeConsoleRevisionSchema = `
DROP TABLE ConsoleAssignmentImport;
DROP TRIGGER console_repository_change;
DROP TRIGGER console_revision_account_insert;
DROP TRIGGER console_revision_account_update;
DROP TRIGGER console_revision_grant_insert;
DROP TRIGGER console_revision_grant_update;
DROP TRIGGER console_revision_grant_delete;
DROP TRIGGER console_revision_repository_insert;
DROP TRIGGER console_revision_repository_update;
DROP TRIGGER console_revision_repository_delete;
DROP TRIGGER console_revision_repositoryalias_insert;
DROP TRIGGER console_revision_repositoryalias_update;
DROP TRIGGER console_revision_repositoryalias_delete;
DROP TRIGGER console_revision_agent_insert;
DROP TRIGGER console_revision_agent_update;
DROP TRIGGER console_revision_agent_delete;
DROP TRIGGER console_revision_execution_insert;
DROP TRIGGER console_revision_execution_update;
DROP TRIGGER console_revision_execution_delete;
DROP TRIGGER console_revision_claim_insert;
DROP TRIGGER console_revision_claim_update;
DROP TRIGGER console_revision_claim_delete;
DROP TRIGGER console_revision_message_insert;
DROP TRIGGER console_revision_message_update;
DROP TRIGGER console_revision_message_delete;
DROP TRIGGER console_revision_delivery_insert;
DROP TRIGGER console_revision_delivery_update;
DROP TRIGGER console_revision_delivery_delete;
DROP TRIGGER console_revision_ack_insert;
DROP TRIGGER console_revision_ack_update;
DROP TRIGGER console_revision_ack_delete;
DROP TRIGGER console_revision_hostreport_insert;
DROP TRIGGER console_revision_hostreport_update;
DROP TRIGGER console_revision_hostreport_delete;
DROP VIEW ConsoleRepositoryChange;
DROP TABLE ConsoleRevision;
`

const removeConsoleSchema = removeConsoleRevisionSchema + `
DROP TRIGGER console_authorship_insert;
DROP TRIGGER console_authorship_immutable;
DROP TRIGGER console_operation_message_delete;
DROP TRIGGER console_operation_delivery_delete;
DROP TRIGGER console_operation_recipient_delete;
DROP TABLE ConsoleOperation;
DROP TABLE ConsoleAction;
DROP TABLE ConsoleImport;
DROP TABLE ConsoleSession;
DROP TABLE ConsoleRepositoryAccess;
ALTER TABLE Message DROP COLUMN sender_account_id;
ALTER TABLE Message DROP COLUMN sender_username;
ALTER TABLE Message DROP COLUMN sender_display_name;
DROP TABLE ConsoleAccount;
`

func createTestConsoleAccount(t *testing.T, s *Store, username, role string) ConsoleAccount {
	t.Helper()
	a, err := s.CreateConsoleAccount(context.Background(), ConsoleAccountInput{Username: username, PasswordHash: consoleTestHash, Role: role, DisplayName: "Display " + username, UILocale: "pl", CorrespondenceLocale: "en"})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func consoleError(t *testing.T, err error, code string) {
	t.Helper()
	var domain *Error
	if !errors.As(err, &domain) || domain.Code != code {
		t.Fatalf("want %s, got %v", code, err)
	}
}

func TestConsoleAccountIdentityStorageAndSafeProjection(t *testing.T) {
	s, clock, _ := testStore(t)
	a := createTestConsoleAccount(t, s, "Caf\u00e9", "member")
	if !strings.HasPrefix(a.ID, "ca_") || a.Username != "Caf\u00e9" || a.LoginKey != "caf\u00e9" || a.PasswordHash != consoleTestHash || a.AuthVersion != 1 || a.EditRevision != 1 || a.LastLoginMS != nil || a.CreatedMS != clock.Now().UnixMilli() || !a.Enabled() {
		t.Fatal("account identity/defaults were not retained")
	}
	got, err := s.LookupConsoleAccount(context.Background(), "CAFE\u0301")
	if err != nil || got.ID != a.ID {
		t.Fatal("normalized lookup did not select the original identity", err)
	}
	_, err = s.CreateConsoleAccount(context.Background(), ConsoleAccountInput{Username: "CAFE\u0301", PasswordHash: consoleTestHash, Role: "operator"})
	consoleError(t, err, "conflict")
	projection, err := json.Marshal(got)
	if err != nil || strings.Contains(string(projection), consoleTestHash) || strings.Contains(string(projection), "login_key") || strings.Contains(string(projection), "password_hash") {
		t.Fatal("account projection contains credential data")
	}
	if _, err = s.db.Exec("UPDATE ConsoleAccount SET last_login_ms=123 WHERE id=?", a.ID); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetConsoleAccount(context.Background(), a.ID)
	if err != nil || got.LastLoginMS == nil || *got.LastLoginMS != 123 {
		t.Fatal("nullable historical login time was lost", err)
	}
	var agents, executions int
	if err = s.db.QueryRow("SELECT (SELECT COUNT(*) FROM Agent),(SELECT COUNT(*) FROM Execution)").Scan(&agents, &executions); err != nil || agents != 0 || executions != 0 {
		t.Fatal("an account created an agent session", err)
	}
}

func TestConsoleCreationRefusesInvalidRecordsAndDefaultsToDisabledNone(t *testing.T) {
	s, _, _ := testStore(t)
	for _, mutate := range []func(*ConsoleAccountInput){
		func(i *ConsoleAccountInput) { i.Username = " leading" },
		func(i *ConsoleAccountInput) { i.PasswordHash = "invalid" },
		func(i *ConsoleAccountInput) { i.DisplayName = strings.Repeat("x", 129) },
		func(i *ConsoleAccountInput) { i.UILocale = "en-US" },
		func(i *ConsoleAccountInput) { i.CorrespondenceLocale = "ar bn" },
		func(i *ConsoleAccountInput) { i.Role = "admin" },
	} {
		input := ConsoleAccountInput{Username: "Member", PasswordHash: consoleTestHash, Role: "member"}
		mutate(&input)
		_, err := s.CreateConsoleAccount(context.Background(), input)
		consoleError(t, err, "bad_request")
	}
	a, err := s.CreateConsoleAccount(context.Background(), ConsoleAccountInput{Username: "Default", PasswordHash: consoleTestHash})
	if err != nil || a.Role != "none" || !a.Disabled || a.Enabled() {
		t.Fatal("omitted role acquired authority", err)
	}
	_, err = s.LookupConsoleAccount(context.Background(), "bad\nname")
	consoleError(t, err, "bad_request")
	_, err = s.LookupConsoleAccount(context.Background(), "absent")
	consoleError(t, err, "not_found")
	_, err = s.GetConsoleAccount(context.Background(), "absent")
	consoleError(t, err, "not_found")
	for _, tc := range []struct {
		account ConsoleAccount
		enabled bool
	}{
		{ConsoleAccount{Role: "operator"}, true},
		{ConsoleAccount{Role: "member"}, true},
		{ConsoleAccount{Role: "member", Disabled: true}, false},
		{ConsoleAccount{Role: "none"}, false},
		{ConsoleAccount{Role: "unknown"}, false},
		{ConsoleAccount{}, false},
	} {
		if tc.account.Enabled() != tc.enabled {
			t.Fatal("role did not deny by default")
		}
	}
}

func TestConsoleProfileRevisionDoesNotChangeCredentials(t *testing.T) {
	s, _, _ := testStore(t)
	a := createTestConsoleAccount(t, s, "Member", "member")
	updated, err := s.UpdateConsoleProfile(context.Background(), a.ID, a.EditRevision, "<b>text only</b>", "zh-Hant", "")
	if err != nil || updated.EditRevision != 2 || updated.AuthVersion != 1 || updated.DisplayName != "<b>text only</b>" || updated.UILocale != "zh-Hant" || updated.CorrespondenceLocale != "" || updated.PasswordHash != a.PasswordHash || updated.Role != a.Role || updated.Username != a.Username {
		t.Fatal("profile update changed authority or lost text", err)
	}
	_, err = s.UpdateConsoleProfile(context.Background(), a.ID, a.EditRevision, "stale", "", "")
	consoleError(t, err, "conflict")
	_, err = s.UpdateConsoleProfile(context.Background(), "missing", 1, "", "", "")
	consoleError(t, err, "not_found")
	_, err = s.UpdateConsoleProfile(context.Background(), a.ID, 2, "", "bad", "")
	consoleError(t, err, "bad_request")
}

func TestConsoleAccessGrantsAreStrictAndAtomic(t *testing.T) {
	s, _, _ := testStore(t)
	_, out := session(t, s, 1)
	repo := out["repository_id"].(string)
	a := createTestConsoleAccount(t, s, "Member", "member")
	for _, account := range []string{a.ID, "unknown"} {
		mode, err := s.ConsoleRepositoryMode(context.Background(), account, repo)
		if err != nil || mode != "" {
			t.Fatal("missing grant acquired access", err)
		}
	}
	if _, err := s.db.Exec("INSERT INTO ConsoleSession VALUES(zeroblob(32),?,1,0,43200000)", a.ID); err != nil {
		t.Fatal(err)
	}
	updated, err := s.ReplaceConsoleAccess(context.Background(), a.ID, 1, "member", false, []ConsoleGrant{{repo, "ro"}})
	if err != nil || updated.AuthVersion != 2 || updated.EditRevision != 2 {
		t.Fatal("access revision did not advance atomically", err)
	}
	mode, err := s.ConsoleRepositoryMode(context.Background(), a.ID, repo)
	if err != nil || mode != "ro" {
		t.Fatal("ro grant was not retained", err)
	}
	var count int
	if err = s.db.QueryRow("SELECT COUNT(*) FROM ConsoleSession WHERE account_id=?", a.ID).Scan(&count); err != nil || count != 0 {
		t.Fatal("old sessions survived access change", err)
	}
	before := databaseState(t, s)
	_, err = s.ReplaceConsoleAccess(context.Background(), a.ID, 2, "member", false, []ConsoleGrant{{"unknown", "rw"}})
	if err == nil || databaseState(t, s) != before {
		t.Fatal("missing repository committed partial access change")
	}
	for _, grants := range [][]ConsoleGrant{{{repo, "read"}}, {{repo, "ro"}, {repo, "rw"}}, {{"", "ro"}}} {
		_, err = s.ReplaceConsoleAccess(context.Background(), a.ID, 2, "member", false, grants)
		consoleError(t, err, "bad_request")
	}
	_, err = s.ReplaceConsoleAccess(context.Background(), a.ID, 2, "operator", false, []ConsoleGrant{{repo, "rw"}})
	consoleError(t, err, "bad_request")
	_, err = s.ReplaceConsoleAccess(context.Background(), a.ID, 2, "unknown", false, nil)
	consoleError(t, err, "bad_request")
	_, err = s.ReplaceConsoleAccess(context.Background(), "missing", 1, "member", false, nil)
	consoleError(t, err, "not_found")
	_, err = s.ReplaceConsoleAccess(context.Background(), a.ID, 1, "member", false, nil)
	consoleError(t, err, "conflict")
	updated, err = s.ReplaceConsoleAccess(context.Background(), a.ID, 2, "member", false, []ConsoleGrant{{repo, "rw"}})
	if err != nil {
		t.Fatal(err)
	}
	mode, err = s.ConsoleRepositoryMode(context.Background(), a.ID, repo)
	if err != nil || mode != "rw" {
		t.Fatal("rw mode lost", err)
	}
	updated, err = s.ReplaceConsoleAccess(context.Background(), a.ID, updated.EditRevision, "member", true, []ConsoleGrant{{repo, "rw"}})
	if err != nil {
		t.Fatal(err)
	}
	mode, err = s.ConsoleRepositoryMode(context.Background(), a.ID, repo)
	if err != nil || mode != "" {
		t.Fatal("disabled account retained grant authority", err)
	}
	updated, err = s.ReplaceConsoleAccess(context.Background(), a.ID, updated.EditRevision, "none", false, nil)
	if err != nil || !updated.Disabled || updated.Enabled() {
		t.Fatal("none was enabled", err)
	}
}

func TestConsoleMalformedStoredRolesAndModesDeny(t *testing.T) {
	s, _, _ := testStore(t)
	_, out := session(t, s, 1)
	repo := out["repository_id"].(string)
	a := createTestConsoleAccount(t, s, "Member", "member")
	if _, err := s.db.Exec("PRAGMA ignore_check_constraints=ON"); err != nil {
		t.Fatal(err)
	}
	for _, roleMode := range [][2]string{{"unknown", "rw"}, {"member", "admin"}, {"none", "rw"}} {
		if _, err := s.db.Exec("UPDATE ConsoleAccount SET role=?,disabled=0 WHERE id=?", roleMode[0], a.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec("INSERT OR REPLACE INTO ConsoleRepositoryAccess VALUES(?,?,?)", a.ID, repo, roleMode[1]); err != nil {
			t.Fatal(err)
		}
		mode, err := s.ConsoleRepositoryMode(context.Background(), a.ID, repo)
		if err != nil || mode != "" {
			t.Fatal("malformed stored authority was accepted", err)
		}
	}
}

func TestConsoleConcurrentRevisionsPermitOneAccessEdit(t *testing.T) {
	s, _, _ := testStore(t)
	a := createTestConsoleAccount(t, s, "Member", "member")
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for range 2 {
		wg.Go(func() {
			_, err := s.ReplaceConsoleAccess(context.Background(), a.ID, 1, "member", false, nil)
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	success, conflicts := 0, 0
	for err := range errs {
		if err == nil {
			success++
		} else {
			consoleError(t, err, "conflict")
			conflicts++
		}
	}
	if success != 1 || conflicts != 1 {
		t.Fatal("concurrent stale revisions both changed access")
	}
}

func TestConsoleSchemaRefusesMalformedRecords(t *testing.T) {
	s, _, _ := testStore(t)
	_, out := session(t, s, 1)
	repo := out["repository_id"].(string)
	a := createTestConsoleAccount(t, s, "Member", "member")
	for _, test := range []struct {
		query string
		args  []any
	}{
		{"INSERT INTO ConsoleRepositoryAccess VALUES(?,?,'admin')", []any{a.ID, repo}},
		{"INSERT INTO ConsoleRepositoryAccess VALUES(?,'missing','ro')", []any{a.ID}},
		{"INSERT INTO ConsoleSession VALUES(NULL,?,1,0,43200000)", []any{a.ID}},
		{"INSERT INTO ConsoleSession VALUES(zeroblob(31),?,1,0,43200000)", []any{a.ID}},
		{"INSERT INTO ConsoleSession VALUES(zeroblob(32),?,1,0,43200001)", []any{a.ID}},
		{"UPDATE ConsoleAccount SET role='admin' WHERE id=?", []any{a.ID}},
		{"UPDATE ConsoleAccount SET auth_version=0 WHERE id=?", []any{a.ID}},
		{"UPDATE ConsoleAccount SET edit_revision=0 WHERE id=?", []any{a.ID}},
		{"UPDATE ConsoleAccount SET role='none',disabled=0 WHERE id=?", []any{a.ID}},
	} {
		if _, err := s.db.Exec(test.query, test.args...); err == nil {
			t.Fatal("malformed storage record accepted")
		}
	}
	if _, err := s.db.Exec("INSERT INTO ConsoleRepositoryAccess VALUES(?,?,'ro')", a.ID, repo); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("INSERT INTO ConsoleRepositoryAccess VALUES(?,?,'rw')", a.ID, repo); err == nil {
		t.Fatal("duplicate grant accepted")
	}
}

func TestConsoleMessageAuthorshipIsCompleteImmutableAndHistorical(t *testing.T) {
	s, _, _ := testStore(t)
	r, out := session(t, s, 1)
	repo := out["repository_id"].(string)
	a := createTestConsoleAccount(t, s, "Member", "member")
	insert := `INSERT INTO Message(id,sender_execution_id,subject,body,created_ms,repository_id,thread_id,sender_account_id,sender_username,sender_display_name) VALUES(?,?,'subject','body',0,?,'thread',?,?,?)`
	for _, attribution := range [][3]any{{a.ID, nil, a.DisplayName}, {nil, a.Username, a.DisplayName}, {a.ID, a.Username, nil}} {
		if _, err := s.db.Exec(insert, "invalid", r.ExecutionID, repo, attribution[0], attribution[1], attribution[2]); err == nil {
			t.Fatal("partial authorship was accepted")
		}
	}
	if _, err := s.db.Exec(insert, "authored", r.ExecutionID, repo, a.ID, a.Username, a.DisplayName); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(insert, "legacy", r.ExecutionID, repo, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	for _, edit := range []string{
		"UPDATE Message SET sender_account_id=NULL WHERE id='authored'",
		"UPDATE Message SET sender_username='forged' WHERE id='authored'",
		"UPDATE Message SET sender_display_name='forged' WHERE id='authored'",
		"UPDATE Message SET sender_account_id=NULL,sender_username=NULL,sender_display_name=NULL WHERE id='authored'",
		"UPDATE Message SET sender_account_id='" + a.ID + "',sender_username='Member',sender_display_name='new' WHERE id='legacy'",
	} {
		if _, err := s.db.Exec(edit); err == nil {
			t.Fatal("historical authorship was rewritten")
		}
	}
	if _, err := s.UpdateConsoleProfile(context.Background(), a.ID, 1, "Updated display", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReplaceConsoleAccess(context.Background(), a.ID, 2, "none", false, nil); err != nil {
		t.Fatal(err)
	}
	var author, username, display string
	if err := s.db.QueryRow("SELECT sender_account_id,sender_username,sender_display_name FROM Message WHERE id='authored'").Scan(&author, &username, &display); err != nil || author != a.ID || username != a.Username || display != a.DisplayName {
		t.Fatal("profile/role change altered at-send metadata", err)
	}
	var legacyAuthor, legacyUsername, legacyDisplay sql.NullString
	if err := s.db.QueryRow("SELECT sender_account_id,sender_username,sender_display_name FROM Message WHERE id='legacy'").Scan(&legacyAuthor, &legacyUsername, &legacyDisplay); err != nil || legacyAuthor.Valid || legacyUsername.Valid || legacyDisplay.Valid {
		t.Fatal("legacy mail was attributed to a person", err)
	}
	if _, err := s.db.Exec("INSERT INTO Repository(id,root_commit) VALUES('moved-repository','root')"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("UPDATE Message SET sender_username=sender_username,repository_id='moved-repository' WHERE id='authored'"); err != nil {
		t.Fatal("identity-preserving message repair was refused", err)
	}
	var moved string
	if err := s.db.QueryRow("SELECT repository_id,sender_account_id,sender_username,sender_display_name FROM Message WHERE id='authored'").Scan(&moved, &author, &username, &display); err != nil || moved != "moved-repository" || author != a.ID || username != a.Username || display != a.DisplayName {
		t.Fatal("repository move lost authorship", err)
	}
}

func TestConsoleSessionAndImportSchemaPreserveIndependentRecords(t *testing.T) {
	s, _, _ := historicalStore(t, 23)
	a := createTestConsoleAccount(t, s, "One", "member")
	b := createTestConsoleAccount(t, s, "Two", "member")
	insert := "INSERT INTO ConsoleImport VALUES(?,?,?,1,zeroblob(32),0)"
	if _, err := s.db.Exec(insert, "source", "10", a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(insert, "source", "10", b.ID); err == nil {
		t.Fatal("a source identity was remapped")
	}
	if _, err := s.db.Exec(insert, "other-source", "10", b.ID); err != nil {
		t.Fatal("source row IDs became global identities", err)
	}
	for _, query := range []string{
		"INSERT INTO ConsoleImport VALUES('source','11','" + a.ID + "',1,zeroblob(32),0)",
		"INSERT INTO ConsoleImport VALUES('source','12','missing',1,zeroblob(32),0)",
		"UPDATE ConsoleImport SET fingerprint_version=2",
		"UPDATE ConsoleImport SET source_fingerprint=zeroblob(31)",
	} {
		if _, err := s.db.Exec(query); err == nil {
			t.Fatal("malformed or duplicate import lineage was accepted")
		}
	}
	if _, err := s.db.Exec("INSERT INTO ConsoleSession VALUES(zeroblob(32),?,1,10,43200010)", a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("INSERT INTO ConsoleSession VALUES(zeroblob(32),?,1,10,43200010)", b.ID); err == nil {
		t.Fatal("a persisted session digest identified two accounts")
	}
}

func TestConsoleMigrationPreservesLegacyMailAndRetryNamespaces(t *testing.T) {
	s, clock, file := historicalStore(t, 23)
	_, registered := session(t, s, 1)
	repo := registered["repository_id"].(string)
	r := Request{RepositoryID: repo, Key: "legacy-retry", To: []string{registered["agent"].(map[string]any)["id"].(string)}, Body: "unattributed shared mail"}
	accepted := call(t, s, "ui_messages", r)
	if _, err := s.db.Exec(removeAttentionIdentitySchema + removeConsoleSchema + "PRAGMA user_version=16"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s.lock = nil
	reopened, err := Open(file, clock)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	var author, username, display sql.NullString
	if err = reopened.db.QueryRow("SELECT sender_account_id,sender_username,sender_display_name FROM Message WHERE id=?", accepted["message_id"]).Scan(&author, &username, &display); err != nil || author.Valid || username.Valid || display.Valid {
		t.Fatal("migration attributed old shared mail", err)
	}
	var accounts, namedOperations, legacyOperations int
	if err = reopened.db.QueryRow("SELECT (SELECT COUNT(*) FROM ConsoleAccount),(SELECT COUNT(*) FROM ConsoleOperation),(SELECT COUNT(*) FROM Operation)").Scan(&accounts, &namedOperations, &legacyOperations); err != nil || accounts != 0 || namedOperations != 0 || legacyOperations != 1 {
		t.Fatal("migration adopted legacy identities or retry records", err)
	}
	if retried := call(t, reopened, "ui_messages", r); retried["message_id"] != accepted["message_id"] {
		t.Fatal("migration changed an accepted legacy retry")
	}
}
