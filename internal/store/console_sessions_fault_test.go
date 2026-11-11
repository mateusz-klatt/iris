package store

import (
	"errors"
	"testing"
)

// These sweeps interrupt real SQLite at every persistence/read boundary. Login
// and password recovery must leave no partial credential or session.
func TestConsoleSessionStorageFailuresRollbackAndRetry(t *testing.T) {
	for _, operation := range []string{"login", "inspect", "inspect-account", "logout", "logout-absent", "password", "provision", "recovery", "frame", "send"} {
		t.Run(operation, func(t *testing.T) {
			s, _, file := testStore(t)
			a := newSessionAccount(t, s, "existing", "member")
			p := issueTestSession(t, s, a, 1)
			_, sr := session(t, s, 1)
			grantNamed(t, s, a, sr["repository_id"].(string), "rw")
			invoke := func() error {
				var err error
				switch operation {
				case "login":
					digest := make([]byte, 32)
					digest[0] = 2
					_, err = s.IssueConsoleSession(t.Context(), a, digest)
				case "inspect":
					_, err = s.ConsoleSessionInfo(t.Context(), p.SessionDigest)
				case "inspect-account":
					_, err = s.ConsoleSessionAccount(t.Context(), p.SessionDigest)
				case "logout":
					err = s.DeleteConsoleSession(t.Context(), p.SessionDigest)
				case "logout-absent":
					err = s.DeleteConsoleSession(t.Context(), make([]byte, 32))
				case "password":
					err = s.ChangeConsolePassword(t.Context(), p, a, consoleOtherArgonTestHash)
				case "provision":
					_, err = s.ProvisionConsoleOperator(t.Context(), ConsoleAccountInput{Username: "new operator", PasswordHash: consoleArgonTestHash})
				case "recovery":
					_, err = s.RecoverConsoleOperator(t.Context(), a, consoleOtherArgonTestHash)
				case "send":
					_, err = s.CallConsole(t.Context(), p, "ui_messages", Request{RepositoryID: sr["repository_id"].(string), Key: "fault-send", To: []string{sr["agent"].(map[string]any)["id"].(string)}, Body: "transactional send"})
				case "frame":
					err = s.AdmitConsoleFrame(t.Context(), p, func(map[string]any) error { return nil })
				}
				return err
			}
			fault := installFaultDriver(t, s, file)
			fault.discover = true
			if err := invoke(); !errors.Is(err, errStorageFault) {
				t.Fatal("discovery failed", err)
			}
			trace := append([]string(nil), fault.trace...)
			fault.discover = false
			before := databaseState(t, s)
			for i := range trace {
				fault.count, fault.at, fault.trace = 0, i+1, nil
				err := invoke()
				fault.at = 0
				if !errors.Is(err, errStorageFault) {
					t.Fatalf("boundary %d lost storage error: %v", i+1, err)
				}
				if databaseState(t, s) != before {
					t.Fatalf("boundary %d committed partial credentials", i+1)
				}
			}
			if err := invoke(); err != nil {
				t.Fatal("retry failed", err)
			}
			switch operation {
			case "password", "recovery", "logout":
				if sessionCount(t, s, a) != 0 {
					t.Fatal("retry failed to revoke sessions")
				}
			}
		})
	}
}
