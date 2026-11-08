package store

import (
	"context"
	"errors"
	"testing"
)

// Each storage boundary is exercised against real SQLite. A failed account edit
// cannot leave grants, versions or session revocation partially committed.
func TestConsoleStorageFailuresRollBackAndRetry(t *testing.T) {
	for _, operation := range []string{"create", "get", "lookup", "profile", "access", "repository-mode"} {
		t.Run(operation, func(t *testing.T) {
			s, _, file := testStore(t)
			_, session := session(t, s, 1)
			repo := session["repository_id"].(string)
			a := createTestConsoleAccount(t, s, "Member", "member")
			if _, err := s.db.Exec("INSERT INTO ConsoleRepositoryAccess VALUES(?,?,'ro')", a.ID, repo); err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.Exec("INSERT INTO ConsoleSession VALUES(zeroblob(32),?,1,0,43200000)", a.ID); err != nil {
				t.Fatal(err)
			}
			invoke := func() error {
				ctx := context.Background()
				var err error
				switch operation {
				case "create":
					_, err = s.CreateConsoleAccount(ctx, ConsoleAccountInput{Username: "New", PasswordHash: consoleTestHash, Role: "member"})
				case "get":
					_, err = s.GetConsoleAccount(ctx, a.ID)
				case "lookup":
					_, err = s.LookupConsoleAccount(ctx, "MEMBER")
				case "profile":
					_, err = s.UpdateConsoleProfile(ctx, a.ID, 1, "New display", "en", "pl")
				case "access":
					_, err = s.ReplaceConsoleAccess(ctx, a.ID, 1, "member", false, []ConsoleGrant{{repo, "rw"}})
				case "repository-mode":
					_, err = s.ConsoleRepositoryMode(ctx, a.ID, repo)
				}
				return err
			}
			fault := installFaultDriver(t, s, file)
			fault.discover = true
			if err := invoke(); !errors.Is(err, errStorageFault) {
				t.Fatal("discovery did not refuse commit", err)
			}
			trace := append([]string(nil), fault.trace...)
			fault.discover = false
			before := databaseState(t, s)
			for i := range trace {
				fault.count, fault.at, fault.trace = 0, i+1, nil
				err := invoke()
				fault.at = 0
				if !errors.Is(err, errStorageFault) {
					t.Fatalf("boundary %d failed without preserving storage error: %v", i+1, err)
				}
				if databaseState(t, s) != before {
					t.Fatalf("boundary %d committed partial account state", i+1)
				}
			}
			if err := invoke(); err != nil {
				t.Fatal("retry failed", err)
			}
			var count int
			if err := s.db.QueryRow("SELECT COUNT(*) FROM ConsoleSession WHERE account_id=?", a.ID).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if (operation == "access" && count != 0) || (operation != "access" && count != 1) {
				t.Fatal("retry did not retain intended session state")
			}
		})
	}
}
