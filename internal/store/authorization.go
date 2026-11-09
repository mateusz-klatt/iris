package store

import (
	"context"
)

// Principal is supplied by trusted transport code, never decoded from a request.
// SessionDigest is the stored opaque-session digest, not a cookie or password.
// HTTP account routes remain gated until the account cutover.
type Principal struct {
	Kind          PrincipalKind
	SessionDigest []byte
}

type PrincipalKind string

const (
	ConsolePrincipal PrincipalKind = "console"
	OwnerPrincipal   PrincipalKind = "owner"
	HostPrincipal    PrincipalKind = "host"
)

type authorizationKey struct{}

// CallAuthorized stages the operation matrix without changing legacy routes.
// The account, session and repository predicates run in Call's object transaction.
func (s *Store) CallAuthorized(ctx context.Context, p Principal, operation string, r Request) (map[string]any, error) {
	return s.Call(context.WithValue(ctx, authorizationKey{}, p), operation, r)
}

// CallConsole cannot use a bearer to assert a named person, including an owner.
func (s *Store) CallConsole(ctx context.Context, p Principal, operation string, r Request) (map[string]any, error) {
	if p.Kind != ConsolePrincipal {
		return nil, fail("forbidden", "a console account session is required")
	}
	return s.CallAuthorized(ctx, p, operation, r)
}

func (t *transaction) authorize(p Principal, operation string, r Request) error {
	if p.Kind == OwnerPrincipal || p.Kind == HostPrincipal {
		if len(p.SessionDigest) != 0 {
			return fail("forbidden", "mixed machine and console credentials")
		}
		if operation == "ui_messages" || operation == "ui_profile" || operation == "ui_access" || operation == "ui_accounts" {
			return fail("forbidden", "a console account session is required")
		}
		if p.Kind == HostPrincipal && (operation == "forget" || operation == "merge" || operation == "contact") {
			return fail("forbidden", "only the owner may manage repositories")
		}
		return nil
	}
	if p.Kind != ConsolePrincipal || len(p.SessionDigest) != 32 {
		return fail("unauthorized", "console session required")
	}
	session, err := t.consoleSession(p.SessionDigest)
	if err != nil {
		return err
	}
	a := session.Account
	t.console = &a
	switch operation {
	case "ui_overview", "ui_deliveries", "ui_threads", "ui_thread", "ui_receipts", "ui_events", "ui_messages", "ui_profile":
	case "ui_read", "ui_revoke", "ui_doctor_get", "ui_accounts", "ui_access":
		if a.Role != "operator" {
			return fail("forbidden", "only console operators may do this")
		}
	default:
		return fail("forbidden", "console sessions authorize only console operations")
	}
	// A read-only member has no send operation at all. Mixed-scope accounts
	// still receive not_found for individual unwritable objects below.
	if operation == "ui_messages" && a.Role == "member" {
		var writable bool
		if err := t.row("SELECT EXISTS(SELECT 1 FROM ConsoleRepositoryAccess WHERE account_id=? AND mode='rw')", a.ID).Scan(&writable); err != nil {
			return err
		}
		if !writable {
			return fail("forbidden", "a writable repository grant is required to send")
		}
	}
	// A send checks current accepted objects before consulting the supplied origin:
	// an exact replay may still name a repository retired by a reviewed merge.
	if r.RepositoryID != "" && operation != "ui_messages" {
		return t.consoleReadRepository(r.RepositoryID)
	}
	return nil
}

func (t *transaction) consoleReadRepository(repo string) error {
	var known int
	err := t.row(`SELECT COUNT(*) FROM Repository r WHERE r.id=? AND (?='operator' OR EXISTS
 (SELECT 1 FROM ConsoleRepositoryAccess g WHERE g.account_id=? AND g.repository_id=r.id AND g.mode IN ('ro','rw')))`, repo, t.console.Role, t.console.ID).Scan(&known)
	if err != nil {
		return err
	}
	if known == 0 {
		return fail("not_found", "unknown repository")
	}
	return nil
}

func (t *transaction) consoleAction(actor, target, operation string) error {
	_, err := t.exec(`INSERT INTO ConsoleAction(actor_account_id,target_account_id,action,outcome,created_ms) VALUES(?,NULLIF(?,''),?,'accepted',?)`, actor, target, operation, t.s.clock.Now().UnixMilli())
	return err
}

// EditConsoleProfile always selects the actor from the validated session. Profile
// input cannot choose another account, a global role, or repository permissions.
func (s *Store) EditConsoleProfile(ctx context.Context, p Principal, revision int64, display, ui, correspondence string) (ConsoleAccount, error) {
	var account ConsoleAccount
	err := s.transact(ctx, func(t *transaction) error {
		if err := t.authorize(p, "ui_profile", Request{}); err != nil {
			return err
		}
		if err := consoleProfile(display, ui, correspondence); err != nil {
			return err
		}
		var err error
		account, err = t.updateConsoleProfile(t.console.ID, revision, display, ui, correspondence)
		if err != nil {
			return err
		}
		return t.consoleAction(t.console.ID, t.console.ID, "profile")
	})
	return account, err
}

// ChangeConsoleAccess validates the acting operator and edit revision together
// with grant replacement and revocation. Owner provisioning has a separate path.
func (s *Store) ChangeConsoleAccess(ctx context.Context, p Principal, target string, revision int64, role string, disabled bool, grants []ConsoleGrant) (ConsoleAccessAccount, error) {
	var account ConsoleAccessAccount
	err := s.transact(ctx, func(t *transaction) error {
		if err := t.authorize(p, "ui_access", Request{}); err != nil {
			return err
		}
		if err := validateConsoleAccess(role, grants); err != nil {
			return err
		}
		before, err := t.consoleAccount("id", target)
		if err != nil {
			return err
		}
		if before.Role == "operator" && !before.Disabled && (role != "operator" || disabled) {
			var operators int
			if err = t.row("SELECT COUNT(*) FROM ConsoleAccount WHERE role='operator' AND disabled=0").Scan(&operators); err != nil {
				return err
			}
			if operators <= 1 {
				return fail("conflict", "the last enabled operator must remain enabled")
			}
		}
		updated, err := t.replaceConsoleAccess(target, revision, role, disabled, grants)
		if err != nil {
			return err
		}
		account, err = t.consoleAccessProjection(updated)
		if err != nil {
			return err
		}
		return t.consoleAction(t.console.ID, target, "access")
	})
	return account, err
}

func (s *Store) ListConsoleAccounts(ctx context.Context, p Principal) ([]ConsoleAccessAccount, error) {
	accounts := []ConsoleAccessAccount{}
	err := s.transact(ctx, func(t *transaction) error {
		if err := t.authorize(p, "ui_accounts", Request{}); err != nil {
			return err
		}
		rows, err := t.rows("SELECT id FROM ConsoleAccount ORDER BY login_key")
		if err != nil {
			return err
		}
		ids := []string{}
		for rows.Next() {
			var id string
			rows.Scan(&id)
			ids = append(ids, id)
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return err
		}
		for _, id := range ids {
			a, err := t.consoleAccount("id", id)
			if err != nil {
				return err
			}
			projected, err := t.consoleAccessProjection(a)
			if err != nil {
				return err
			}
			accounts = append(accounts, projected)
		}
		return nil
	})
	return accounts, err
}

func (t *transaction) consoleAccessProjection(a ConsoleAccount) (ConsoleAccessAccount, error) {
	// Trusted password lookup has a separate method. No console projection may
	// carry the password record or login key, even inside a Go result.
	a.PasswordHash, a.LoginKey = "", ""
	account := ConsoleAccessAccount{ConsoleAccount: a, Grants: []ConsoleGrant{}}
	rows, err := t.rows("SELECT repository_id,mode FROM ConsoleRepositoryAccess WHERE account_id=? ORDER BY repository_id", a.ID)
	if err != nil {
		return ConsoleAccessAccount{}, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var g ConsoleGrant
		rows.Scan(&g.RepositoryID, &g.Mode)
		account.Grants = append(account.Grants, g)
	}
	return account, rows.Err()
}
