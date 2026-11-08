package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestConsoleOperationStorageFailuresRollBackAndRetry(t *testing.T) {
	for _, operation := range []string{"console-remember", "console-cached", "console-delete-message", "console-delete-delivery", "console-sweep"} {
		t.Run(operation, func(t *testing.T) {
			s, clock, file := testStore(t)
			_, registered := session(t, s, 1)
			a := operationAccount(t, s, "author")
			r := consoleMessageRequest{RepositoryID: registered["repository_id"].(string), To: []string{registered["agent"].(map[string]any)["id"].(string)}, Body: "durable named message"}
			accepted := acceptedConsoleSend(t, s, a, "accepted", r)
			if operation == "console-sweep" {
				clock.advance(25 * time.Hour)
			}
			invoke := func() error {
				if operation == "console-sweep" {
					_, err := s.Sweep(context.Background())
					return err
				}
				return s.transact(context.Background(), func(tx *transaction) error {
					switch operation {
					case "console-remember":
						_, err := stagedConsoleSend(tx, a, "new", r, true)
						return err
					case "console-cached":
						_, err := tx.consoleOperation(a.ID, "accepted", "send", r)
						return err
					case "console-delete-message":
						if _, err := tx.exec("DELETE FROM Delivery WHERE message_id=?", accepted.MessageID); err != nil {
							return err
						}
						_, err := tx.exec("DELETE FROM Message WHERE id=?", accepted.MessageID)
						return err
					default:
						_, err := tx.exec("DELETE FROM Delivery WHERE message_id=?", accepted.MessageID)
						return err
					}
				})
			}
			fault := installFaultDriver(t, s, file)
			fault.discover = true
			if err := invoke(); !errors.Is(err, errStorageFault) {
				t.Fatal("discovery did not roll back", err)
			}
			trace := append([]string(nil), fault.trace...)
			fault.discover = false
			before := databaseState(t, s)
			for index, statement := range trace {
				fault.at, fault.count, fault.trace = index+1, 0, nil
				err := invoke()
				fault.at = 0
				if !errors.Is(err, errStorageFault) {
					t.Fatalf("%s fault %d did not remain retryable: %v", statement, index+1, err)
				}
				if databaseState(t, s) != before {
					t.Fatalf("%s fault %d exposed partial account/retry/message state", statement, index+1)
				}
			}
			if err := invoke(); err != nil {
				t.Fatal("retry failed after storage recovered", err)
			}
			switch operation {
			case "console-remember":
				if rowCount(t, s, "SELECT COUNT(*) FROM Message") != 2 || rowCount(t, s, "SELECT COUNT(*) FROM ConsoleOperation") != 2 {
					t.Fatal("retry duplicated or lost the accepted send")
				}
			case "console-cached":
				if databaseState(t, s) != before {
					t.Fatal("cached lookup changed state")
				}
			case "console-sweep":
				if rowCount(t, s, "SELECT COUNT(*) FROM ConsoleOperation") != 0 || rowCount(t, s, "SELECT COUNT(*) FROM Message") != 1 {
					t.Fatal("retention removed mail or retained an expired key")
				}
			default:
				if rowCount(t, s, "SELECT COUNT(*) FROM ConsoleOperation WHERE tombstone=1 AND result_json IS NULL") != 1 {
					t.Fatal("deletion did not preserve the retired key")
				}
			}
		})
	}
}

func TestConsoleRetryFailureDoesNotCommitAnotherMessage(t *testing.T) {
	s, _, _ := testStore(t)
	_, registered := session(t, s, 1)
	a := operationAccount(t, s, "author")
	r := consoleMessageRequest{RepositoryID: registered["repository_id"].(string), To: []string{registered["agent"].(map[string]any)["id"].(string)}}
	acceptedConsoleSend(t, s, a, "same", r)
	before := databaseState(t, s)
	err := s.transact(context.Background(), func(tx *transaction) error {
		_, err := stagedConsoleSend(tx, a, "same", r, true)
		return err
	})
	if err == nil || databaseState(t, s) != before {
		t.Fatal("retry conflict committed a second message")
	}
}
