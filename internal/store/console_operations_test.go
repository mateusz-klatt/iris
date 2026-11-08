package store

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

func operationAccount(t *testing.T, s *Store, username string) ConsoleAccount {
	t.Helper()
	a, err := s.CreateConsoleAccount(context.Background(), ConsoleAccountInput{
		Username: username, Role: "member", DisplayName: "Person " + username,
		PasswordHash: "scrypt$16384$8$1$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
	})
	if err != nil {
		t.Fatal("create fixture account:", err)
	}
	return a
}

// This fixture writes the accepted transport records and account retry in one
// transaction. It deliberately does not add an account-authorized send surface.
func stagedConsoleSend(tx *transaction, account ConsoleAccount, key string, request consoleMessageRequest, remember bool) (consoleSendResult, error) {
	var out consoleSendResult
	e, err := tx.operatorSender(request.RepositoryID)
	if err != nil {
		return out, err
	}
	out.MessageID, out.ThreadID = id("ms_"), id("th_")
	if _, err = tx.exec(`INSERT INTO Message(id,sender_execution_id,subject,body,created_ms,repository_id,thread_id,sender_account_id,sender_username,sender_display_name)
 VALUES(?,?,?,?,?,?,?,?,?,?)`, out.MessageID, e.ID, request.Subject, request.Body, tx.s.clock.Now().UnixMilli(), e.Repo, out.ThreadID, account.ID, account.Username, account.DisplayName); err != nil {
		return out, err
	}
	for _, recipient := range request.To {
		delivery := consoleDeliveryResult{ID: id("dl_"), Recipient: recipient, RepositoryID: request.RepositoryID}
		if _, err = tx.exec("INSERT INTO Delivery(id,message_id,recipient_agent_id) VALUES(?,?,?)", delivery.ID, out.MessageID, recipient); err != nil {
			return out, err
		}
		out.Deliveries = append(out.Deliveries, delivery)
	}
	if remember {
		err = tx.rememberConsoleOperation(account.ID, key, "send", request, out)
	}
	return out, err
}

func acceptedConsoleSend(t *testing.T, s *Store, account ConsoleAccount, key string, request consoleMessageRequest) consoleSendResult {
	t.Helper()
	var result consoleSendResult
	err := s.transact(context.Background(), func(tx *transaction) error {
		var err error
		result, err = stagedConsoleSend(tx, account, key, request, true)
		return err
	})
	if err != nil {
		t.Fatal("persist staged console send:", err)
	}
	return result
}

func lookupConsoleOperation(t *testing.T, s *Store, account, key, operation string, r consoleMessageRequest) (*consoleOperation, error) {
	t.Helper()
	var saved *consoleOperation
	err := s.transact(context.Background(), func(tx *transaction) error {
		var err error
		saved, err = tx.consoleOperation(account, key, operation, r)
		return err
	})
	return saved, err
}

func assertConsoleError(t *testing.T, err error, code string) {
	t.Helper()
	var domain *Error
	if !errors.As(err, &domain) || domain.Code != code {
		t.Fatalf("wanted %s; got %v", code, err)
	}
}

func TestConsoleRetriesBelongToAccountsAndKeepAcceptedRecords(t *testing.T) {
	s, clock, file := testStore(t)
	_, registered := session(t, s, 1)
	repo := registered["repository_id"].(string)
	recipient := registered["agent"].(map[string]any)["id"].(string)
	a, b := operationAccount(t, s, "one"), operationAccount(t, s, "two")
	r := consoleMessageRequest{RepositoryID: repo, To: []string{recipient}, Subject: "Accepted", Body: "shared human transport"}
	if missing, err := lookupConsoleOperation(t, s, a.ID, "same", "send", r); missing != nil || err != nil {
		t.Fatal("unused key was occupied", err)
	}
	first := acceptedConsoleSend(t, s, a, "same", r)
	second := acceptedConsoleSend(t, s, b, "same", r)
	if first.MessageID == second.MessageID {
		t.Fatal("two accounts were coalesced")
	}
	for _, pair := range []struct {
		account ConsoleAccount
		result  consoleSendResult
	}{{a, first}, {b, second}} {
		saved, err := lookupConsoleOperation(t, s, pair.account.ID, "same", "send", r)
		if err != nil || saved == nil || saved.OriginRepositoryID != repo || saved.MessageID != pair.result.MessageID || saved.CreatedMS != clock.Now().UnixMilli() || !reflect.DeepEqual(saved.RecipientIDs, []string{recipient}) || !reflect.DeepEqual(saved.Result, pair.result) {
			t.Fatal("accepted result was not preserved for its author", err)
		}
	}
	for _, mutate := range []func(*consoleMessageRequest){
		func(r *consoleMessageRequest) { r.RepositoryID = "elsewhere" },
		func(r *consoleMessageRequest) { r.To = []string{"another-agent"} },
		func(r *consoleMessageRequest) { r.Subject = "changed" },
		func(r *consoleMessageRequest) { r.Body = "changed" },
		func(r *consoleMessageRequest) { r.ReplyTo = "hidden-parent" },
	} {
		changed := r
		mutate(&changed)
		_, err := lookupConsoleOperation(t, s, a.ID, "same", "send", changed)
		assertConsoleError(t, err, "key_reused")
	}
	_, err := lookupConsoleOperation(t, s, a.ID, "same", "reply", r)
	assertConsoleError(t, err, "key_reused")
	// Sessions and process lifetime do not select the namespace.
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s.lock = nil
	reopened, err := Open(file, clock)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	saved, err := lookupConsoleOperation(t, reopened, a.ID, "same", "send", r)
	if err != nil || saved == nil || !reflect.DeepEqual(saved.Result, first) || rowCount(t, reopened, "SELECT COUNT(*) FROM Message") != 2 {
		t.Fatal("restart lost or duplicated the accepted result", err)
	}
}

func TestLegacyConsoleRetriesNeverBecomeAccountRetries(t *testing.T) {
	s, _, _ := testStore(t)
	_, registered := session(t, s, 1)
	repo := registered["repository_id"].(string)
	recipient := registered["agent"].(map[string]any)["id"].(string)
	legacy := call(t, s, "ui_messages", Request{RepositoryID: repo, Key: "legacy", To: []string{recipient}, Body: "old shared send"})
	a := operationAccount(t, s, "person")
	r := consoleMessageRequest{RepositoryID: repo, To: []string{recipient}, Body: "old shared send"}
	if saved, err := lookupConsoleOperation(t, s, a.ID, "legacy", "send", r); err != nil || saved != nil {
		t.Fatal("legacy shared cache was attributed to an account", err)
	}
	err := s.transact(context.Background(), func(tx *transaction) error {
		return tx.rememberConsoleOperation(a.ID, "legacy", "send", r, consoleSendResult{MessageID: legacy["message_id"].(string)})
	})
	assertConsoleError(t, err, "conflict")
	if rowCount(t, s, "SELECT COUNT(*) FROM ConsoleOperation") != 0 || rowCount(t, s, "SELECT COUNT(*) FROM Message WHERE sender_account_id IS NOT NULL") != 0 {
		t.Fatal("a legacy entry acquired account attribution")
	}
}

func TestConsoleRetryTombstonesSurviveDeletedResultsAndExpireAfterADay(t *testing.T) {
	for _, deleted := range []string{"message", "delivery", "recipient"} {
		t.Run(deleted, func(t *testing.T) {
			s, clock, _ := testStore(t)
			_, registered := session(t, s, 1)
			repo := registered["repository_id"].(string)
			recipient := registered["agent"].(map[string]any)["id"].(string)
			a := operationAccount(t, s, "author")
			r := consoleMessageRequest{RepositoryID: repo, To: []string{recipient}, Body: "accepted"}
			out := acceptedConsoleSend(t, s, a, "retained", r)
			if err := s.transact(context.Background(), func(tx *transaction) error {
				switch deleted {
				case "message":
					if _, err := tx.exec("DELETE FROM Delivery WHERE message_id=?", out.MessageID); err != nil {
						return err
					}
					_, err := tx.exec("DELETE FROM Message WHERE id=?", out.MessageID)
					return err
				case "delivery":
					_, err := tx.exec("DELETE FROM Delivery WHERE id=?", out.Deliveries[0].ID)
					return err
				default:
					// No deliveries remain, but the saved recipient IDs must still be
					// enough to retire the key when its mailbox itself disappears.
					if _, err := tx.exec("DELETE FROM Delivery WHERE message_id=?", out.MessageID); err != nil {
						return err
					}
					if _, err := tx.exec("UPDATE ConsoleOperation SET tombstone=0,result_json='{}' WHERE account_id=?", a.ID); err != nil {
						return err
					}
					if _, err := tx.exec("DELETE FROM Event WHERE agent_id=?", recipient); err != nil {
						return err
					}
					if _, err := tx.exec("DELETE FROM Execution WHERE agent_id=?", recipient); err != nil {
						return err
					}
					_, err := tx.exec("DELETE FROM Agent WHERE id=?", recipient)
					return err
				}
			}); err != nil {
				t.Fatal("delete accepted records:", err)
			}
			if n := rowCount(t, s, "SELECT COUNT(*) FROM ConsoleOperation WHERE account_id=? AND tombstone=1 AND result_json IS NULL", a.ID); n != 1 {
				t.Fatal("deleted result did not leave a tombstone")
			}
			_, err := lookupConsoleOperation(t, s, a.ID, "retained", "send", r)
			assertConsoleError(t, err, "not_found")
			changed := r
			changed.Body = "fresh attempt"
			_, err = lookupConsoleOperation(t, s, a.ID, "retained", "send", changed)
			assertConsoleError(t, err, "key_reused")
			clock.advance(24 * time.Hour)
			sweep(t, s)
			if rowCount(t, s, "SELECT COUNT(*) FROM ConsoleOperation") != 1 {
				t.Fatal("tombstone expired before its full retention horizon")
			}
			clock.advance(time.Millisecond)
			if got := sweep(t, s); got["operations"] != 1 || rowCount(t, s, "SELECT COUNT(*) FROM ConsoleOperation") != 0 {
				t.Fatal("tombstone did not expire after 24 hours", got)
			}
		})
	}
}

func TestConsoleRetryStorageRefusesMissingIdentityAndCorruptResults(t *testing.T) {
	s, _, _ := testStore(t)
	_, registered := session(t, s, 1)
	a := operationAccount(t, s, "author")
	r := consoleMessageRequest{RepositoryID: registered["repository_id"].(string), To: []string{registered["agent"].(map[string]any)["id"].(string)}}
	acceptedConsoleSend(t, s, a, "saved", r)
	for _, invalid := range []struct{ account, key, operation string }{{"", "saved", "send"}, {a.ID, "", "send"}, {a.ID, "saved", ""}} {
		_, err := lookupConsoleOperation(t, s, invalid.account, invalid.key, invalid.operation, r)
		assertConsoleError(t, err, "bad_request")
		err = s.transact(context.Background(), func(tx *transaction) error {
			return tx.rememberConsoleOperation(invalid.account, invalid.key, invalid.operation, r, consoleSendResult{})
		})
		assertConsoleError(t, err, "bad_request")
	}
	// Valid SQLite JSON can still have the wrong shape for a persisted result.
	for _, bad := range []struct{ column, value string }{{"recipient_ids_json", "[1]"}, {"result_json", "false"}} {
		if _, err := s.db.Exec("UPDATE ConsoleOperation SET "+bad.column+"=? WHERE key='saved'", bad.value); err != nil {
			t.Fatal(err)
		}
		if _, err := lookupConsoleOperation(t, s, a.ID, "saved", "send", r); err == nil {
			t.Fatal("corrupt stored result was accepted")
		}
		if _, err := s.db.Exec("UPDATE ConsoleOperation SET recipient_ids_json='[]',result_json='{}' WHERE key='saved'"); err != nil {
			t.Fatal(err)
		}
	}
}
