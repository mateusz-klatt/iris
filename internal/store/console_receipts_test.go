package store

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestConsoleReceiptsRemainReadableAfterWriteDowngradeAndUseCurrentScope(t *testing.T) {
	s, _, ao, b, bo := crossRepo(t)
	origin, target := ao["repository_id"].(string), bo["repository_id"].(string)
	author := createTestConsoleAccount(t, s, "Receipt author", "member")
	giveReadGrant(t, s, author, origin, "rw")
	giveReadGrant(t, s, author, target, "rw")
	principal := readConsolePrincipal(t, s, author)
	request := Request{RepositoryID: origin, Key: "receipt-send", To: []string{bo["agent"].(map[string]any)["id"].(string)}, Body: "A named message"}
	sent := accountRead(t, s, principal, "ui_messages", request)
	messageID := sent["message_id"].(string)
	updated, err := s.ReplaceConsoleAccess(context.Background(), author.ID, author.EditRevision, "member", false, []ConsoleGrant{{origin, "ro"}, {target, "ro"}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.CallConsole(context.Background(), principal, "ui_receipts", Request{Key: messageID})
	consoleError(t, err, "unauthorized")
	principal = readConsolePrincipal(t, s, updated)
	_, err = s.CallConsole(context.Background(), principal, "ui_messages", request)
	consoleError(t, err, "not_found")
	status := accountRead(t, s, principal, "ui_receipts", Request{Key: messageID})
	recipients := status["recipients"].([]map[string]any)
	if status["sender_account_id"] != author.ID || status["sender_username"] != author.Username || status["sender_display_name"] != author.DisplayName || status["reply_address"] != "human:"+origin || len(recipients) != 1 || recipients[0]["recipient"] != agentName(bo) || recipients[0]["repository_id"] != target || recipients[0]["state"] != "pending" || recipients[0]["at"] != nil {
		t.Fatal("downgraded reader lost accepted receipt facts", status)
	}
	inbox := call(t, s, "inbox", b)["deliveries"].([]map[string]any)
	acknowledgement := b
	acknowledgement.Key, acknowledgement.Attempt, acknowledgement.Handled = inbox[0]["id"].(string), number(inbox[0]["attempt"]), true
	call(t, s, "ack", acknowledgement)
	handled := accountRead(t, s, principal, "ui_receipts", Request{Key: messageID})["recipients"].([]map[string]any)[0]
	if handled["id"] != inbox[0]["id"] || handled["state"] != "handled" || handled["at"] == nil {
		t.Fatal("receipt did not describe actual recipient ACK and handled state", handled)
	}
	// Another reader shares the same transport state; there is no personal inbox.
	reader := createTestConsoleAccount(t, s, "Other receipt reader", "member")
	giveReadGrant(t, s, reader, origin, "ro")
	otherPrincipal := readConsolePrincipal(t, s, reader)
	scoped := accountRead(t, s, otherPrincipal, "ui_receipts", Request{Key: messageID})
	if scoped["sender_account_id"] != author.ID || len(scoped["recipients"].([]map[string]any)) != 0 {
		t.Fatal("receipt exposed outside destination state", scoped)
	}
	raw, marshalErr := json.Marshal(scoped)
	if marshalErr != nil || strings.Contains(string(raw), target) || strings.Contains(string(raw), agentName(bo)) {
		t.Fatal("receipt leaked destination metadata", marshalErr)
	}
	updated, err = s.ReplaceConsoleAccess(context.Background(), author.ID, updated.EditRevision, "member", false, nil)
	if err != nil {
		t.Fatal(err)
	}
	principal = readConsolePrincipal(t, s, updated)
	for _, id := range []string{messageID, "guessed-message"} {
		_, err = s.CallConsole(context.Background(), principal, "ui_receipts", Request{Key: id})
		consoleError(t, err, "not_found")
	}
}

func TestConsoleReceiptsRedactIncomingOriginAndKeepAgentMessagesUnattributed(t *testing.T) {
	s, _, ao, b, bo := crossRepo(t)
	origin, target := ao["repository_id"].(string), bo["repository_id"].(string)
	author := createTestConsoleAccount(t, s, "Outside receipt author", "operator")
	accepted := acceptedConsoleSend(t, s, author, "outside", consoleMessageRequest{RepositoryID: origin, To: []string{bo["agent"].(map[string]any)["id"].(string)}, Body: "Cross-repository envelope"})
	reader := createTestConsoleAccount(t, s, "Incoming receipt reader", "member")
	giveReadGrant(t, s, reader, target, "ro")
	principal := readConsolePrincipal(t, s, reader)
	status := accountRead(t, s, principal, "ui_receipts", Request{Key: accepted.MessageID, RepositoryID: target})
	if status["sender_account_id"] != author.ID || status["repository_id"] != nil || status["reply_address"] != nil || len(status["recipients"].([]map[string]any)) != 1 {
		t.Fatal("incoming receipt exposed origin or hid visible facts", status)
	}
	sent := call(t, s, "messages", sendTo(b, "local-message", "Local agent message", agentName(bo)))
	agentStatus := accountRead(t, s, principal, "ui_receipts", Request{Key: sent["message_id"].(string)})
	if agentStatus["sender_account_id"] != nil || agentStatus["reply_address"] != nil || agentStatus["repository_id"] != target {
		t.Fatal("machine receipt asserted a named human author", agentStatus)
	}
}
