package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/mateusz-klatt/iris/internal/accountimport"
)

// Assignment transfer is deliberately separate from account-table import. A
// reviewed source viewer may receive ro; unsupported source writes never become rw.
type ConsoleAssignmentTransfer struct {
	Version  int                    `json:"version"`
	SourceID string                 `json:"source_id"`
	Rows     []ConsoleAssignmentRow `json:"rows"`
	Projects map[string]string      `json:"projects"`
}

type ConsoleAssignmentRow struct {
	ID           string `json:"id"`
	AccountRowID string `json:"account_row_id"`
	ProjectID    string `json:"project_id"`
	Role         string `json:"role"`
}

func validateConsoleTransfer(input ConsoleAssignmentTransfer) error {
	if input.Version != 1 || !accountimport.ValidSourceID(input.SourceID) || len(input.Rows) == 0 || len(input.Rows) > accountimport.MaxRows || len(input.Projects) == 0 || len(input.Projects) > accountimport.MaxRows {
		return fail("bad_request", "invalid console assignment manifest")
	}
	ids, destinations, pairs, used := map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}
	for project, repository := range input.Projects {
		if !accountimport.ValidSourceID(project) || !accountimport.ValidSourceID(repository) || destinations[repository] {
			return fail("bad_request", "invalid or ambiguous project mapping")
		}
		destinations[repository] = true
	}
	for _, row := range input.Rows {
		if !accountimport.ValidRowID(row.ID) || !accountimport.ValidRowID(row.AccountRowID) || !accountimport.ValidSourceID(row.ProjectID) {
			return fail("bad_request", "invalid console assignment row identifier")
		}
		if ids[row.ID] || pairs[row.AccountRowID+":"+row.ProjectID] {
			return importRefusal(row.ID, "duplicate_assignment")
		}
		ids[row.ID], pairs[row.AccountRowID+":"+row.ProjectID] = true, true
		if row.Role != "viewer" {
			return importRefusal(row.ID, "unsupported_assignment_role")
		}
		if input.Projects[row.ProjectID] == "" {
			return importRefusal(row.ID, "unmapped_project")
		}
		used[row.ProjectID] = true
	}
	if len(used) != len(input.Projects) {
		return fail("bad_request", "unused project mapping")
	}
	return nil
}

// TransferConsoleAssignments accepts only owner-reviewed explicit mappings to
// existing immutable Repository IDs. It cannot overwrite a grant or infer a role.
func (s *Store) TransferConsoleAssignments(ctx context.Context, input ConsoleAssignmentTransfer, dryRun bool) (ConsoleImportReport, error) {
	report := ConsoleImportReport{DryRun: dryRun, Rows: []ConsoleImportRow{}}
	validate := func(t *transaction) error {
		if err := validateConsoleTransfer(input); err != nil {
			return err
		}
		for _, row := range input.Rows {
			result, err := t.importConsoleAssignment(input.SourceID, row, input.Projects[row.ProjectID], dryRun)
			if err != nil {
				return err
			}
			report.Rows = append(report.Rows, result)
			if result.Action == "skip" {
				report.Skipped++
			} else {
				report.Created++
			}
		}
		return nil
	}
	var err error
	if dryRun {
		err = s.importRead(ctx, validate)
	} else {
		err = s.transact(ctx, validate)
	}
	if err != nil {
		return ConsoleImportReport{}, err
	}
	return report, nil
}

func (t *transaction) importConsoleAssignment(sourceID string, row ConsoleAssignmentRow, repository string, dryRun bool) (ConsoleImportRow, error) {
	result := ConsoleImportRow{SourceRowID: row.ID, Action: "create", Role: "ro"}
	var role string
	err := t.row(`SELECT a.id,a.role,a.disabled FROM ConsoleImport i JOIN ConsoleAccount a ON a.id=i.account_id WHERE i.source_id=? AND i.source_row_id=?`, sourceID, row.AccountRowID).Scan(&result.AccountID, &role, &result.Disabled)
	if errors.Is(err, sql.ErrNoRows) {
		return ConsoleImportRow{}, importRefusal(row.ID, "unimported_account")
	}
	if err != nil {
		return ConsoleImportRow{}, err
	}
	if role != "member" {
		return ConsoleImportRow{}, importRefusal(row.ID, "assignment_requires_member")
	}
	var count int
	if err = t.row("SELECT COUNT(*) FROM Repository WHERE id=?", repository).Scan(&count); err != nil {
		return ConsoleImportRow{}, err
	}
	if count != 1 {
		return ConsoleImportRow{}, importRefusal(row.ID, "unknown_repository")
	}
	canonical, _ := json.Marshal(struct {
		Version    int
		SourceID   string
		Row        ConsoleAssignmentRow
		Repository string
	}{1, sourceID, row, repository})
	fingerprint := sha256.Sum256(canonical)
	var recorded []byte
	var version int
	err = t.row(`SELECT fingerprint_version,source_fingerprint FROM ConsoleAssignmentImport WHERE source_id=? AND source_row_id=?`, sourceID, row.ID).Scan(&version, &recorded)
	if err == nil {
		if version != 1 || !bytes.Equal(recorded, fingerprint[:]) {
			return ConsoleImportRow{}, importRefusal(row.ID, "assignment_changed")
		}
		result.Action = "skip"
		return result, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return ConsoleImportRow{}, err
	}
	if err = t.row("SELECT COUNT(*) FROM ConsoleRepositoryAccess WHERE account_id=? AND repository_id=?", result.AccountID, repository).Scan(&count); err != nil {
		return ConsoleImportRow{}, err
	}
	if count != 0 {
		return ConsoleImportRow{}, importRefusal(row.ID, "grant_already_exists")
	}
	if dryRun {
		return result, nil
	}
	if _, err = t.exec("INSERT INTO ConsoleRepositoryAccess(account_id,repository_id,mode) VALUES(?,?,'ro')", result.AccountID, repository); err != nil {
		return ConsoleImportRow{}, err
	}
	if _, err = t.exec(`INSERT INTO ConsoleAssignmentImport(source_id,source_row_id,account_id,repository_id,fingerprint_version,source_fingerprint,created_ms) VALUES(?,?,?,?,1,?,?)`, sourceID, row.ID, result.AccountID, repository, fingerprint[:], t.s.clock.Now().UnixMilli()); err != nil {
		return ConsoleImportRow{}, err
	}
	if _, err = t.exec("UPDATE ConsoleAccount SET auth_version=auth_version+1,edit_revision=edit_revision+1 WHERE id=?", result.AccountID); err != nil {
		return ConsoleImportRow{}, err
	}
	if _, err = t.exec("DELETE FROM ConsoleSession WHERE account_id=?", result.AccountID); err != nil {
		return ConsoleImportRow{}, err
	}
	if err = t.consoleOwnerAction(result.AccountID, "owner_assignment_import"); err != nil {
		return ConsoleImportRow{}, err
	}
	t.notify = true
	return result, nil
}
