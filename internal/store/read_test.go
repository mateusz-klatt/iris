package store

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

// Opening a thread reads the operator's mail in it, so the human inbox's unread count goes
// down (A13). A repository's view reads that repository's human mail only, and reading
// again finds nothing.
func TestReadingAThreadClearsItsHumanUnread(t *testing.T) {
	s, _, irisOut, snap, snapOut := crossRepo(t)
	irisRepo, snapRepo := irisOut["repository_id"].(string), snapOut["repository_id"].(string)
	sent := call(t, s, "messages", sendTo(snap, "both", "x", "human", "human:"+irisRepo))
	thread := sent["thread_id"].(string)
	unread := func() int {
		return call(t, s, "ui_overview", Request{})["human_unread"].(int)
	}
	if unread() != 2 {
		t.Fatal(unread())
	}
	watermark := sent["message_id"].(string)
	if out := call(t, s, "ui_read", Request{Key: thread, RepositoryID: snapRepo, LastMessageID: watermark}); out["read"] != 1 || unread() != 1 {
		t.Fatalf("%v, unread %d", out, unread())
	}
	if out := call(t, s, "ui_read", Request{Key: thread, RepositoryID: snapRepo, LastMessageID: watermark}); out["read"] != 0 {
		t.Fatal(out)
	}
	if out := call(t, s, "ui_read", Request{Key: thread, LastMessageID: watermark}); out["read"] != 1 || unread() != 0 {
		t.Fatalf("whole-hub view: %v, unread %d", out, unread())
	}
	refusedWith(t, s, "ui_read", Request{Key: "ms_none"}, "not_found")
}

func TestReadHighWaterAcknowledgesOnlyLoadedMessages(t *testing.T) {
	s, _, _ := testStore(t)
	sender, _ := session(t, s, 1)
	send := func(key, replyTo string) map[string]any {
		r := sendTo(sender, key, key, "human")
		r.ReplyTo = replyTo
		return call(t, s, "messages", r)
	}
	first := send("first", "")
	second := send("second", first["message_id"].(string))
	third := send("third", second["message_id"].(string))
	thread := first["thread_id"].(string)
	other := send("other", "")

	detail := call(t, s, "ui_thread", Request{Key: thread})
	messages := detail["messages"].([]map[string]any)
	ids := make([]string, len(messages))
	for i, message := range messages {
		ids[i] = message["id"].(string)
	}
	wantOrder := []string{first["message_id"].(string), second["message_id"].(string), third["message_id"].(string)}
	if !slices.Equal(ids, wantOrder) {
		t.Fatalf("thread detail order is %v, want insertion order %v for equal timestamps", ids, wantOrder)
	}

	refusedWith(t, s, "ui_read", Request{Key: thread, LastMessageID: "ms_unknown"}, "not_found")
	refusedWith(t, s, "ui_read", Request{Key: thread, LastMessageID: other["message_id"].(string)}, "not_found")
	read := call(t, s, "ui_read", Request{Key: thread, LastMessageID: second["message_id"].(string)})
	if read["read"] != 2 {
		t.Fatalf("high-water read acknowledged %v deliveries, want the two inserted at or before %s", read, second["message_id"])
	}

	messages = call(t, s, "ui_thread", Request{Key: thread})["messages"].([]map[string]any)
	for i, message := range messages {
		state := message["recipients"].([]map[string]any)[0]["state"]
		want := "acked"
		if i == 2 {
			want = "pending"
		}
		if state != want {
			t.Fatalf("message %s after high-water read is %s, want %s", message["id"], state, want)
		}
	}

	before := call(t, s, "ui_overview", Request{})["human_unread"]
	_, err := s.Call(context.Background(), "ui_read", Request{Key: thread})
	var domain *Error
	if !errors.As(err, &domain) || domain.Code != "bad_request" || !strings.Contains(domain.Message, "reload the console") {
		t.Fatalf("empty watermark: %v", err)
	}
	if unread := call(t, s, "ui_overview", Request{})["human_unread"]; unread != before {
		t.Fatalf("empty watermark changed unread from %v to %v", before, unread)
	}
	if state := call(t, s, "ui_thread", Request{Key: thread})["messages"].([]map[string]any)[2]["recipients"].([]map[string]any)[0]["state"]; state != "pending" {
		t.Fatalf("empty watermark marked the later message %s", state)
	}
}

func TestReadHighWaterExcludesMessagesCommittedAfterDetail(t *testing.T) {
	for _, test := range []struct {
		name   string
		sameMS bool
	}{
		{"same millisecond with a smaller id", true},
		{"earlier timestamp after a clock step back", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, clock, _ := testStore(t)
			sender, registered := session(t, s, 1)
			at := clock.Now().UnixMilli()
			r := sendTo(sender, "watermark", "loaded body", "human")
			r.Subject = "watermark subject"
			sent := call(t, s, "messages", r)
			watermark := sent["message_id"].(string)
			thread := sent["thread_id"].(string)
			detail := call(t, s, "ui_thread", Request{Key: thread})
			loaded := detail["messages"].([]map[string]any)
			if len(loaded) != 1 || loaded[0]["id"] != watermark || detail["high_water_message_id"] != watermark {
				t.Fatalf("detail snapshot contains %v, want only the watermark %s", detail, watermark)
			}

			lateID, lateCreatedMS := "ms_after_clock_step_back", at-1
			if test.sameMS {
				lateID, lateCreatedMS = watermark[:len(watermark)-1], at
			}
			repo := registered["repository_id"].(string)
			if _, err := s.db.Exec(`INSERT INTO Message(id,sender_execution_id,subject,body,created_ms,repository_id,thread_id) VALUES(?,?,?,?,?,?,?)`, lateID, registered["execution_id"], "late subject", "unseen body", lateCreatedMS, repo, thread); err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.Exec(`INSERT INTO Delivery(id,message_id,recipient_agent_id,attention) VALUES(?,?,?,1)`, "dl_late", lateID, "human_"+repo); err != nil {
				t.Fatal(err)
			}
			var watermarkRowID, lateRowID int64
			if err := s.db.QueryRow("SELECT rowid FROM Message WHERE id=?", watermark).Scan(&watermarkRowID); err != nil {
				t.Fatal(err)
			}
			if err := s.db.QueryRow("SELECT rowid FROM Message WHERE id=?", lateID).Scan(&lateRowID); err != nil {
				t.Fatal(err)
			}
			if lateRowID <= watermarkRowID {
				t.Fatalf("late message rowid %d did not follow watermark rowid %d", lateRowID, watermarkRowID)
			}
			reloaded := call(t, s, "ui_thread", Request{Key: thread})
			reloadedMessages := reloaded["messages"].([]map[string]any)
			if reloaded["high_water_message_id"] != lateID {
				t.Fatalf("high water %v followed display order, want inserted message %s", reloaded["high_water_message_id"], lateID)
			}
			if !test.sameMS && (reloadedMessages[0]["id"] != lateID || reloadedMessages[len(reloadedMessages)-1]["id"] != watermark) {
				t.Fatalf("clock step display order is %v, want %s before %s", reloadedMessages, lateID, watermark)
			}

			read := call(t, s, "ui_read", Request{Key: thread, LastMessageID: watermark})
			if read["read"] != 1 {
				t.Fatalf("read acknowledged %v deliveries, want only the loaded watermark", read)
			}
			messages := call(t, s, "ui_thread", Request{Key: thread})["messages"].([]map[string]any)
			states := map[string]any{}
			for _, message := range messages {
				states[message["id"].(string)] = message["recipients"].([]map[string]any)[0]["state"]
			}
			if states[watermark] != "acked" || states[lateID] != "pending" {
				t.Fatalf("states after read are %v, want watermark acked and late message pending", states)
			}
			var lateAcks int
			if err := s.db.QueryRow("SELECT COUNT(*) FROM Ack a JOIN Delivery d ON d.id=a.delivery_id WHERE d.message_id=?", lateID).Scan(&lateAcks); err != nil {
				t.Fatal(err)
			}
			if lateAcks != 0 {
				t.Fatalf("late message has %d acknowledgements, want none", lateAcks)
			}
			summaries := call(t, s, "ui_threads", Request{})["threads"].([]map[string]any)
			if len(messages) != 2 || len(summaries) != 1 || summaries[0]["subject"] != messages[0]["subject"] {
				t.Fatalf("list title %v does not match first displayed message %v", summaries, messages)
			}
		})
	}
}

func TestReadHighWaterRequiresAMessageVisibleInTheRepositoryView(t *testing.T) {
	s, irisAgent, irisOut, sampleAgent, sampleOut := crossRepo(t)
	workOut := call(t, s, "sessions", Request{Client: "claude", Host: "work", SessionID: "w", Cwd: "/w/shop", Worktree: "/w/shop", RemoteURL: "https://github.com/team/shop.git", RootCommit: "shop-root", Source: "hook"})
	workRepo := workOut["repository_id"].(string)
	first := call(t, s, "messages", sendTo(sampleAgent, "first", "not visible from work", agentName(irisOut), "human"))
	reply := sendTo(irisAgent, "second", "visible from work", agentName(workOut))
	reply.ReplyTo = first["message_id"].(string)
	second := call(t, s, "messages", reply)
	thread := first["thread_id"].(string)

	visibleDetail := call(t, s, "ui_thread", Request{Key: thread, RepositoryID: workRepo})
	visible := visibleDetail["messages"].([]map[string]any)
	if len(visible) != 1 || visible[0]["id"] != second["message_id"] || visibleDetail["high_water_message_id"] != second["message_id"] {
		t.Fatalf("work repository can see %v, want only the second message", visibleDetail)
	}
	hidden := sendTo(irisAgent, "later", "hidden from work", agentName(sampleOut))
	hidden.ReplyTo = second["message_id"].(string)
	later := call(t, s, "messages", hidden)
	if later["thread_id"] != thread {
		t.Fatal(later)
	}
	again := call(t, s, "ui_thread", Request{Key: thread, RepositoryID: workRepo})
	if again["high_water_message_id"] != second["message_id"] || len(again["messages"].([]map[string]any)) != 1 {
		t.Fatalf("a hidden later row changed the work high water: %v", again)
	}
	if hub := call(t, s, "ui_thread", Request{Key: thread}); hub["high_water_message_id"] != later["message_id"] {
		t.Fatalf("whole-hub high water is %v, want the newest visible row %s", hub["high_water_message_id"], later["message_id"])
	}
	refusedWith(t, s, "ui_read", Request{Key: thread, RepositoryID: workRepo, LastMessageID: first["message_id"].(string)}, "not_found")
	if unread := call(t, s, "ui_overview", Request{})["human_unread"]; unread != 1 {
		t.Fatalf("refused high-water read changed unread count to %v", unread)
	}
}

func TestEmptyWatermarkAcknowledgesNothing(t *testing.T) {
	s, _, _ := testStore(t)
	sender, _ := session(t, s, 1)
	sent := call(t, s, "messages", sendTo(sender, "unread", "still unread", "human"))
	thread := sent["thread_id"].(string)
	_, err := s.Call(context.Background(), "ui_read", Request{Key: thread})
	var domain *Error
	if !errors.As(err, &domain) || domain.Code != "bad_request" || !strings.Contains(domain.Message, "reload the console") {
		t.Fatalf("empty watermark: %v", err)
	}
	if unread := call(t, s, "ui_overview", Request{})["human_unread"]; unread != 1 {
		t.Fatalf("empty watermark acknowledged mail, unread %v", unread)
	}
	var acks int
	if err = s.db.QueryRow("SELECT COUNT(*) FROM Ack").Scan(&acks); err != nil || acks != 0 {
		t.Fatalf("acknowledgements %d: %v", acks, err)
	}
	if state := call(t, s, "ui_thread", Request{Key: thread})["messages"].([]map[string]any)[0]["recipients"].([]map[string]any)[0]["state"]; state != "pending" {
		t.Fatalf("delivery state %s", state)
	}
}

func TestSnapshotHighWaterAcknowledgesLoadedRowsAndLeavesLaterMail(t *testing.T) {
	s, clock, _ := testStore(t)
	sender, registered := session(t, s, 1)
	at := clock.Now().UnixMilli()
	r := sendTo(sender, "loaded", "first body", "human")
	r.Subject = "loaded subject"
	sent := call(t, s, "messages", r)
	firstID := sent["message_id"].(string)
	thread := sent["thread_id"].(string)
	repo := registered["repository_id"].(string)
	secondID := "ms_clock_step_back"
	if _, err := s.db.Exec(`INSERT INTO Message(id,sender_execution_id,subject,body,created_ms,repository_id,thread_id) VALUES(?,?,?,?,?,?,?)`, secondID, registered["execution_id"], "earlier clock", "shown first", at-1, repo, thread); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO Delivery(id,message_id,recipient_agent_id,attention) VALUES(?,?,?,1)`, "dl_clock", secondID, "human_"+repo); err != nil {
		t.Fatal(err)
	}
	detail := call(t, s, "ui_thread", Request{Key: thread})
	messages := detail["messages"].([]map[string]any)
	if len(messages) != 2 || messages[0]["id"] != secondID || messages[1]["id"] != firstID {
		t.Fatalf("display order %v, want the earlier clock first", messages)
	}
	if detail["high_water_message_id"] != secondID || detail["high_water_message_id"] == messages[len(messages)-1]["id"] {
		t.Fatalf("high water %v followed the last displayed message", detail["high_water_message_id"])
	}
	var firstRow, secondRow int64
	if err := s.db.QueryRow("SELECT rowid FROM Message WHERE id=?", firstID).Scan(&firstRow); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow("SELECT rowid FROM Message WHERE id=?", secondID).Scan(&secondRow); err != nil {
		t.Fatal(err)
	}
	if secondRow <= firstRow {
		t.Fatalf("clock-step rowid %d did not follow %d", secondRow, firstRow)
	}

	thirdID := "ms_after_snapshot"
	if _, err := s.db.Exec(`INSERT INTO Message(id,sender_execution_id,subject,body,created_ms,repository_id,thread_id) VALUES(?,?,?,?,?,?,?)`, thirdID, registered["execution_id"], "after snapshot", "not loaded", at-2, repo, thread); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO Delivery(id,message_id,recipient_agent_id,attention) VALUES(?,?,?,1)`, "dl_after", thirdID, "human_"+repo); err != nil {
		t.Fatal(err)
	}
	read := call(t, s, "ui_read", Request{Key: thread, LastMessageID: detail["high_water_message_id"].(string)})
	if read["read"] != 2 {
		t.Fatalf("snapshot watermark acknowledged %v, want the two loaded messages", read)
	}
	states := map[string]any{}
	for _, message := range call(t, s, "ui_thread", Request{Key: thread})["messages"].([]map[string]any) {
		states[message["id"].(string)] = message["recipients"].([]map[string]any)[0]["state"]
	}
	if states[firstID] != "acked" || states[secondID] != "acked" || states[thirdID] != "pending" {
		t.Fatalf("states %v, want both loaded messages acked and the later insert pending", states)
	}
	var thirdAcks int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM Ack a JOIN Delivery d ON d.id=a.delivery_id WHERE d.message_id=?", thirdID).Scan(&thirdAcks); err != nil || thirdAcks != 0 {
		t.Fatalf("later insert acknowledgements %d: %v", thirdAcks, err)
	}
}
