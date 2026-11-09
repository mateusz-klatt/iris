package store

import (
	"context"
	"encoding/json"
	"testing"
)

func assertAuthor(t *testing.T, out map[string]any, account ConsoleAccount, repo string) {
	t.Helper()
	if out["sender_account_id"] != account.ID || out["sender_username"] != account.Username || out["sender_display_name"] != account.DisplayName || out["reply_address"] != "human:"+repo {
		t.Fatal("message projection lost its immutable author or shared reply route")
	}
	for _, field := range []string{"role", "password_hash", "ui_locale", "correspondence_locale", "login_key"} {
		if _, ok := out[field]; ok {
			t.Fatal("message exposed private profile field", field)
		}
	}
}

func TestNamedMessageAuthorsReachAgentProjectionsAndSharedReplies(t *testing.T) {
	s, _, aout, br, bout := crossRepo(t)
	aRepo, bRepo := aout["repository_id"].(string), bout["repository_id"].(string)
	a := operationAccount(t, s, "Author")
	grantNamed(t, s, a, aRepo, "rw")
	grantNamed(t, s, a, bRepo, "rw")
	p := consoleTestPrincipal(t, s, a)
	r := Request{RepositoryID: aRepo, Key: "named", To: []string{bout["agent"].(map[string]any)["id"].(string)}, Subject: "shared person", Body: "untrusted message"}
	sent := consoleCall(t, s, p, "ui_messages", r)
	assertAuthor(t, sent, a, aRepo)
	inbox := call(t, s, "inbox", br)["deliveries"].([]map[string]any)
	if len(inbox) != 1 || inbox[0]["sender"] != "human" {
		t.Fatal("human transport identity changed")
	}
	assertAuthor(t, inbox[0], a, aRepo)
	for _, op := range []string{"history", "thread"} {
		q := br
		if op == "thread" {
			q.Key = sent["thread_id"].(string)
		}
		items := call(t, s, op, q)["messages"].([]map[string]any)
		if len(items) != 1 {
			t.Fatal("missing authored history")
		}
		assertAuthor(t, items[0], a, aRepo)
	}
	for _, op := range []string{"read_body", "read_message_body"} {
		q := br
		q.Key = inbox[0]["id"].(string)
		if op == "read_message_body" {
			q.Key = sent["message_id"].(string)
		}
		assertAuthor(t, call(t, s, op, q), a, aRepo)
		q.After = 1000
		expectError(t, s, op, q, "bad_request")
	}
	// Profile changes, disabling and demotion do not rewrite mail already sent.
	updated, err := s.UpdateConsoleProfile(context.Background(), a.ID, 1, "Changed person", "en", "pl")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ReplaceConsoleAccess(context.Background(), a.ID, updated.EditRevision, "member", true, nil); err != nil {
		t.Fatal(err)
	}
	assertAuthor(t, call(t, s, "inbox", br)["deliveries"].([]map[string]any)[0], a, aRepo)
	// An agent explicitly addresses the original human repository, so a cross-
	// repository reply reaches the shared mailbox rather than a private account.
	reply := sendTo(br, "shared-reply", "agent reply", inbox[0]["reply_address"].(string))
	reply.ReplyTo = sent["message_id"].(string)
	response := call(t, s, "messages", reply)
	if response["thread_id"] != sent["thread_id"] || firstDelivery(response)["repository_id"] != aRepo || firstDelivery(response)["recipient"] != "human" {
		t.Fatal("shared cross-repository reply was misrouted")
	}
	// The reply is the agent's own message, with no borrowed person attribution.
	q := br
	q.Key = response["message_id"].(string)
	body := call(t, s, "read_message_body", q)
	if body["sender_account_id"] != nil || body["sender"] != agentName(bout) {
		t.Fatal("agent reply borrowed person attribution")
	}
}

func TestNamedReplayAfterMergeUsesCurrentGrantsAndNeverReopensRetiredOrigins(t *testing.T) {
	s, _, _ := testStore(t)
	_, ar, _, br := mergeSplit(t, s)
	from, into := br["repository_id"].(string), ar["repository_id"].(string)
	recipient := ar["agent"].(map[string]any)["id"].(string)
	a := operationAccount(t, s, "Repairing")
	grantNamed(t, s, a, from, "rw")
	grantNamed(t, s, a, into, "rw")
	r := Request{RepositoryID: from, Key: "lost-response", To: []string{recipient}, Body: "before repair"}
	accepted := mustNamedSend(t, s, a, r)
	if _, err := s.db.Exec("DELETE FROM ConsoleRepositoryAccess WHERE account_id=?", a.ID); err != nil {
		t.Fatal(err)
	}
	_, err := namedSend(s, a, r)
	consoleError(t, err, "not_found")
	call(t, s, "merge", mergeRequest(from, into, false))
	_, err = namedSend(s, a, r)
	consoleError(t, err, "not_found")
	grantNamed(t, s, a, into, "ro")
	_, err = namedSend(s, a, r)
	consoleError(t, err, "not_found")
	grantNamed(t, s, a, into, "rw")
	replayed := mustNamedSend(t, s, a, r)
	if replayed["message_id"] != accepted["message_id"] || firstDelivery(replayed)["id"] != firstDelivery(accepted)["id"] || replayed["reply_address"] != "human:"+into {
		t.Fatal("repair created new mail or retained retired reply address")
	}
	r.Key = "new-after-repair"
	_, err = namedSend(s, a, r)
	consoleError(t, err, "not_found")
	if rowCount(t, s, "SELECT COUNT(*) FROM Message") != 1 || rowCount(t, s, "SELECT COUNT(*) FROM Delivery") != 1 {
		t.Fatal("retired ID originated new mail")
	}
	if count(t, s, "SELECT COUNT(*) FROM Message WHERE sender_account_id=? AND sender_username=? AND sender_display_name=?", a.ID, a.Username, a.DisplayName) != 1 {
		t.Fatal("merge changed immutable author")
	}
}

func TestNamedDeletionTombstoneCannotBecomeANewSend(t *testing.T) {
	for _, deleted := range []string{"message", "delivery", "recipient"} {
		t.Run(deleted, func(t *testing.T) {
			s, _, _ := testStore(t)
			_, ar := session(t, s, 1)
			repo := ar["repository_id"].(string)
			recipient := ar["agent"].(map[string]any)["id"].(string)
			a := operationAccount(t, s, "Tombstone")
			grantNamed(t, s, a, repo, "rw")
			r := Request{RepositoryID: repo, Key: "lost-response", To: []string{recipient}, Body: "accepted"}
			accepted := mustNamedSend(t, s, a, r)
			if deleted == "recipient" {
				call(t, s, "end", Request{ExecutionID: ar["execution_id"].(string), Generation: ar["generation"].(int64)})
				call(t, s, "forget", Request{Agent: agentName(ar)})
			} else {
				if _, err := s.db.Exec("DELETE FROM Delivery WHERE message_id=?", accepted["message_id"]); err != nil {
					t.Fatal(err)
				}
				if deleted == "message" {
					if _, err := s.db.Exec("DELETE FROM Message WHERE id=?", accepted["message_id"]); err != nil {
						t.Fatal(err)
					}
				}
			}
			before := rowCount(t, s, "SELECT COUNT(*) FROM Message")
			_, err := namedSend(s, a, r)
			consoleError(t, err, "not_found")
			r.Body = "changed request"
			_, err = namedSend(s, a, r)
			consoleError(t, err, "key_reused")
			if rowCount(t, s, "SELECT COUNT(*) FROM Message") != before || count(t, s, "SELECT COUNT(*) FROM ConsoleOperation WHERE tombstone=1") != 1 {
				t.Fatal("deleted accepted key became a fresh send")
			}
		})
	}
}

func TestNamedSendIgnoresForgedAuthorsAndExecutionFields(t *testing.T) {
	s, _, _ := testStore(t)
	_, ar := session(t, s, 1)
	repo := ar["repository_id"].(string)
	a := operationAccount(t, s, "Actual")
	b := operationAccount(t, s, "Forged")
	grantNamed(t, s, a, repo, "rw")
	p := consoleTestPrincipal(t, s, a)
	raw, err := json.Marshal(map[string]any{
		"repository_id": repo, "key": "forged", "to": []string{ar["agent"].(map[string]any)["id"].(string)}, "body": "person-authored",
		"sender_account_id": b.ID, "sender_username": b.Username, "sender_display_name": b.DisplayName,
		"source": "machine", "execution_id": ar["execution_id"], "generation": ar["generation"],
	})
	if err != nil {
		t.Fatal(err)
	}
	var r Request
	if err = json.Unmarshal(raw, &r); err != nil {
		t.Fatal(err)
	}
	out := consoleCall(t, s, p, "ui_messages", r)
	assertAuthor(t, out, a, repo)
	if count(t, s, "SELECT COUNT(*) FROM Message m JOIN Execution e ON e.id=m.sender_execution_id WHERE m.sender_account_id=? AND e.client='human'", a.ID) != 1 {
		t.Fatal("wire author or execution selected a principal")
	}
	// A fresh login keeps the original account's retry namespace.
	p2 := consoleTestPrincipal(t, s, a)
	again := consoleCall(t, s, p2, "ui_messages", r)
	if again["message_id"] != out["message_id"] || rowCount(t, s, "SELECT COUNT(*) FROM Delivery") != 1 {
		t.Fatal("relogin duplicated named mail")
	}
}

func TestNamedRetryChecksMovedRecipientsAndChangedDeliveryDestinations(t *testing.T) {
	s, _, aout, _, bout := crossRepo(t)
	aRepo, bRepo := aout["repository_id"].(string), bout["repository_id"].(string)
	aID := aout["agent"].(map[string]any)["id"].(string)
	bID := bout["agent"].(map[string]any)["id"].(string)
	a := operationAccount(t, s, "Checking current destinations")
	grantNamed(t, s, a, aRepo, "rw")
	r := Request{RepositoryID: aRepo, Key: "changed-delivery", To: []string{aID}, Body: "accepted"}
	out := mustNamedSend(t, s, a, r)
	delivery := firstDelivery(out)["id"]
	// Delivery handover can move an accepted delivery without changing its ID.
	// A saved result must check that current mailbox, as well as saved recipients.
	if _, err := s.db.Exec("UPDATE Delivery SET recipient_agent_id=? WHERE id=?", bID, delivery); err != nil {
		t.Fatal(err)
	}
	_, err := namedSend(s, a, r)
	consoleError(t, err, "not_found")
	grantNamed(t, s, a, bRepo, "ro")
	_, err = namedSend(s, a, r)
	consoleError(t, err, "not_found")
	grantNamed(t, s, a, bRepo, "rw")
	again := mustNamedSend(t, s, a, r)
	if firstDelivery(again)["id"] != delivery || firstDelivery(again)["repository_id"] != bRepo || firstDelivery(again)["recipient"] != agentName(bout) {
		t.Fatal("retry did not return the current accepted delivery")
	}
	// Even with rw, a member may not replay a former nonhuman delivery moved to
	// a shared human mailbox. It can still use a scoped read projection.
	_, err = namedSend(s, createTestConsoleAccount(t, s, "Human creator", "operator"), Request{RepositoryID: bRepo, Key: "create-human", To: []string{"human"}, Body: "shared"})
	if err != nil {
		t.Fatal(err)
	}
	var humanID string
	if err = s.db.QueryRow("SELECT id FROM Agent WHERE repository_id=? AND client='human'", bRepo).Scan(&humanID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec("UPDATE Delivery SET recipient_agent_id=? WHERE id=?", humanID, delivery); err != nil {
		t.Fatal(err)
	}
	_, err = namedSend(s, a, r)
	consoleError(t, err, "not_found")
}
