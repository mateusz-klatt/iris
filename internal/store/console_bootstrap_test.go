package store

import (
	"errors"
	"sync"
	"testing"
)

func TestFreshConsoleProvisioningRollsBackEveryBoundaryAndRetries(t *testing.T) {
	s, _, file := testStore(t)
	input := ConsoleAccountInput{Username: "fresh operator", PasswordHash: consoleArgonTestHash}
	invoke := func() (ConsoleAccount, bool, error) {
		return s.ProvisionConsoleOperatorWithActivation(t.Context(), input, func() bool { return true })
	}
	fault := installFaultDriver(t, s, file)
	fault.discover = true
	if account, prepare, err := invoke(); !errors.Is(err, errStorageFault) || prepare || account.ID != "" {
		t.Fatal("failed commit exposed provisioning or activation", err)
	}
	trace := append([]string(nil), fault.trace...)
	fault.discover = false
	before := databaseState(t, s)
	for i := range trace {
		fault.count, fault.at, fault.trace = 0, i+1, nil
		account, prepare, err := invoke()
		fault.at = 0
		if !errors.Is(err, errStorageFault) || prepare || account.ID != "" || databaseState(t, s) != before {
			t.Fatalf("boundary %d left partial provisioning or activation: %v", i+1, err)
		}
	}
	account, prepare, err := invoke()
	if err != nil || !prepare || account.Role != "operator" || !account.Enabled() {
		t.Fatal("retry did not provision the fresh operator", err)
	}
	if rowCount(t, s, "SELECT COUNT(*) FROM ConsoleAction WHERE actor_kind='owner' AND actor_account_id IS NULL AND target_account_id=? AND action='owner_provision' AND outcome='accepted'", account.ID) != 1 || rowCount(t, s, "SELECT COUNT(*) FROM ConsoleAction WHERE action='activation_prepared'") != 0 {
		t.Fatal("fresh provisioning omitted its audit or claimed key preparation")
	}
}

func TestFreshConsoleProvisioningRechecksAccountsUnderConcurrency(t *testing.T) {
	s, _, _ := testStore(t)
	var wg sync.WaitGroup
	prepared := make(chan bool, 2)
	for _, name := range []string{"first candidate", "second candidate"} {
		wg.Go(func() {
			_, prepare, err := s.ProvisionConsoleOperatorWithActivation(t.Context(), ConsoleAccountInput{Username: name, PasswordHash: consoleArgonTestHash}, func() bool { return true })
			if err != nil {
				t.Error(err)
			}
			prepared <- prepare
		})
	}
	wg.Wait()
	if first, second := <-prepared, <-prepared; first == second || rowCount(t, s, "SELECT COUNT(*) FROM ConsoleAccount") != 2 || rowCount(t, s, "SELECT COUNT(*) FROM ConsoleAction WHERE action='activation_prepared'") != 0 {
		t.Fatal("concurrent creates misreported eligibility or claimed key preparation")
	}
}

func TestFreshConsoleProvisioningRejectsUnsupportedNewHash(t *testing.T) {
	s, _, _ := testStore(t)
	before := databaseState(t, s)
	account, prepare, err := s.ProvisionConsoleOperatorWithActivation(t.Context(), ConsoleAccountInput{Username: "fresh operator", PasswordHash: "invalid"}, func() bool { t.Fatal("invalid material inspected the key"); return true })
	if err == nil || account.ID != "" || prepare || databaseState(t, s) != before {
		t.Fatal("invalid provisioning material changed the hub")
	}
}
