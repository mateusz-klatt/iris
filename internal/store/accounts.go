package store

import (
	"context"
	"database/sql"

	"github.com/mateusz-klatt/iris/internal/consoleauth"
)

// ConsoleAccount is a storage record, not an authenticated principal. Its password
// hash is available only to trusted hub code for work outside the store lock.
// Password lookup is a trusted hub primitive, never an account/profile projection.
type ConsoleAccount struct {
	ID                   string `json:"id"`
	Username             string `json:"username"`
	LoginKey             string `json:"-"`
	PasswordHash         string `json:"-"`
	Role                 string `json:"role"`
	Disabled             bool   `json:"disabled"`
	DisplayName          string `json:"display_name"`
	UILocale             string `json:"ui_locale"`
	CorrespondenceLocale string `json:"correspondence_locale"`
	CreatedMS            int64  `json:"created_ms"`
	LastLoginMS          *int64 `json:"last_login_ms"`
	AuthVersion          int64  `json:"auth_version"`
	EditRevision         int64  `json:"edit_revision"`
}

type ConsoleAccountInput struct {
	Username, PasswordHash, Role, DisplayName, UILocale, CorrespondenceLocale string
	Disabled                                                                  bool
}

type ConsoleGrant struct {
	RepositoryID string `json:"repository_id"`
	Mode         string `json:"mode"`
}

// ConsoleAccessAccount is the operator editor's snapshot. Its grants and edit
// revision are read in the same transaction; credential fields are cleared.
type ConsoleAccessAccount struct {
	ConsoleAccount
	Grants []ConsoleGrant `json:"grants"`
}

// Enabled denies unrecognized stored roles even if a malformed database row has
// disabled=false. Grants never turn a disabled or unrecognized role into a login.
func (a ConsoleAccount) Enabled() bool {
	return !a.Disabled && (a.Role == "operator" || a.Role == "member")
}

func consoleRole(role string) bool {
	return role == "operator" || role == "member" || role == "none"
}

// Locale values are the exact web/src/i18n.ts catalog, or unset. An importer must
// explicitly resolve other source values; storage never guesses a replacement.
func consoleLocale(locale string) bool {
	return consoleauth.ValidLocale(locale)
}

func consoleProfile(display, ui, correspondence string) error {
	if err := consoleauth.ValidateDisplayName(display); err != nil {
		return fail("bad_request", "invalid display name")
	}
	if !consoleLocale(ui) || !consoleLocale(correspondence) {
		return fail("bad_request", "unsupported console locale")
	}
	return nil
}

func (s *Store) CreateConsoleAccount(ctx context.Context, input ConsoleAccountInput) (ConsoleAccount, error) {
	return s.createConsoleAccount(ctx, input, false)
}

func (s *Store) createConsoleAccount(ctx context.Context, input ConsoleAccountInput, owner bool) (ConsoleAccount, error) {
	key, err := consoleauth.LoginKey(input.Username)
	if err != nil {
		return ConsoleAccount{}, fail("bad_request", "invalid console username")
	}
	if err = consoleauth.ValidateHash(input.PasswordHash); err != nil {
		return ConsoleAccount{}, fail("bad_request", "unsupported console password record")
	}
	if err = consoleProfile(input.DisplayName, input.UILocale, input.CorrespondenceLocale); err != nil {
		return ConsoleAccount{}, err
	}
	if input.Role == "" {
		input.Role = "none"
	}
	if !consoleRole(input.Role) {
		return ConsoleAccount{}, fail("bad_request", "invalid console role")
	}
	if input.Role == "none" {
		input.Disabled = true
	}
	accountID := id("ca_")
	var account ConsoleAccount
	err = s.transact(ctx, func(t *transaction) error {
		result, err := t.exec(`INSERT INTO ConsoleAccount(id,username,login_key,password_hash,role,disabled,display_name,ui_locale,correspondence_locale,created_ms)
 VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(login_key) DO NOTHING`, accountID, input.Username, key, input.PasswordHash, input.Role, input.Disabled, input.DisplayName, input.UILocale, input.CorrespondenceLocale, s.clock.Now().UnixMilli())
		if err != nil {
			return err
		}
		if n, _ := result.RowsAffected(); n == 0 {
			return fail("conflict", "console username is already in use")
		}
		t.notify = true
		account, err = t.consoleAccount("id", accountID)
		if err != nil {
			return err
		}
		if owner {
			return t.consoleOwnerAction(accountID, "owner_provision")
		}
		return nil
	})
	return account, err
}

func (t *transaction) consoleAccount(column, value string) (ConsoleAccount, error) {
	var a ConsoleAccount
	var last sql.NullInt64
	err := t.row(`SELECT id,username,login_key,password_hash,role,disabled,display_name,ui_locale,correspondence_locale,created_ms,last_login_ms,auth_version,edit_revision FROM ConsoleAccount WHERE `+column+`=?`, value).
		Scan(&a.ID, &a.Username, &a.LoginKey, &a.PasswordHash, &a.Role, &a.Disabled, &a.DisplayName, &a.UILocale, &a.CorrespondenceLocale, &a.CreatedMS, &last, &a.AuthVersion, &a.EditRevision)
	if last.Valid {
		a.LastLoginMS = &last.Int64
	}
	return a, missing(err, "unknown console account")
}

func (s *Store) GetConsoleAccount(ctx context.Context, accountID string) (ConsoleAccount, error) {
	var account ConsoleAccount
	err := s.transact(ctx, func(t *transaction) error {
		var err error
		account, err = t.consoleAccount("id", accountID)
		return err
	})
	return account, err
}

func (s *Store) LookupConsoleAccount(ctx context.Context, username string) (ConsoleAccount, error) {
	key, err := consoleauth.LoginKey(username)
	if err != nil {
		return ConsoleAccount{}, fail("bad_request", "invalid console username")
	}
	var account ConsoleAccount
	err = s.transact(ctx, func(t *transaction) error {
		var err error
		account, err = t.consoleAccount("login_key", key)
		return err
	})
	return account, err
}

// UpdateConsoleProfile is a trusted storage primitive. Account requests use
// EditConsoleProfile, which selects the actor from the validated session.
func (s *Store) UpdateConsoleProfile(ctx context.Context, accountID string, expectedRevision int64, display, ui, correspondence string) (ConsoleAccount, error) {
	if err := consoleProfile(display, ui, correspondence); err != nil {
		return ConsoleAccount{}, err
	}
	var account ConsoleAccount
	err := s.transact(ctx, func(t *transaction) error {
		var err error
		account, err = t.updateConsoleProfile(accountID, expectedRevision, display, ui, correspondence)
		return err
	})
	return account, err
}

func (t *transaction) updateConsoleProfile(accountID string, expectedRevision int64, display, ui, correspondence string) (ConsoleAccount, error) {
	before, err := t.consoleAccount("id", accountID)
	if err != nil {
		return ConsoleAccount{}, err
	}
	if before.EditRevision != expectedRevision {
		return ConsoleAccount{}, fail("conflict", "console account changed; refresh before editing")
	}
	if _, err = t.exec(`UPDATE ConsoleAccount SET display_name=?,ui_locale=?,correspondence_locale=?,edit_revision=edit_revision+1 WHERE id=?`, display, ui, correspondence, accountID); err != nil {
		return ConsoleAccount{}, err
	}
	t.notify = true
	return t.consoleAccount("id", accountID)
}

// validateConsoleAccess accepts exactly Iris roles and unique ro/rw grants.
func validateConsoleAccess(role string, grants []ConsoleGrant) error {
	if !consoleRole(role) || (role != "member" && len(grants) != 0) {
		return fail("bad_request", "invalid console role or grants")
	}
	seen := map[string]bool{}
	for _, grant := range grants {
		if grant.RepositoryID == "" || (grant.Mode != "ro" && grant.Mode != "rw") || seen[grant.RepositoryID] {
			return fail("bad_request", "invalid or duplicate console grant")
		}
		seen[grant.RepositoryID] = true
	}
	return nil
}

// ReplaceConsoleAccess is a trusted storage primitive, without console acting-
// operator checks. Account requests use ChangeConsoleAccess for those checks,
// last-operator protection and transactional action attribution.
func (s *Store) ReplaceConsoleAccess(ctx context.Context, accountID string, expectedRevision int64, role string, disabled bool, grants []ConsoleGrant) (ConsoleAccount, error) {
	if err := validateConsoleAccess(role, grants); err != nil {
		return ConsoleAccount{}, err
	}
	var account ConsoleAccount
	err := s.transact(ctx, func(t *transaction) error {
		var err error
		account, err = t.replaceConsoleAccess(accountID, expectedRevision, role, disabled, grants)
		return err
	})
	return account, err
}

func (t *transaction) replaceConsoleAccess(accountID string, expectedRevision int64, role string, disabled bool, grants []ConsoleGrant) (ConsoleAccount, error) {
	if role == "none" {
		disabled = true
	}
	before, err := t.consoleAccount("id", accountID)
	if err != nil {
		return ConsoleAccount{}, err
	}
	if before.EditRevision != expectedRevision {
		return ConsoleAccount{}, fail("conflict", "console account changed; refresh before editing")
	}
	if _, err = t.exec(`UPDATE ConsoleAccount SET role=?,disabled=?,auth_version=auth_version+1,edit_revision=edit_revision+1 WHERE id=?`, role, disabled, accountID); err != nil {
		return ConsoleAccount{}, err
	}
	if _, err = t.exec("DELETE FROM ConsoleRepositoryAccess WHERE account_id=?", accountID); err != nil {
		return ConsoleAccount{}, err
	}
	for _, grant := range grants {
		if _, err = t.exec("INSERT INTO ConsoleRepositoryAccess(account_id,repository_id,mode) VALUES(?,?,?)", accountID, grant.RepositoryID, grant.Mode); err != nil {
			return ConsoleAccount{}, err
		}
	}
	if _, err = t.exec("DELETE FROM ConsoleSession WHERE account_id=?", accountID); err != nil {
		return ConsoleAccount{}, err
	}
	t.notify = true
	return t.consoleAccount("id", accountID)
}

// ConsoleRepositoryMode reads only strict grant modes. This prepares the storage
// contract; console operations authorize through CallConsole instead.
func (s *Store) ConsoleRepositoryMode(ctx context.Context, accountID, repositoryID string) (string, error) {
	mode := ""
	err := s.transact(ctx, func(t *transaction) error {
		return t.row(`SELECT COALESCE((SELECT g.mode FROM ConsoleRepositoryAccess g JOIN ConsoleAccount a ON a.id=g.account_id
 WHERE a.id=? AND a.disabled=0 AND a.role='member' AND g.repository_id=? AND g.mode IN ('ro','rw')),'')`, accountID, repositoryID).Scan(&mode)
	})
	return mode, err
}
