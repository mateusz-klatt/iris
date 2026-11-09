package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"

	"github.com/mateusz-klatt/iris/internal/accountimport"
)

// ConsoleImportReport deliberately contains neither source credentials nor row
// payloads. A dry-run is a snapshot; apply repeats validation under the write lock.
type ConsoleImportReport struct {
	DryRun      bool               `json:"dry_run"`
	Created     int                `json:"created"`
	Skipped     int                `json:"skipped"`
	Quarantined int                `json:"quarantined"`
	Rows        []ConsoleImportRow `json:"rows"`
}

type ConsoleImportRow struct {
	SourceRowID    string `json:"source_row_id"`
	AccountID      string `json:"account_id,omitempty"`
	Action         string `json:"action"`
	Role           string `json:"role"`
	Disabled       bool   `json:"disabled"`
	Renamed        bool   `json:"renamed"`
	LocaleResolved bool   `json:"locale_resolved"`
}

func importRefusal(rowID, reason string) error {
	return &Error{Code: "conflict", Message: "console import requires review", Details: map[string]any{"source_row_id": rowID, "reason": reason}}
}

// importRead never calls expiry bookkeeping or commits a write transaction. Its
// deferred rollback also releases a snapshot after cancellation or an error.
func (s *Store) importRead(ctx context.Context, f func(*transaction) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	defer func() { _, _ = c.ExecContext(context.Background(), "ROLLBACK") }()
	if _, err = c.ExecContext(ctx, "BEGIN"); err != nil {
		return err
	}
	s.transactions.Add(1)
	return f(&transaction{conn: c, ctx: ctx, s: s})
}

// ImportConsoleAccounts is a trusted owner primitive. The CLI reads only the
// source; the authenticated hub invokes this method and remains the sole writer.
func (s *Store) ImportConsoleAccounts(ctx context.Context, batch accountimport.Batch, dryRun bool) (ConsoleImportReport, error) {
	report := ConsoleImportReport{DryRun: dryRun, Rows: []ConsoleImportRow{}}
	validate := func(t *transaction) error {
		prepared, err := accountimport.Validate(batch)
		if err != nil {
			return fail("bad_request", err.Error())
		}
		for _, row := range prepared {
			result, err := t.importConsoleAccount(batch.Manifest.SourceID, row, dryRun)
			if err != nil {
				return err
			}
			report.Rows = append(report.Rows, result)
			if result.Action == "skip" {
				report.Skipped++
			} else {
				report.Created++
			}
			if result.Role == "none" {
				report.Quarantined++
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

func (t *transaction) importConsoleAccount(sourceID string, row accountimport.PreparedRow, dryRun bool) (ConsoleImportRow, error) {
	a := row.Account
	result := ConsoleImportRow{SourceRowID: row.Original.ID, Action: "create", Role: a.Role, Disabled: a.Disabled,
		Renamed:        a.Username != row.Original.Username,
		LocaleResolved: a.UILocale != row.Original.UILocale || a.CorrespondenceLocale != row.Original.CorrespondenceLocale}
	var fingerprint []byte
	var version int
	err := t.row(`SELECT account_id,fingerprint_version,source_fingerprint FROM ConsoleImport WHERE source_id=? AND source_row_id=?`, sourceID, row.Original.ID).Scan(&result.AccountID, &version, &fingerprint)
	if err == nil {
		if version != 1 || !bytes.Equal(fingerprint, row.Fingerprint[:]) {
			return ConsoleImportRow{}, importRefusal(row.Original.ID, "source_changed")
		}
		result.Action = "skip"
		return result, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return ConsoleImportRow{}, err
	}
	var collisions int
	if err = t.row("SELECT COUNT(*) FROM ConsoleAccount WHERE login_key=?", row.LoginKey).Scan(&collisions); err != nil {
		return ConsoleImportRow{}, err
	}
	if collisions != 0 {
		return ConsoleImportRow{}, importRefusal(row.Original.ID, "login_collision")
	}
	if dryRun {
		return result, nil
	}
	result.AccountID = id("ca_")
	if _, err = t.exec(`INSERT INTO ConsoleAccount(id,username,login_key,password_hash,role,disabled,display_name,ui_locale,correspondence_locale,created_ms,last_login_ms)
 VALUES(?,?,?,?,?,?,?,?,?,?,?)`, result.AccountID, a.Username, row.LoginKey, a.PasswordHash, a.Role, a.Disabled, a.DisplayName, a.UILocale, a.CorrespondenceLocale, a.CreatedMS, a.LastLoginMS); err != nil {
		return ConsoleImportRow{}, err
	}
	if _, err = t.exec(`INSERT INTO ConsoleImport(source_id,source_row_id,account_id,fingerprint_version,source_fingerprint,created_ms) VALUES(?,?,?,1,?,?)`, sourceID, row.Original.ID, result.AccountID, row.Fingerprint[:], t.s.clock.Now().UnixMilli()); err != nil {
		return ConsoleImportRow{}, err
	}
	if err = t.consoleOwnerAction(result.AccountID, "owner_import"); err != nil {
		return ConsoleImportRow{}, err
	}
	t.notify = true
	return result, nil
}
