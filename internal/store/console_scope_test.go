package store

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

func readConsolePrincipal(t *testing.T, s *Store, account ConsoleAccount) Principal {
	t.Helper()
	digest := sha256.Sum256([]byte(account.ID))
	now := s.clock.Now().UnixMilli()
	if _, err := s.db.Exec("INSERT OR REPLACE INTO ConsoleSession VALUES(?,?,?,?,?)", digest[:], account.ID, account.AuthVersion, now, now+int64(time.Hour/time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	return Principal{Kind: ConsolePrincipal, SessionDigest: digest[:]}
}

func accountRead(t *testing.T, s *Store, principal Principal, operation string, request Request) map[string]any {
	t.Helper()
	out, err := s.CallConsole(context.Background(), principal, operation, request)
	if err != nil {
		t.Fatal(operation, err)
	}
	return out
}

func giveReadGrant(t *testing.T, s *Store, account ConsoleAccount, repo, mode string) {
	t.Helper()
	if _, err := s.db.Exec("INSERT INTO ConsoleRepositoryAccess VALUES(?,?,?)", account.ID, repo, mode); err != nil {
		t.Fatal(err)
	}
}

func thirdReadRepository(t *testing.T, s *Store, name string) (Request, map[string]any) {
	t.Helper()
	out := call(t, s, "sessions", Request{Client: "codex", Host: "read-host", SessionID: name, Cwd: "/read/" + name, Worktree: "/read/" + name, RemoteURL: "https://example.invalid/" + name + ".git", RootCommit: name, Source: "hook"})
	return Request{ExecutionID: out["execution_id"].(string), Generation: out["generation"].(int64)}, out
}

func TestConsoleMemberQueriesHideOtherMessageEnvelopesBeforeSelection(t *testing.T) {
	s, a, ao, b, bo := crossRepo(t)
	c, co := thirdReadRepository(t, s, "third")
	_, dout := thirdReadRepository(t, s, "fourth")
	account := createTestConsoleAccount(t, s, "Reader", "member")
	giveReadGrant(t, s, account, ao["repository_id"].(string), "ro")
	principal := readConsolePrincipal(t, s, account)
	hiddenRequest := sendTo(b, "hidden-root", "hiddenneedle", agentName(co))
	hiddenRequest.Subject = "Hidden root title"
	hidden := call(t, s, "messages", hiddenRequest)
	visibleRequest := sendTo(c, "visible-reply", "Visible incoming envelope", agentName(ao), agentName(bo))
	visibleRequest.Subject, visibleRequest.ReplyTo = "Visible title", hidden["message_id"].(string)
	visible := call(t, s, "messages", visibleRequest)
	tail := sendTo(c, "hidden-tail", "tailneedle", agentName(dout))
	tail.ReplyTo = visible["message_id"].(string)
	call(t, s, "messages", tail)
	own := sendTo(a, "another-thread", "Own repository message", agentName(ao))
	own.Subject = "Own title"
	another := call(t, s, "messages", own)
	thread := accountRead(t, s, principal, "ui_thread", Request{Key: hidden["thread_id"].(string)})
	messages := thread["messages"].([]map[string]any)
	if len(messages) != 1 || messages[0]["id"] != visible["message_id"] || messages[0]["sender"] != agentName(co) || messages[0]["repository_id"] != nil {
		t.Fatal("cross-repository envelope widened visible metadata", thread)
	}
	recipients := messages[0]["recipients"].([]map[string]any)
	if len(recipients) != 1 || recipients[0]["name"] != agentName(ao) {
		t.Fatal("outside recipients leaked", recipients)
	}
	for _, request := range []Request{{}, {RepositoryID: ao["repository_id"].(string)}, {Q: "Visible"}, {Agent: agentName(ao)}, {Before: "9999999999999"}} {
		threads := accountRead(t, s, principal, "ui_threads", request)["threads"].([]map[string]any)
		found := false
		for _, th := range threads {
			if th["id"] == hidden["thread_id"] {
				found = true
				names := th["participants"].([]string)
				if th["count"] != int64(1) || th["subject"] != "Visible title" || len(names) != 2 || !slices.Contains(names, agentName(ao)) || !slices.Contains(names, agentName(co)) {
					t.Fatal("hidden messages affected thread summary", th)
				}
			}
		}
		if !found {
			t.Fatal("readable envelope missing", request, threads)
		}
	}
	for _, request := range []Request{{Q: "hiddenneedle"}, {Q: "tailneedle"}} {
		if threads := accountRead(t, s, principal, "ui_threads", request)["threads"].([]map[string]any); len(threads) != 0 {
			t.Fatal("hidden match selected a thread", request, threads)
		}
	}
	for _, request := range []Request{{Agent: agentName(dout)}, {Agent: "guessed-agent"}, {Before: "guessed-thread"}} {
		_, err := s.CallConsole(context.Background(), principal, "ui_threads", request)
		consoleError(t, err, "not_found")
	}
	page := accountRead(t, s, principal, "ui_threads", Request{Limit: 1})
	if len(page["threads"].([]map[string]any)) != 1 || page["next"] == nil {
		t.Fatal("visible pagination missing", page)
	}
	next := accountRead(t, s, principal, "ui_threads", Request{Limit: 1, Before: page["next"].(string)})
	firstID := page["threads"].([]map[string]any)[0]["id"]
	nextThreads := next["threads"].([]map[string]any)
	if len(nextThreads) != 1 || nextThreads[0]["id"] == firstID || next["next"] != nil {
		t.Fatal("pagination included hidden activity", page, next, another)
	}
	overview := accountRead(t, s, principal, "ui_overview", Request{})
	if len(overview["repositories"].([]map[string]any)) != 1 || len(overview["agents"].([]map[string]any)) != 1 || len(overview["claims"].([]map[string]any)) != 0 {
		t.Fatal("overview widened grant scope", overview)
	}
	raw, err := json.Marshal(overview)
	if err != nil || strings.Contains(string(raw), bo["repository_id"].(string)) || strings.Contains(string(raw), co["repository_id"].(string)) {
		t.Fatal("overview leaked hidden repository metadata", err)
	}
}

func TestConsoleMemberReadUnionDefaultsToNoAccessAndTracksBothModes(t *testing.T) {
	s, a, ao, b, bo := crossRepo(t)
	sent := call(t, s, "messages", sendTo(a, "read-scope", "Message to own repo", agentName(ao)))
	hidden := call(t, s, "messages", sendTo(b, "hidden-scope", "Hidden message", agentName(bo)))
	account := createTestConsoleAccount(t, s, "No grants", "member")
	principal := readConsolePrincipal(t, s, account)
	for _, operation := range []string{"ui_threads", "ui_deliveries"} {
		out := accountRead(t, s, principal, operation, Request{})
		field := "threads"
		if operation == "ui_deliveries" {
			field = "messages"
		}
		if len(out[field].([]map[string]any)) != 0 {
			t.Fatal("no grant exposed records", operation, out)
		}
	}
	for _, request := range []Request{{Key: sent["thread_id"].(string)}, {Key: hidden["thread_id"].(string)}, {Key: "guessed-thread"}} {
		_, err := s.CallConsole(context.Background(), principal, "ui_thread", request)
		consoleError(t, err, "not_found")
	}
	for _, mode := range []string{"ro", "rw"} {
		if _, err := s.db.Exec("DELETE FROM ConsoleRepositoryAccess WHERE account_id=?", account.ID); err != nil {
			t.Fatal(err)
		}
		giveReadGrant(t, s, account, ao["repository_id"].(string), mode)
		_, hiddenCursorErr := s.CallConsole(context.Background(), principal, "ui_threads", Request{Before: hidden["thread_id"].(string)})
		consoleError(t, hiddenCursorErr, "not_found")
		if got := accountRead(t, s, principal, "ui_threads", Request{})["threads"].([]map[string]any); len(got) != 1 || got[0]["id"] != sent["thread_id"] {
			t.Fatal("mode did not grant scoped reads", mode, got)
		}
		_, err := s.CallConsole(context.Background(), principal, "ui_threads", Request{RepositoryID: bo["repository_id"].(string)})
		consoleError(t, err, "not_found")
	}
	giveReadGrant(t, s, account, bo["repository_id"].(string), "ro")
	if got := accountRead(t, s, principal, "ui_threads", Request{})["threads"].([]map[string]any); len(got) != 2 {
		t.Fatal("missing filter did not mean allowed union", got)
	}
}

func TestConsoleAuthorshipAppearsInScopedThreadsParticipantsAndDeliveryViews(t *testing.T) {
	s, _, ao, b, bo := crossRepo(t)
	author := createTestConsoleAccount(t, s, "Author", "operator")
	reader := createTestConsoleAccount(t, s, "Reader", "member")
	origin, target := ao["repository_id"].(string), bo["repository_id"].(string)
	giveReadGrant(t, s, reader, target, "ro")
	principal := readConsolePrincipal(t, s, reader)
	accepted := acceptedConsoleSend(t, s, author, "attributed", consoleMessageRequest{RepositoryID: origin, To: []string{bo["agent"].(map[string]any)["id"].(string)}, Subject: "Named shared mail", Body: "Shared contents"})
	call(t, s, "end", b)
	detail := accountRead(t, s, principal, "ui_thread", Request{Key: accepted.ThreadID})
	message := detail["messages"].([]map[string]any)[0]
	if message["sender"] != "human" || message["sender_account_id"] != author.ID || message["sender_username"] != author.Username || message["sender_display_name"] != author.DisplayName || message["repository_id"] != nil || message["reply_address"] != nil {
		t.Fatal("author metadata or origin redaction incorrect", message)
	}
	threads := accountRead(t, s, principal, "ui_threads", Request{})["threads"].([]map[string]any)
	authors := threads[0]["authors"].([]map[string]any)
	if len(authors) != 1 || authors[0]["sender_account_id"] != author.ID {
		t.Fatal("visible participant authorship missing", threads)
	}
	for _, request := range []Request{{}, {RepositoryID: target}} {
		problems := accountRead(t, s, principal, "ui_deliveries", request)["messages"].([]map[string]any)
		if len(problems) != 1 || problems[0]["sender_account_id"] != author.ID || len(problems[0]["recipients"].([]map[string]any)) != 1 {
			t.Fatal("scoped receipt missing author or recipient", problems)
		}
	}
	if _, err := s.UpdateConsoleProfile(context.Background(), author.ID, author.EditRevision, "Changed name", "en", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReplaceConsoleAccess(context.Background(), author.ID, 2, "member", true, nil); err != nil {
		t.Fatal(err)
	}
	after := accountRead(t, s, principal, "ui_thread", Request{Key: accepted.ThreadID})["messages"].([]map[string]any)[0]
	if after["sender_display_name"] != author.DisplayName {
		t.Fatal("profile or access edit rewrote message authorship", after)
	}
	// An author's old messages confer no lasting permission after demotion/regrant.
	refreshed, err := s.GetConsoleAccount(context.Background(), author.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec("UPDATE ConsoleAccount SET disabled=0 WHERE id=?", author.ID); err != nil {
		t.Fatal(err)
	}
	refreshed.Disabled = false
	authorPrincipal := readConsolePrincipal(t, s, refreshed)
	_, err = s.CallConsole(context.Background(), authorPrincipal, "ui_thread", Request{Key: accepted.ThreadID})
	consoleError(t, err, "not_found")
}

func TestConsoleScopedReadFailuresRollBackAndRetry(t *testing.T) {
	for _, operation := range []string{"ui_overview", "ui_threads", "ui_thread", "ui_deliveries", "ui_receipts"} {
		t.Run(operation, func(t *testing.T) {
			s, clock, file := testStore(t)
			_, originOut := session(t, s, 1)
			recipient, recipientOut := thirdReadRepository(t, s, "fault-target")
			target := recipientOut["repository_id"].(string)
			author := createTestConsoleAccount(t, s, "Fault author", "operator")
			reader := createTestConsoleAccount(t, s, "Fault reader", "member")
			giveReadGrant(t, s, reader, target, "ro")
			principal := readConsolePrincipal(t, s, reader)
			request := consoleMessageRequest{RepositoryID: originOut["repository_id"].(string), To: []string{recipientOut["agent"].(map[string]any)["id"].(string)}, Subject: "Named fault message", Body: "Visible envelope"}
			accepted := acceptedConsoleSend(t, s, author, "accepted", request)
			clock.advance(time.Second)
			cursor := acceptedConsoleSend(t, s, author, "cursor", request)
			call(t, s, "end", recipient)
			call(t, s, "claim", claimRequest(Request{ExecutionID: originOut["execution_id"].(string), Generation: originOut["generation"].(int64)}, "hidden-claim", "private", "exact"))
			filter := Request{}
			switch operation {
			case "ui_threads":
				filter = Request{Q: "Named", Agent: agentName(recipientOut), Before: cursor.ThreadID}
			case "ui_thread":
				filter = Request{Key: accepted.ThreadID}
			case "ui_receipts":
				filter = Request{Key: accepted.MessageID}
			}
			invoke := func() error {
				_, err := s.CallConsole(context.Background(), principal, operation, filter)
				return err
			}
			fault := installFaultDriver(t, s, file)
			fault.discover = true
			if err := invoke(); !errors.Is(err, errStorageFault) {
				t.Fatal("discovery did not refuse read commit", err)
			}
			trace := append([]string(nil), fault.trace...)
			fault.discover = false
			before := databaseState(t, s)
			for i := range trace {
				fault.count, fault.at, fault.trace = 0, i+1, nil
				err := invoke()
				fault.at = 0
				if !errors.Is(err, errStorageFault) {
					t.Fatalf("boundary %d did not preserve storage error: %v", i+1, err)
				}
				if databaseState(t, s) != before {
					t.Fatalf("boundary %d committed partial read state", i+1)
				}
			}
			if err := invoke(); err != nil {
				t.Fatal("read retry failed", err)
			}
		})
	}
}

func TestConsoleMemberUnreadClaimsAndReadActionRemainSharedButScoped(t *testing.T) {
	s, a, ao, b, bo := crossRepo(t)
	origin, target := ao["repository_id"].(string), bo["repository_id"].(string)
	reader := createTestConsoleAccount(t, s, "Scoped human reader", "member")
	operator := createTestConsoleAccount(t, s, "Shared inbox operator", "operator")
	giveReadGrant(t, s, reader, origin, "ro")
	principal := readConsolePrincipal(t, s, reader)
	operatorPrincipal := readConsolePrincipal(t, s, operator)
	sent := call(t, s, "messages", sendTo(b, "shared-inboxes", "Visible shared inbox envelope", "human", "human:"+origin))
	call(t, s, "claim", claimRequest(a, "visible-claim", "visible-file", "exact"))
	call(t, s, "claim", claimRequest(b, "hidden-claim", "hidden-file", "exact"))
	overview := accountRead(t, s, principal, "ui_overview", Request{})
	claims := overview["claims"].([]map[string]any)
	if overview["human_unread"] != 1 || len(claims) != 1 || claims[0]["repository_id"] != origin {
		t.Fatal("outside unread or claims leaked", overview)
	}
	threads := accountRead(t, s, principal, "ui_threads", Request{Agent: "human"})["threads"].([]map[string]any)
	if len(threads) != 1 || threads[0]["unread"] != int64(1) {
		t.Fatal("human participant filter counted outside shared mailbox", threads)
	}
	read := accountRead(t, s, operatorPrincipal, "ui_read", Request{Key: sent["thread_id"].(string), RepositoryID: origin})
	if read["read"] != 1 {
		t.Fatal("operator did not acknowledge selected shared mailbox", read)
	}
	if got := accountRead(t, s, principal, "ui_overview", Request{})["human_unread"]; got != 0 {
		t.Fatal("member sees private rather than shared unread state", got)
	}
	if got := call(t, s, "ui_threads", Request{RepositoryID: target})["threads"].([]map[string]any)[0]["unread"]; got != int64(1) {
		t.Fatal("scoped operator read acknowledged another repository", got)
	}
}
