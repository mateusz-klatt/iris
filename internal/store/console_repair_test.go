package store

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestMergeRequiresReviewOfBothMemberGrantModesOnEitherRepository(t *testing.T) {
	for _, mode := range []string{"ro", "rw"} {
		for _, side := range []string{"source", "destination"} {
			t.Run(mode+"/"+side, func(t *testing.T) {
				s, _, _ := testStore(t)
				_, ar, _, br := mergeSplit(t, s)
				from, into := br["repository_id"].(string), ar["repository_id"].(string)
				grant := from
				if side == "destination" {
					grant = into
				}
				a := createTestConsoleAccount(t, s, "Member", "member")
				// Disabled accounts still need an explicit grant review before repair.
				a, err := s.ReplaceConsoleAccess(t.Context(), a.ID, a.EditRevision, "member", true, []ConsoleGrant{{grant, mode}})
				if err != nil {
					t.Fatal(err)
				}
				before := databaseState(t, s)
				for _, dry := range []bool{true, false} {
					expectError(t, s, "merge", mergeRequest(from, into, dry), "conflict")
					if databaseState(t, s) != before {
						t.Fatal("a refused repair changed account or repository state")
					}
				}
				if _, err = s.ReplaceConsoleAccess(t.Context(), a.ID, a.EditRevision, "member", true, nil); err != nil {
					t.Fatal(err)
				}
				call(t, s, "merge", mergeRequest(from, into, false))
				if count(t, s, "SELECT COUNT(*) FROM Repository WHERE id=?", from) != 0 {
					t.Fatal("repair failed after explicit grant removal")
				}
			})
		}
	}
}

func TestMergePreservesPeopleAuthorshipAndRetryEvidence(t *testing.T) {
	s, _, _ := testStore(t)
	_, ar, _, br := mergeSplit(t, s)
	from, into := br["repository_id"].(string), ar["repository_id"].(string)
	recipient := ar["agent"].(map[string]any)["id"].(string)
	a := createTestConsoleAccount(t, s, "First", "operator")
	b := createTestConsoleAccount(t, s, "Second", "operator")
	r := consoleMessageRequest{RepositoryID: from, To: []string{recipient}, Subject: "repair evidence", Body: "shared transport, separate people"}
	first := acceptedConsoleSend(t, s, a, "same-key", r)
	second := acceptedConsoleSend(t, s, b, "same-key", r)
	call(t, s, "merge", mergeRequest(from, into, false))
	for _, pair := range []struct {
		account ConsoleAccount
		result  consoleSendResult
	}{{a, first}, {b, second}} {
		var accountID, username, display, repo string
		if err := s.db.QueryRow("SELECT sender_account_id,sender_username,sender_display_name,repository_id FROM Message WHERE id=?", pair.result.MessageID).Scan(&accountID, &username, &display, &repo); err != nil {
			t.Fatal(err)
		}
		if accountID != pair.account.ID || username != pair.account.Username || display != pair.account.DisplayName || repo != into {
			t.Fatal("repair changed immutable authorship or failed to move origin")
		}
		saved, err := lookupConsoleOperation(t, s, pair.account.ID, "same-key", "send", r)
		if err != nil || saved == nil || saved.OriginRepositoryID != from || !reflect.DeepEqual(saved.Result, pair.result) {
			t.Fatal("repair coalesced people or rewrote retry evidence", err)
		}
	}
}

func TestForgetRemovesGrantsAndRevokesOnlyAffectedAccountsAtomically(t *testing.T) {
	s, _, _ := testStore(t)
	_, gone := ended(t, s, "gone-with-grants")
	_, kept := ended(t, s, "kept-with-grants")
	repo, keep := gone["repository_id"].(string), kept["repository_id"].(string)
	a := createTestConsoleAccount(t, s, "ReadOnly", "member")
	b := createTestConsoleAccount(t, s, "Writer", "member")
	c := createTestConsoleAccount(t, s, "Unaffected", "member")
	for index, entry := range []struct {
		account ConsoleAccount
		grants  []ConsoleGrant
	}{{a, []ConsoleGrant{{repo, "ro"}, {keep, "rw"}}}, {b, []ConsoleGrant{{repo, "rw"}}}, {c, []ConsoleGrant{{keep, "ro"}}}} {
		if _, err := s.ReplaceConsoleAccess(t.Context(), entry.account.ID, 1, "member", false, entry.grants); err != nil {
			t.Fatal(err)
		}
		digest := make([]byte, 32)
		digest[0] = byte(index)
		if _, err := s.db.Exec("INSERT INTO ConsoleSession VALUES(?,?,2,0,43200000)", digest, entry.account.ID); err != nil {
			t.Fatal(err)
		}
	}
	before := databaseState(t, s)
	call(t, s, "forget", Request{RepositoryID: repo, DryRun: true})
	if databaseState(t, s) != before {
		t.Fatal("forget dry run changed grants, sessions or revisions")
	}
	call(t, s, "forget", Request{RepositoryID: repo})
	for _, account := range []ConsoleAccount{a, b, c} {
		got, err := s.GetConsoleAccount(t.Context(), account.ID)
		if err != nil {
			t.Fatal(err)
		}
		wantVersion, wantSessions := int64(3), 0
		if account.ID == c.ID {
			wantVersion, wantSessions = 2, 1
		}
		if got.AuthVersion != wantVersion || got.EditRevision != wantVersion || count(t, s, "SELECT COUNT(*) FROM ConsoleSession WHERE account_id=?", account.ID) != wantSessions {
			t.Fatal("forget did not invalidate exactly the affected accounts")
		}
	}
	if count(t, s, "SELECT COUNT(*) FROM ConsoleRepositoryAccess WHERE repository_id=?", repo) != 0 || count(t, s, "SELECT COUNT(*) FROM ConsoleRepositoryAccess WHERE repository_id=?", keep) != 2 {
		t.Fatal("forget retained removed grants or removed unrelated grants")
	}
}

func TestConsoleRepairStorageFailuresLeaveGrantsSessionsAndRevisionsUnchanged(t *testing.T) {
	for _, operation := range []string{"merge", "forget", "console-events"} {
		t.Run(operation, func(t *testing.T) {
			s, _, file := testStore(t)
			_, a := ended(t, s, "repair-a")
			_, b := ended(t, s, "repair-b")
			repo, into := a["repository_id"].(string), b["repository_id"].(string)
			account := createTestConsoleAccount(t, s, "Person", "member")
			if operation == "forget" {
				if _, err := s.ReplaceConsoleAccess(t.Context(), account.ID, 1, "member", false, []ConsoleGrant{{repo, "rw"}}); err != nil {
					t.Fatal(err)
				}
				if _, err := s.db.Exec("INSERT INTO ConsoleSession VALUES(zeroblob(32),?,2,0,43200000)", account.ID); err != nil {
					t.Fatal(err)
				}
			}
			invoke := func() error {
				if operation == "console-events" {
					return s.transact(context.Background(), func(tx *transaction) error {
						tx.console = &account
						_, err := tx.consoleEvents()
						return err
					})
				}
				r := Request{RepositoryID: repo}
				if operation == "merge" {
					r = mergeRequest(repo, into, false)
				}
				_, err := s.Call(context.Background(), operation, r)
				return err
			}
			fault := installFaultDriver(t, s, file)
			fault.discover = true
			if err := invoke(); !errors.Is(err, errStorageFault) {
				t.Fatal("discovery did not roll back", err)
			}
			trace := append([]string(nil), fault.trace...)
			fault.discover = false
			before := databaseState(t, s)
			for index := range trace {
				fault.at, fault.count, fault.trace = index+1, 0, nil
				err := invoke()
				fault.at = 0
				if !errors.Is(err, errStorageFault) || databaseState(t, s) != before {
					t.Fatalf("storage boundary %d exposed partial repair/revision state: %v", index+1, err)
				}
			}
			if err := invoke(); err != nil {
				t.Fatal("retry after storage recovery failed", err)
			}
		})
	}
}
