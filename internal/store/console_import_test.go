package store

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/accountimport"
)

func importPassword(fill byte) string {
	return strings.Join([]string{"scrypt", "16384", "8", "1", base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{fill}, 16)), base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{fill}, 32))}, "$")
}

func importBatch() accountimport.Batch {
	return accountimport.Batch{Manifest: accountimport.Manifest{Version: 1, SourceID: "synthetic-accounts", Table: "accounts", TimestampEncoding: "unix_milliseconds", TimestampZone: "UTC"}, Rows: []accountimport.Row{{ID: "1", Username: "Imported", PasswordHash: importPassword(0), Role: "member", DisplayName: "Imported display", UILocale: "pl", CorrespondenceLocale: "en", CreatedMS: 1000}}}
}

func importAccount(t *testing.T, s *Store) (accountimport.Batch, ConsoleAccount) {
	t.Helper()
	batch := importBatch()
	report, err := s.ImportConsoleAccounts(t.Context(), batch, false)
	if err != nil || report.Created != 1 || report.Skipped != 0 {
		t.Fatal("synthetic account import failed", err)
	}
	a, err := s.GetConsoleAccount(t.Context(), report.Rows[0].AccountID)
	if err != nil {
		t.Fatal("imported account unavailable", err)
	}
	return batch, a
}

func importTransfer(repository string) ConsoleAssignmentTransfer {
	return ConsoleAssignmentTransfer{Version: 1, SourceID: "synthetic-accounts", Rows: []ConsoleAssignmentRow{{ID: "1", AccountRowID: "1", ProjectID: "7", Role: "viewer"}}, Projects: map[string]string{"7": repository}}
}

func TestConsoleImportDryRunPreservesAllStateAndApplyPreservesHistory(t *testing.T) {
	s, clock, _ := testStore(t)
	request, _ := session(t, s, 1)
	call(t, s, "claim", claimRequest(request, "synthetic-held", "held", "exact"))
	clock.advance(48 * time.Hour)
	batch := importBatch()
	batch.Manifest.Renames = map[string]string{"1": "Reviewed"}
	batch.Manifest.Locales = map[string]string{"unsupported": ""}
	batch.Rows[0].CorrespondenceLocale = "unsupported"
	for i, role := range []string{"admin", "unrecognized-synthetic-role", "member"} {
		row := batch.Rows[0]
		row.ID, row.Username, row.Role = strconv.Itoa(i+2), "Imported"+strconv.Itoa(i+2), role
		row.Disabled = i == 2
		if i == 0 {
			last := int64(2000)
			row.LastLoginMS = &last
		}
		batch.Rows = append(batch.Rows, row)
	}
	before, changed := databaseState(t, s), s.Changes()
	report, err := s.ImportConsoleAccounts(t.Context(), batch, true)
	if err != nil || report.Created != 4 || report.Quarantined != 1 || !report.DryRun || !report.Rows[0].Renamed || !report.Rows[0].LocaleResolved {
		t.Fatal("dry-run did not report reviewed import", err)
	}
	if databaseState(t, s) != before || s.Changes() != changed {
		t.Fatal("dry-run changed account or coordination expiry state")
	}
	for _, row := range report.Rows {
		if row.AccountID != "" {
			t.Fatal("dry-run claimed to create an identity")
		}
	}
	report, err = s.ImportConsoleAccounts(t.Context(), batch, false)
	if err != nil || report.DryRun || report.Created != 4 || s.Changes() == changed {
		t.Fatal("apply did not commit and announce import", err)
	}
	roles := []string{"member", "operator", "none", "member"}
	for i, row := range report.Rows {
		a, err := s.GetConsoleAccount(t.Context(), row.AccountID)
		if err != nil || a.ID == batch.Rows[i].ID || !strings.HasPrefix(a.ID, "ca_") || a.Role != roles[i] || a.Disabled != (i >= 2) || a.PasswordHash != batch.Rows[i].PasswordHash || a.CreatedMS != 1000 || a.AuthVersion != 1 || a.EditRevision != 1 || a.CorrespondenceLocale != "" {
			t.Fatal("imported account did not preserve the reviewed contract", i, err)
		}
		if i == 1 {
			if a.LastLoginMS == nil || *a.LastLoginMS != 2000 {
				t.Fatal("historical last login was not preserved")
			}
		} else if a.LastLoginMS != nil {
			t.Fatal("null last login became a timestamp")
		}
	}
	var grants, sessions, actions, ledger int
	if err = s.db.QueryRow(`SELECT (SELECT COUNT(*) FROM ConsoleRepositoryAccess),(SELECT COUNT(*) FROM ConsoleSession),(SELECT COUNT(*) FROM ConsoleAction WHERE actor_kind='owner' AND action='owner_import'),(SELECT COUNT(*) FROM ConsoleImport)`).Scan(&grants, &sessions, &actions, &ledger); err != nil || grants != 0 || sessions != 0 || actions != 4 || ledger != 4 {
		t.Fatal("account import created access/sessions or lost its atomic ledger/audit", err)
	}
	raw, err := json.Marshal(report)
	if err != nil || bytes.Contains(raw, []byte(batch.Rows[0].PasswordHash)) || bytes.Contains(raw, []byte("unrecognized-synthetic-role")) || bytes.Contains(raw, []byte("password_hash")) || bytes.Contains(raw, []byte("fingerprint")) {
		t.Fatal("import report contains untrusted or credential-bearing source data")
	}
	var state string
	if err = s.db.QueryRow("SELECT state FROM Execution WHERE id=?", request.ExecutionID).Scan(&state); err != nil || state != "active" {
		t.Fatal("apply ran unrelated coordination expiry", err)
	}
}

func TestConsoleImportRerunCannotRestoreChangedTarget(t *testing.T) {
	s, _, _ := testStore(t)
	batch, a := importAccount(t, s)
	if _, err := s.db.Exec("UPDATE ConsoleAccount SET password_hash=?,display_name='edited',disabled=1,auth_version=auth_version+1,edit_revision=edit_revision+1 WHERE id=?", importPassword(1), a.ID); err != nil {
		t.Fatal(err)
	}
	before := databaseState(t, s)
	for _, dryRun := range []bool{true, false} {
		report, err := s.ImportConsoleAccounts(t.Context(), batch, dryRun)
		if err != nil || report.Skipped != 1 || report.Created != 0 || report.Rows[0].AccountID != a.ID || databaseState(t, s) != before {
			t.Fatal("exact re-run changed target password/profile/access", err)
		}
	}
	for _, change := range []func(*accountimport.Batch){
		func(b *accountimport.Batch) { b.Rows[0].PasswordHash = importPassword(2) },
		func(b *accountimport.Batch) { b.Rows[0].DisplayName = "different source" },
		func(b *accountimport.Batch) { b.Rows[0].Username = "Different" },
		func(b *accountimport.Batch) { b.Rows[0].Role = "admin" },
		func(b *accountimport.Batch) { b.Rows[0].Disabled = true },
		func(b *accountimport.Batch) { b.Rows[0].CreatedMS++ },
		func(b *accountimport.Batch) { b.Manifest.Renames = map[string]string{"1": "Reviewed"} },
		func(b *accountimport.Batch) { b.Manifest.Locales = map[string]string{"en": "pl"} },
	} {
		changed := importBatch()
		change(&changed)
		_, err := s.ImportConsoleAccounts(t.Context(), changed, false)
		consoleError(t, err, "conflict")
		if databaseState(t, s) != before {
			t.Fatal("changed source re-run committed changes")
		}
	}
	batch.Manifest.SourceID = "different-source"
	_, err := s.ImportConsoleAccounts(t.Context(), batch, false)
	consoleError(t, err, "conflict")
}

func TestConsoleImportRechecksCollisionsAndRefusesWholeBatch(t *testing.T) {
	s, _, _ := testStore(t)
	batch := importBatch()
	if _, err := s.ImportConsoleAccounts(t.Context(), batch, true); err != nil {
		t.Fatal(err)
	}
	createTestConsoleAccount(t, s, "IMPORTED", "member")
	before := databaseState(t, s)
	_, err := s.ImportConsoleAccounts(t.Context(), batch, false)
	consoleError(t, err, "conflict")
	if databaseState(t, s) != before {
		t.Fatal("collision after dry-run changed state")
	}
	batch.Rows[0].Username = "Safe"
	other := batch.Rows[0]
	other.ID, other.Username = "2", "imported"
	batch.Rows = append(batch.Rows, other)
	_, err = s.ImportConsoleAccounts(t.Context(), batch, false)
	consoleError(t, err, "conflict")
	if databaseState(t, s) != before {
		t.Fatal("late collision committed the earlier account or ledger")
	}
	batch.Rows = batch.Rows[:1]
	other.ID, other.Username = "2", "safe"
	batch.Rows = append(batch.Rows, other)
	_, err = s.ImportConsoleAccounts(t.Context(), batch, true)
	if err == nil || databaseState(t, s) != before {
		t.Fatal("in-batch login collision was accepted")
	}
	batch.Manifest.Renames = map[string]string{"2": "Reviewed safe"}
	if report, err := s.ImportConsoleAccounts(t.Context(), batch, false); err != nil || report.Created != 2 {
		t.Fatal("reviewed rename failed", err)
	}
}

func TestConsoleImportRefusesMalformedBatchesWithoutWriting(t *testing.T) {
	s, _, _ := testStore(t)
	before := databaseState(t, s)
	for _, change := range []func(*accountimport.Batch){
		func(b *accountimport.Batch) { b.Rows = append(b.Rows, b.Rows[0]) },
		func(b *accountimport.Batch) { b.Rows[0].PasswordHash = "malformed-synthetic-record" },
		func(b *accountimport.Batch) { b.Rows[0].UILocale = "unsupported" },
		func(b *accountimport.Batch) { b.Rows[0].CreatedMS = -1 },
		func(b *accountimport.Batch) { b.Manifest.TimestampZone = "guessed" },
	} {
		batch := importBatch()
		change(&batch)
		_, err := s.ImportConsoleAccounts(t.Context(), batch, false)
		consoleError(t, err, "bad_request")
		if databaseState(t, s) != before {
			t.Fatal("invalid import changed state")
		}
	}
}

func TestConcurrentConsoleImportsSerializeAndDeduplicate(t *testing.T) {
	for _, sameSource := range []bool{true, false} {
		s, _, _ := testStore(t)
		var wg sync.WaitGroup
		var reports [2]ConsoleImportReport
		var failures [2]error
		for i := range 2 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				batch := importBatch()
				if !sameSource && i == 1 {
					batch.Manifest.SourceID = "other-source"
				}
				reports[i], failures[i] = s.ImportConsoleAccounts(t.Context(), batch, false)
			}()
		}
		wg.Wait()
		if sameSource {
			if failures[0] != nil || failures[1] != nil || reports[0].Created+reports[1].Created != 1 || reports[0].Skipped+reports[1].Skipped != 1 {
				t.Fatal("concurrent exact applies did not create once")
			}
		} else {
			if (failures[0] == nil) == (failures[1] == nil) || reports[0].Created+reports[1].Created != 1 {
				t.Fatal("concurrent colliding sources did not select one winner")
			}
		}
	}
}
