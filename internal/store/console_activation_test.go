package store

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"
)

func TestHasEnabledConsoleOperatorReadsOnlyCurrentEnabledRole(t *testing.T) {
	s, clock, _ := testStore(t)
	active, _ := session(t, s, 1)
	call(t, s, "touch", touchRequest(active, "held", "file.go"))
	clock.advance(2 * time.Minute)
	assertEnabled := func(want bool) {
		t.Helper()
		before, changes := databaseState(t, s), s.Changes()
		got, err := s.HasEnabledConsoleOperator(t.Context())
		if err != nil || got != want {
			t.Fatalf("enabled operator = %v, want %v: %v", got, want, err)
		}
		if databaseState(t, s) != before || s.Changes() != changes {
			t.Fatal("operator inspection changed coordination or account state")
		}
	}
	assertEnabled(false)
	newSessionAccount(t, s, "member", "member")
	newSessionAccount(t, s, "none", "none")
	if _, err := s.CreateConsoleAccount(t.Context(), ConsoleAccountInput{Username: "disabled", PasswordHash: consoleArgonTestHash, Role: "operator", Disabled: true}); err != nil {
		t.Fatal(err)
	}
	assertEnabled(false)
	newSessionAccount(t, s, "operator", "operator")
	assertEnabled(true)
}

func TestPrepareConsoleActivationRevokesAllSessionsWithoutChangingAccounts(t *testing.T) {
	for _, profile := range []string{"argon2id", "scrypt"} {
		t.Run(profile, func(t *testing.T) {
			s, clock, file := testStore(t)
			operator := newSessionAccount(t, s, "operator", "operator")
			if profile == "scrypt" {
				operator = createTestConsoleAccount(t, s, "imported operator", "operator")
			}
			other := newSessionAccount(t, s, "other", "member")
			// Include an expired session and sessions for unrelated accounts. Owner
			// activation clears all of them, not only the verifying operator's.
			now := clock.Now().UnixMilli()
			for i, account := range []ConsoleAccount{operator, other, other} {
				digest := make([]byte, 32)
				digest[0] = byte(i + 1)
				if _, err := s.db.Exec("INSERT INTO ConsoleSession VALUES(?,?,?,?,?)", digest, account.ID, account.AuthVersion, now-(2*time.Hour).Milliseconds(), now+(time.Duration(i-1)*time.Hour).Milliseconds()); err != nil {
					t.Fatal(err)
				}
			}
			beforeOther, err := s.GetConsoleAccount(t.Context(), other.ID)
			if err != nil {
				t.Fatal(err)
			}
			changes := s.Changes()
			got, err := s.PrepareConsoleActivation(t.Context(), operator)
			if err != nil {
				t.Fatal(err)
			}
			want := operator
			want.LoginKey, want.PasswordHash = "", ""
			if !reflect.DeepEqual(got, want) {
				t.Fatal("activation returned credentials or changed the account")
			}
			select {
			case <-changes:
			default:
				t.Fatal("activation did not wake revoked session watchers")
			}
			if rowCount(t, s, "SELECT COUNT(*) FROM ConsoleSession") != 0 {
				t.Fatal("activation retained an old or unrelated session")
			}
			current, err := s.GetConsoleAccount(t.Context(), operator.ID)
			if err != nil || !reflect.DeepEqual(current, operator) {
				t.Fatal("verification changed operator credentials, versions or login time", err)
			}
			currentOther, err := s.GetConsoleAccount(t.Context(), other.ID)
			if err != nil || !reflect.DeepEqual(currentOther, beforeOther) {
				t.Fatal("verification changed another account", err)
			}
			if rowCount(t, s, "SELECT COUNT(*) FROM ConsoleAction WHERE actor_kind='owner' AND actor_account_id IS NULL AND target_account_id=? AND action='activation_verified' AND outcome='accepted'", operator.ID) != 1 {
				t.Fatal("verification was not attributed to the owner")
			}
			if err = s.Close(); err != nil {
				t.Fatal(err)
			}
			s.lock = nil
			reopened, err := Open(file, clock)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = reopened.Close() }()
			if rowCount(t, reopened, "SELECT COUNT(*) FROM ConsoleSession") != 0 || rowCount(t, reopened, "SELECT COUNT(*) FROM ConsoleAction WHERE action='activation_verified'") != 1 {
				t.Fatal("restart lost the session clearing or verification record")
			}
		})
	}
}

func TestPrepareConsoleActivationRefusesUnusableOrChangedOperator(t *testing.T) {
	for _, change := range []string{"malformed-hash", "unsupported-hash", "missing", "password-reset", "version", "hash", "member", "none", "disabled"} {
		t.Run(change, func(t *testing.T) {
			s, _, _ := testStore(t)
			operator := newSessionAccount(t, s, "operator", "operator")
			other := newSessionAccount(t, s, "other", "member")
			issueTestSession(t, s, other, 1)
			code := "unauthorized"
			switch change {
			case "malformed-hash":
				operator.PasswordHash = "private malformed password record"
				code = "bad_request"
			case "unsupported-hash":
				operator.PasswordHash = "$argon2id$v=19$m=999999999,t=3,p=1$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
				code = "bad_request"
			case "missing":
				operator.ID = "ca_missing"
			case "password-reset":
				if _, err := s.RecoverConsoleOperator(t.Context(), operator, consoleOtherArgonTestHash); err != nil {
					t.Fatal(err)
				}
			case "version":
				operator.AuthVersion++
			case "hash":
				operator.PasswordHash = consoleOtherArgonTestHash
			case "member", "none":
				if _, err := s.db.Exec("UPDATE ConsoleAccount SET role=?,disabled=? WHERE id=?", change, change == "none", operator.ID); err != nil {
					t.Fatal(err)
				}
			case "disabled":
				if _, err := s.db.Exec("UPDATE ConsoleAccount SET disabled=1 WHERE id=?", operator.ID); err != nil {
					t.Fatal(err)
				}
			}
			before, changes := databaseState(t, s), s.Changes()
			got, err := s.PrepareConsoleActivation(t.Context(), operator)
			consoleError(t, err, code)
			if !reflect.DeepEqual(got, ConsoleAccount{}) || databaseState(t, s) != before || s.Changes() != changes {
				t.Fatal("refused activation returned an account or changed sessions, audit or coordination")
			}
		})
	}
}

func TestConsoleActivationStorageFailuresRollbackAndRetry(t *testing.T) {
	for _, operation := range []string{"inspect", "prepare"} {
		t.Run(operation, func(t *testing.T) {
			s, _, file := testStore(t)
			operator := newSessionAccount(t, s, "operator", "operator")
			other := newSessionAccount(t, s, "other", "member")
			issueTestSession(t, s, operator, 1)
			issueTestSession(t, s, other, 2)
			invoke := func() (ConsoleAccount, bool, error) {
				if operation == "inspect" {
					enabled, err := s.HasEnabledConsoleOperator(t.Context())
					return ConsoleAccount{}, enabled, err
				}
				account, err := s.PrepareConsoleActivation(t.Context(), operator)
				return account, false, err
			}
			fault := installFaultDriver(t, s, file)
			fault.discover = true
			_, _, err := invoke()
			if (operation == "prepare" && !errors.Is(err, errStorageFault)) || (operation == "inspect" && err != nil) {
				t.Fatal("storage discovery lost the intended transaction boundary", err)
			}
			trace := append([]string(nil), fault.trace...)
			fault.discover = false
			before, changes := databaseState(t, s), s.Changes()
			for i := range trace {
				fault.count, fault.at, fault.trace = 0, i+1, nil
				account, enabled, err := invoke()
				fault.at = 0
				if !errors.Is(err, errStorageFault) || !reflect.DeepEqual(account, ConsoleAccount{}) || enabled {
					t.Fatalf("boundary %d returned activation state or lost its storage error: %v", i+1, err)
				}
				if databaseState(t, s) != before || s.Changes() != changes {
					t.Fatalf("boundary %d exposed partial session clearing, audit or notification", i+1)
				}
			}
			account, enabled, err := invoke()
			if err != nil || (operation == "inspect" && !enabled) || (operation == "prepare" && (account.ID != operator.ID || account.PasswordHash != "" || account.LoginKey != "" || rowCount(t, s, "SELECT COUNT(*) FROM ConsoleSession") != 0)) {
				t.Fatal("storage retry did not safely complete", err)
			}
		})
	}
}

func TestCanceledConsoleActivationCannotExposeOrChangeState(t *testing.T) {
	s, _, _ := testStore(t)
	operator := newSessionAccount(t, s, "operator", "operator")
	issueTestSession(t, s, operator, 1)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	before, changes := databaseState(t, s), s.Changes()
	got, err := s.PrepareConsoleActivation(ctx, operator)
	if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(got, ConsoleAccount{}) {
		t.Fatal("canceled activation returned account data or lost cancellation", err)
	}
	enabled, err := s.HasEnabledConsoleOperator(ctx)
	if !errors.Is(err, context.Canceled) || enabled || databaseState(t, s) != before || s.Changes() != changes {
		t.Fatal("canceled inspection or activation changed durable state", err)
	}
}

func TestConcurrentActivationAndRecoveryRespectTheVerifiedSnapshot(t *testing.T) {
	for range 6 {
		s, _, _ := testStore(t)
		operator := newSessionAccount(t, s, "operator", "operator")
		other := newSessionAccount(t, s, "other", "member")
		issueTestSession(t, s, operator, 1)
		issueTestSession(t, s, other, 2)
		start := make(chan struct{})
		var wg sync.WaitGroup
		var prepared ConsoleAccount
		var prepareErr, recoveryErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			prepared, prepareErr = s.PrepareConsoleActivation(t.Context(), operator)
		}()
		go func() {
			defer wg.Done()
			<-start
			_, recoveryErr = s.RecoverConsoleOperator(t.Context(), operator, consoleOtherArgonTestHash)
		}()
		close(start)
		wg.Wait()
		if recoveryErr != nil {
			t.Fatal("verification prevented owner password recovery", recoveryErr)
		}
		wantAudit, wantOtherSessions := 1, 0
		if prepareErr != nil {
			consoleError(t, prepareErr, "unauthorized")
			if !reflect.DeepEqual(prepared, ConsoleAccount{}) {
				t.Fatal("stale verification returned account data")
			}
			wantAudit, wantOtherSessions = 0, 1
		} else if prepared.ID != operator.ID || prepared.PasswordHash != "" || prepared.LoginKey != "" {
			t.Fatal("accepted verification returned credentials")
		}
		current, err := s.GetConsoleAccount(t.Context(), operator.ID)
		if err != nil || current.PasswordHash != consoleOtherArgonTestHash || current.AuthVersion != operator.AuthVersion+1 || sessionCount(t, s, operator) != 0 || sessionCount(t, s, other) != wantOtherSessions || rowCount(t, s, "SELECT COUNT(*) FROM ConsoleAction WHERE action='activation_verified'") != wantAudit {
			t.Fatal("activation/recovery race restored authority or exposed a partial verification", err)
		}
	}
}
