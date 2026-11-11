package store

import (
	"context"

	"github.com/mateusz-klatt/iris/internal/consoleauth"
)

// activationRead never calls expiry bookkeeping or commits a write transaction. Its
// deferred rollback also releases a snapshot after cancellation or an error.
func (s *Store) activationRead(ctx context.Context, f func(*transaction) error) error {
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

// HasEnabledConsoleOperator checks the persisted operator role without expiry
// bookkeeping, writes or notifications. It does not prove possession of an
// operator password; explicit owner activation performs that separate check.
func (s *Store) HasEnabledConsoleOperator(ctx context.Context) (bool, error) {
	var enabled bool
	err := s.activationRead(ctx, func(t *transaction) error {
		return t.row("SELECT EXISTS(SELECT 1 FROM ConsoleAccount WHERE role='operator' AND disabled=0)").Scan(&enabled)
	})
	if err != nil {
		return false, err
	}
	return enabled, nil
}

// PrepareConsoleActivation is a trusted owner setup primitive. The hub must
// authenticate the owner and verify before's password outside the store lock
// before calling it. This transaction rechecks the verified snapshot and enabled
// operator role, clears every console session, and records verification.
// The audit record does not claim that subsequent key creation or activation
// succeeded. No browser session is issued and account credentials never escape
// through its result, including when a read, write or commit fails.
func (s *Store) PrepareConsoleActivation(ctx context.Context, before ConsoleAccount) (ConsoleAccount, error) {
	if consoleauth.ValidateHash(before.PasswordHash) != nil {
		return ConsoleAccount{}, fail("bad_request", "invalid console activation material")
	}
	var account ConsoleAccount
	err := s.transact(ctx, func(t *transaction) error {
		var err error
		account, err = t.consoleSnapshot(before)
		if err != nil {
			return err
		}
		if account.Role != "operator" || !account.Enabled() {
			return fail("unauthorized", "enabled console operator required")
		}
		if _, err = t.exec("DELETE FROM ConsoleSession"); err != nil {
			return err
		}
		if err = t.consoleOwnerAction(account.ID, "activation_verified"); err != nil {
			return err
		}
		t.notify = true
		return nil
	})
	if err != nil {
		return ConsoleAccount{}, err
	}
	account.PasswordHash, account.LoginKey = "", ""
	return account, nil
}
