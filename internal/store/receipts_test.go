package store

import (
	"testing"
	"time"
)

// receiptSetup sends one message from session 1 to session 2's agent and returns the sender,
// the recipient and the message id.
func receiptSetup(t *testing.T, s *Store) (sender, recipient Request, message string) {
	t.Helper()
	sender, _ = session(t, s, 1)
	recipient, ro := session(t, s, 2)
	msg := sender
	msg.Key, msg.Subject, msg.Body = "status", "subject", "untrusted body"
	msg.To = []string{ro["agent"].(map[string]any)["name"].(string)}
	message = call(t, s, "messages", msg)["message_id"].(string)
	return sender, recipient, message
}

func receiptOf(t *testing.T, s *Store, r Request, message string) map[string]any {
	t.Helper()
	r.Key = message
	out := call(t, s, "receipts", r)
	if out["message_id"] != message {
		t.Fatal(out)
	}
	recipients := out["recipients"].([]map[string]any)
	if len(recipients) != 1 {
		t.Fatal(recipients)
	}
	return recipients[0]
}

// Human mail is available through the console without an active Execution,
// including explicit repository addresses and a repository's fallback contact.
func TestReceiptsDoNotFlagHumanRecipientsAsUnreachable(t *testing.T) {
	s, _, target, sender, origin := crossRepo(t)
	for _, tc := range []struct{ address, repository string }{
		{"human", origin["repository_id"].(string)},
		{"human:" + target["repository_id"].(string), target["repository_id"].(string)},
		{"repo:" + target["repository_id"].(string), target["repository_id"].(string)},
	} {
		t.Run(tc.address, func(t *testing.T) {
			sent := call(t, s, "messages", sendTo(sender, tc.address, "waiting for human consent", tc.address))
			got := receiptOf(t, s, sender, sent["message_id"].(string))
			if _, flagged := got["unreachable"]; flagged || got["recipient"] != "human" || got["repository_id"] != tc.repository || got["state"] != "pending" || got["at"] != nil {
				t.Fatal("human mail was reported unreachable before the console read it", got)
			}
			read := call(t, s, "ui_read", Request{Key: sent["thread_id"].(string), RepositoryID: tc.repository})
			if read["read"] != 1 {
				t.Fatal("the console could not read the human delivery immediately", read)
			}
			got = receiptOf(t, s, sender, sent["message_id"].(string))
			if _, flagged := got["unreachable"]; flagged || got["state"] != "acked" || got["at"] == nil {
				t.Fatal("the console read did not settle the human receipt", got)
			}
		})
	}
}

// The sender follows its message through every state, and sees no body.
func TestTheSenderSeesEachRecipientsState(t *testing.T) {
	s, c, _ := testStore(t)
	sender, recipient, message := receiptSetup(t, s)
	got := receiptOf(t, s, sender, message)
	if got["state"] != "pending" || got["at"] != nil || got["repository_id"] == "" || got["recipient"] == "" {
		t.Fatal(got)
	}
	if _, leaked := got["body"]; leaked {
		t.Fatal("a receipt carries the body", got)
	}
	d := call(t, s, "inbox", recipient)["deliveries"].([]map[string]any)[0]
	if got := receiptOf(t, s, sender, message); got["state"] != "inflight" || got["at"] != nil {
		t.Fatal(got)
	}
	c.advance(time.Second)
	ack := recipient
	ack.Key, ack.Attempt = d["id"].(string), number(d["attempt"])
	call(t, s, "ack", ack)
	acked := receiptOf(t, s, sender, message)
	if acked["state"] != "acked" || acked["at"] != "2027-01-15T08:00:01Z" {
		t.Fatal(acked)
	}
	c.advance(time.Minute)
	ack.Handled = true
	call(t, s, "ack", ack)
	handled := receiptOf(t, s, sender, message)
	if handled["state"] != "handled" || handled["at"] != "2027-01-15T08:01:01Z" {
		t.Fatal(handled)
	}
	// A repeat changes nothing, and a plain ack never takes handled back.
	c.advance(time.Minute)
	call(t, s, "ack", ack)
	ack.Handled = false
	call(t, s, "ack", ack)
	if again := receiptOf(t, s, sender, message); again["state"] != "handled" || again["at"] != handled["at"] {
		t.Fatal(again)
	}
}

// Unreachable describes the mailbox now. Losing a session leaves the mail waiting,
// and a replacement session makes it reachable and receives that same delivery.
func TestReceiptsReportUnreachableUntilTheRecipientReturns(t *testing.T) {
	for _, state := range []string{"lost", "ended"} {
		t.Run(state, func(t *testing.T) {
			s, clock, _ := testStore(t)
			sender, recipient, message := receiptSetup(t, s)
			got := receiptOf(t, s, sender, message)
			if _, flagged := got["unreachable"]; flagged {
				t.Fatal("a live recipient was unreachable before fetching", got)
			}
			delivery := call(t, s, "inbox", recipient)["deliveries"].([]map[string]any)[0]
			got = receiptOf(t, s, sender, message)
			if _, flagged := got["unreachable"]; flagged || got["state"] != "inflight" {
				t.Fatal("a live recipient was unreachable after fetching", got)
			}
			if state == "ended" {
				call(t, s, "end", recipient)
			} else {
				recipient.Source = "shim"
				call(t, s, "heartbeat", recipient)
				clock.advance(61 * time.Second)
			}
			if got := receiptOf(t, s, sender, message); got["state"] != "pending" || got["at"] != nil || got["unreachable"] != true {
				t.Fatal("an undelivered receipt hid the unreachable recipient", got)
			}
			out := call(t, s, "sessions", Request{Client: "codex", Host: "host", Cwd: "/repo/w2", Worktree: "/repo/w2", RemoteURL: "https://example.com/team/project.git", RootCommit: "root", SessionID: "returned", Source: "hook"})
			returned := Request{ExecutionID: out["execution_id"].(string), Generation: out["generation"].(int64)}
			got = receiptOf(t, s, sender, message)
			if _, flagged := got["unreachable"]; flagged || got["state"] != "pending" {
				t.Fatal("a returned recipient stayed unreachable", got)
			}
			again := call(t, s, "inbox", returned)["deliveries"].([]map[string]any)
			if len(again) != 1 || again[0]["id"] != delivery["id"] || number(again[0]["attempt"]) != number(delivery["attempt"])+1 {
				t.Fatal("unreachable mail did not survive for the returning recipient", again)
			}
		})
	}
}

// Recovery still counts as active, as it does in delivery problems. The sender sees
// unreachable only if the recipient fails to reattach before its recovery deadline.
func TestReceiptsWaitForRecoveryBeforeReportingUnreachable(t *testing.T) {
	s, clock, file := testStore(t)
	sender, recipient, message := receiptSetup(t, s)
	call(t, s, "inbox", recipient)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s.lock = nil
	reopened, err := Open(file, clock)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	// The sender reattaches, but the recipient remains in its 120-second window.
	sender.Source = "hook"
	call(t, reopened, "heartbeat", sender)
	clock.advance(time.Minute)
	got := receiptOf(t, reopened, sender, message)
	if _, flagged := got["unreachable"]; flagged || got["state"] != "inflight" || got["at"] != nil {
		t.Fatal("a recovering recipient was reported unreachable", got)
	}
	clock.advance(61 * time.Second)
	if got := receiptOf(t, reopened, sender, message); got["state"] != "pending" || got["at"] != nil || got["unreachable"] != true {
		t.Fatal("an expired recovery hid the unreachable recipient", got)
	}
}

func TestReceiptsDoNotFlagSettledDeliveriesWhenTheRecipientEnds(t *testing.T) {
	for _, state := range []string{"acked", "handled"} {
		t.Run(state, func(t *testing.T) {
			s, _, _ := testStore(t)
			sender, recipient, message := receiptSetup(t, s)
			delivery := call(t, s, "inbox", recipient)["deliveries"].([]map[string]any)[0]
			ack := recipient
			ack.Key, ack.Attempt, ack.Handled = delivery["id"].(string), number(delivery["attempt"]), state == "handled"
			call(t, s, "ack", ack)
			before := receiptOf(t, s, sender, message)
			call(t, s, "end", recipient)
			got := receiptOf(t, s, sender, message)
			if _, flagged := got["unreachable"]; flagged || got["state"] != state || got["at"] != before["at"] {
				t.Fatal("an ended recipient changed its settled receipt", got)
			}
		})
	}
}

func TestReceiptReachabilityIsPerRecipientIncludingAMailboxWithoutAnExecution(t *testing.T) {
	s, _, _ := testStore(t)
	sender, _ := session(t, s, 1)
	missing, mo := session(t, s, 2)
	_, live := session(t, s, 3)
	// A mailbox can outlive all its Executions. It must still receive queued mail.
	if _, err := s.db.Exec("DELETE FROM Execution WHERE id=?", missing.ExecutionID); err != nil {
		t.Fatal(err)
	}
	mail := sender
	mail.Key, mail.Body = "mixed-reachability", "untrusted body"
	mail.To = []string{mo["agent"].(map[string]any)["name"].(string), live["agent"].(map[string]any)["name"].(string)}
	message := call(t, s, "messages", mail)["message_id"].(string)
	sender.Key = message
	got := call(t, s, "receipts", sender)["recipients"].([]map[string]any)
	if len(got) != 2 {
		t.Fatal(got)
	}
	for _, recipient := range got {
		want := recipient["recipient"] == mail.To[0]
		value, flagged := recipient["unreachable"]
		if recipient["state"] != "pending" || recipient["at"] != nil || flagged != want || (flagged && value != true) {
			t.Fatal("reachability leaked between recipients", recipient)
		}
	}
}

// One call can accept and mark handled, under the accepting ack's rules.
func TestAHandledAckAlsoAccepts(t *testing.T) {
	s, _, _ := testStore(t)
	sender, recipient, message := receiptSetup(t, s)
	d := call(t, s, "inbox", recipient)["deliveries"].([]map[string]any)[0]
	ack := recipient
	ack.Key, ack.Attempt, ack.Handled = d["id"].(string), number(d["attempt"])+1, true
	expectError(t, s, "ack", ack, "conflict")
	if got := receiptOf(t, s, sender, message); got["state"] != "inflight" {
		t.Fatal("a stale handled ack changed the state", got)
	}
	ack.Attempt--
	call(t, s, "ack", ack)
	if got := receiptOf(t, s, sender, message); got["state"] != "handled" {
		t.Fatal(got)
	}
	if len(call(t, s, "inbox", recipient)["deliveries"].([]map[string]any)) != 0 {
		t.Fatal("a handled delivery was fetched again")
	}
}

// Work often ends in a later session of the same agent, which never fetched the delivery.
// It may mark the accepted delivery handled. Another agent may not, and learns nothing.
func TestALaterSessionOfTheRecipientMarksHandled(t *testing.T) {
	s, _, _ := testStore(t)
	sender, recipient, message := receiptSetup(t, s)
	d := call(t, s, "inbox", recipient)["deliveries"].([]map[string]any)[0]
	ack := recipient
	ack.Key, ack.Attempt = d["id"].(string), number(d["attempt"])
	// Not yet accepted: only the Execution it is assigned to can accept it.
	other, _ := session(t, s, 3)
	early := other
	early.Key, early.Handled = ack.Key, true
	expectError(t, s, "ack", early, "conflict")
	call(t, s, "ack", ack)

	expectError(t, s, "ack", early, "not_found")
	call(t, s, "end", recipient)
	out := call(t, s, "sessions", Request{Client: "codex", Host: "host", Cwd: "/repo/w2", Worktree: "/repo/w2", RemoteURL: "https://example.com/team/project.git", RootCommit: "root", SessionID: "8", Source: "hook"})
	later := Request{ExecutionID: out["execution_id"].(string), Generation: out["generation"].(int64)}
	if later.ExecutionID == recipient.ExecutionID {
		t.Fatal("the replacement is the same Execution")
	}
	mark := later
	mark.Key, mark.Handled = ack.Key, true
	call(t, s, "ack", mark)
	if got := receiptOf(t, s, sender, message); got["state"] != "handled" {
		t.Fatal(got)
	}
}

// Only the sender reads a message's status. Anyone else is told it does not exist, the
// same answer as for an id that does not.
func TestOnlyTheSenderReadsTheStatus(t *testing.T) {
	s, _, _ := testStore(t)
	_, recipient, message := receiptSetup(t, s)
	r := recipient
	r.Key = message
	expectError(t, s, "receipts", r, "not_found")
	r.Key = "ms_missing"
	expectError(t, s, "receipts", r, "not_found")
}

// Every Execution of the sending agent may ask, not only the one that sent.
func TestALaterSessionOfTheSenderReadsTheStatus(t *testing.T) {
	s, _, _ := testStore(t)
	sender, _, message := receiptSetup(t, s)
	call(t, s, "end", sender)
	out := call(t, s, "sessions", Request{Client: "codex", Host: "host", Cwd: "/repo/w1", Worktree: "/repo/w1", RemoteURL: "https://example.com/team/project.git", RootCommit: "root", SessionID: "9", Source: "hook"})
	later := Request{ExecutionID: out["execution_id"].(string), Generation: out["generation"].(int64)}
	if later.ExecutionID == sender.ExecutionID {
		t.Fatal("the later session is the same Execution")
	}
	if got := receiptOf(t, s, later, message); got["state"] != "pending" {
		t.Fatal(got)
	}
}
