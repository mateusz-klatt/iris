package store

import (
	"testing"

	"github.com/mateusz-klatt/iris/internal/accountimport"
)

func TestConsoleAssignmentTransferIsSeparateReviewedReadOnlyAndRevokesSessions(t *testing.T) {
	s, _, _ := testStore(t)
	_, registered := session(t, s, 1)
	repo := registered["repository_id"].(string)
	_, a := importAccount(t, s)
	digest := make([]byte, 32)
	now := s.clock.Now().UnixMilli()
	if _, err := s.db.Exec("INSERT INTO ConsoleSession(digest,account_id,auth_version,created_ms,expires_ms) VALUES(?,?,?,?,?)", digest, a.ID, a.AuthVersion, now, now+43200000); err != nil {
		t.Fatal(err)
	}
	session := Principal{Kind: ConsolePrincipal, SessionDigest: digest}
	if _, err := s.ConsoleSessionInfo(t.Context(), digest); err != nil {
		t.Fatal("synthetic active session unavailable before transfer", err)
	}
	transfer := importTransfer(repo)
	before, changed := databaseState(t, s), s.Changes()
	report, err := s.TransferConsoleAssignments(t.Context(), transfer, true)
	if err != nil || !report.DryRun || report.Created != 1 || report.Rows[0].AccountID != a.ID || report.Rows[0].Role != "ro" || databaseState(t, s) != before || s.Changes() != changed {
		t.Fatal("assignment dry-run mutated or misreported the explicit mapping", err)
	}
	report, err = s.TransferConsoleAssignments(t.Context(), transfer, false)
	if err != nil || report.Created != 1 || report.Skipped != 0 || s.Changes() == changed {
		t.Fatal("reviewed viewer transfer failed", err)
	}
	mode, err := s.ConsoleRepositoryMode(t.Context(), a.ID, repo)
	if err != nil || mode != "ro" {
		t.Fatal("viewer was not mapped to exactly ro", err)
	}
	_, err = s.ConsoleSessionInfo(t.Context(), session.SessionDigest)
	consoleError(t, err, "unauthorized")
	after, err := s.GetConsoleAccount(t.Context(), a.ID)
	if err != nil || after.AuthVersion != a.AuthVersion+1 || after.EditRevision != a.EditRevision+1 {
		t.Fatal("assignment transfer failed to revoke prior authority", err)
	}
	var ledger, actions int
	if err = s.db.QueryRow(`SELECT (SELECT COUNT(*) FROM ConsoleAssignmentImport),(SELECT COUNT(*) FROM ConsoleAction WHERE actor_kind='owner' AND action='owner_assignment_import')`).Scan(&ledger, &actions); err != nil || ledger != 1 || actions != 1 {
		t.Fatal("assignment transfer lost ledger or owner action", err)
	}
	// Later Iris review can grant rw. A source replay cannot restore its old ro.
	if _, err = s.ReplaceConsoleAccess(t.Context(), a.ID, after.EditRevision, "member", false, []ConsoleGrant{{repo, "rw"}}); err != nil {
		t.Fatal(err)
	}
	before = databaseState(t, s)
	for _, dryRun := range []bool{true, false} {
		report, err = s.TransferConsoleAssignments(t.Context(), transfer, dryRun)
		if err != nil || report.Skipped != 1 || report.Created != 0 || databaseState(t, s) != before {
			t.Fatal("assignment rerun changed target grants", err)
		}
	}
}

func TestConsoleAssignmentTransferRefusesUnsupportedOrAmbiguousManifest(t *testing.T) {
	s, _, _ := testStore(t)
	_, registered := session(t, s, 1)
	repo := registered["repository_id"].(string)
	importAccount(t, s)
	before := databaseState(t, s)
	cases := []struct {
		name   string
		change func(*ConsoleAssignmentTransfer)
	}{
		{"version", func(v *ConsoleAssignmentTransfer) { v.Version = 2 }},
		{"source", func(v *ConsoleAssignmentTransfer) { v.SourceID = "invalid source" }},
		{"empty", func(v *ConsoleAssignmentTransfer) { v.Rows = nil }},
		{"unmapped", func(v *ConsoleAssignmentTransfer) { v.Rows[0].ProjectID = "8" }},
		{"unknown-role", func(v *ConsoleAssignmentTransfer) { v.Rows[0].Role = "unknown" }},
		{"source-project-operator-despite-rw", func(v *ConsoleAssignmentTransfer) { v.Rows[0].Role = "operator" }},
		{"source-rw", func(v *ConsoleAssignmentTransfer) { v.Rows[0].Role = "rw" }},
		{"duplicate-id", func(v *ConsoleAssignmentTransfer) { v.Rows = append(v.Rows, v.Rows[0]) }},
		{"duplicate-pair", func(v *ConsoleAssignmentTransfer) { r := v.Rows[0]; r.ID = "2"; v.Rows = append(v.Rows, r) }},
		{"bad-row-id", func(v *ConsoleAssignmentTransfer) { v.Rows[0].ID = "01" }},
		{"bad-account-id", func(v *ConsoleAssignmentTransfer) { v.Rows[0].AccountRowID = "0" }},
		{"bad-project-id", func(v *ConsoleAssignmentTransfer) { v.Rows[0].ProjectID = "bad project" }},
		{"bad-project-key", func(v *ConsoleAssignmentTransfer) { v.Projects = map[string]string{"bad project": repo} }},
		{"bad-repository-value", func(v *ConsoleAssignmentTransfer) { v.Projects["7"] = "invalid repository" }},
		{"ambiguous-projects", func(v *ConsoleAssignmentTransfer) { v.Projects["8"] = repo }},
		{"unused-project", func(v *ConsoleAssignmentTransfer) { v.Projects["8"] = "rp_missing" }},
		{"unknown-repository", func(v *ConsoleAssignmentTransfer) { v.Projects["7"] = "rp_missing" }},
		{"unimported-account", func(v *ConsoleAssignmentTransfer) { v.Rows[0].AccountRowID = "2" }},
		{"too-many", func(v *ConsoleAssignmentTransfer) { v.Rows = make([]ConsoleAssignmentRow, accountimport.MaxRows+1) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			transfer := importTransfer(repo)
			tc.change(&transfer)
			if _, err := s.TransferConsoleAssignments(t.Context(), transfer, false); err == nil || databaseState(t, s) != before {
				t.Fatal("unsupported assignment transfer changed state")
			}
		})
	}
}

func TestConsoleAssignmentTransferCannotRestrictGlobalOperatorOrOverwriteGrant(t *testing.T) {
	for _, sourceRole := range []string{"admin", "unrecognized"} {
		s, _, _ := testStore(t)
		_, registered := session(t, s, 1)
		batch := importBatch()
		batch.Rows[0].Role = sourceRole
		if _, err := s.ImportConsoleAccounts(t.Context(), batch, false); err != nil {
			t.Fatal(err)
		}
		before := databaseState(t, s)
		_, err := s.TransferConsoleAssignments(t.Context(), importTransfer(registered["repository_id"].(string)), false)
		consoleError(t, err, "conflict")
		if databaseState(t, s) != before {
			t.Fatal("assignment constrained a global operator or quarantined account")
		}
	}
	s, _, _ := testStore(t)
	_, registered := session(t, s, 1)
	repo := registered["repository_id"].(string)
	_, a := importAccount(t, s)
	if _, err := s.ReplaceConsoleAccess(t.Context(), a.ID, a.EditRevision, "member", false, []ConsoleGrant{{repo, "rw"}}); err != nil {
		t.Fatal(err)
	}
	before := databaseState(t, s)
	_, err := s.TransferConsoleAssignments(t.Context(), importTransfer(repo), false)
	consoleError(t, err, "conflict")
	if databaseState(t, s) != before {
		t.Fatal("source viewer overwrote Iris rw")
	}
}

func TestConsoleAssignmentExactReplayRechecksCurrentRole(t *testing.T) {
	s, _, _ := testStore(t)
	_, registered := session(t, s, 1)
	_, a := importAccount(t, s)
	transfer := importTransfer(registered["repository_id"].(string))
	if _, err := s.TransferConsoleAssignments(t.Context(), transfer, false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReplaceConsoleAccess(t.Context(), a.ID, a.EditRevision+1, "operator", false, nil); err != nil {
		t.Fatal(err)
	}
	before := databaseState(t, s)
	_, err := s.TransferConsoleAssignments(t.Context(), transfer, false)
	consoleError(t, err, "conflict")
	if databaseState(t, s) != before {
		t.Fatal("replayed source assignment changed a current global operator")
	}
}

func TestConsoleAssignmentTransferChangedRerunAndBatchFailureAreAtomic(t *testing.T) {
	s, _, _ := testStore(t)
	_, registered := session(t, s, 1)
	repo := registered["repository_id"].(string)
	other := call(t, s, "sessions", Request{Client: "codex", Host: "other-import", SessionID: "other-import", Cwd: "/other-import", Worktree: "/other-import", RemoteURL: "https://example.invalid/other-import.git"})["repository_id"].(string)
	_, a := importAccount(t, s)
	transfer := importTransfer(repo)
	transfer.Rows = append(transfer.Rows, ConsoleAssignmentRow{ID: "2", AccountRowID: "2", ProjectID: "8", Role: "viewer"})
	transfer.Projects["8"] = other
	before := databaseState(t, s)
	_, err := s.TransferConsoleAssignments(t.Context(), transfer, false)
	consoleError(t, err, "conflict")
	if databaseState(t, s) != before {
		t.Fatal("late assignment refusal committed the earlier grant")
	}
	transfer = importTransfer(repo)
	if _, err = s.TransferConsoleAssignments(t.Context(), transfer, false); err != nil {
		t.Fatal(err)
	}
	before = databaseState(t, s)
	transfer.Projects["7"] = other
	_, err = s.TransferConsoleAssignments(t.Context(), transfer, false)
	consoleError(t, err, "conflict")
	if databaseState(t, s) != before {
		t.Fatal("changed mapping modified imported grant")
	}
	// Removing the target repository leaves the durable transfer ledger, which
	// must fail closed instead of granting a different repository on replay.
	if _, err = s.db.Exec("DELETE FROM ConsoleRepositoryAccess WHERE account_id=?", a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec("UPDATE ConsoleAssignmentImport SET repository_id='retired' WHERE account_id=?", a.ID); err != nil {
		t.Fatal(err)
	}
	before = databaseState(t, s)
	transfer.Projects["7"] = "retired"
	_, err = s.TransferConsoleAssignments(t.Context(), transfer, false)
	consoleError(t, err, "conflict")
	if databaseState(t, s) != before {
		t.Fatal("missing repository resurrected transferred grant")
	}
}
