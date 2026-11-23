package store

import (
	"slices"
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
	if out := call(t, s, "ui_read", Request{Key: thread, RepositoryID: snapRepo}); out["read"] != 1 || unread() != 1 {
		t.Fatalf("%v, unread %d", out, unread())
	}
	if out := call(t, s, "ui_read", Request{Key: thread, RepositoryID: snapRepo}); out["read"] != 0 {
		t.Fatal(out)
	}
	if out := call(t, s, "ui_read", Request{Key: thread}); out["read"] != 1 || unread() != 0 {
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

	legacy := call(t, s, "ui_read", Request{Key: thread})
	if legacy["read"] != 1 {
		t.Fatalf("legacy read acknowledged %v deliveries, want the later message %s", legacy, third["message_id"])
	}
	if unread := call(t, s, "ui_overview", Request{})["human_unread"]; unread != 1 {
		t.Fatalf("legacy read left %v unread deliveries, want only the unrelated thread", unread)
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
			loaded := call(t, s, "ui_thread", Request{Key: thread})["messages"].([]map[string]any)
			if len(loaded) != 1 || loaded[0]["id"] != watermark {
				t.Fatalf("detail snapshot contains %v, want only the watermark", loaded)
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
	s, irisAgent, irisOut, sampleAgent, _ := crossRepo(t)
	workOut := call(t, s, "sessions", Request{Client: "claude", Host: "work", SessionID: "w", Cwd: "/w/shop", Worktree: "/w/shop", RemoteURL: "https://github.com/team/shop.git", RootCommit: "shop-root", Source: "hook"})
	workRepo := workOut["repository_id"].(string)
	first := call(t, s, "messages", sendTo(sampleAgent, "first", "not visible from work", agentName(irisOut), "human"))
	reply := sendTo(irisAgent, "second", "visible from work", agentName(workOut))
	reply.ReplyTo = first["message_id"].(string)
	second := call(t, s, "messages", reply)
	thread := first["thread_id"].(string)

	visible := call(t, s, "ui_thread", Request{Key: thread, RepositoryID: workRepo})["messages"].([]map[string]any)
	if len(visible) != 1 || visible[0]["id"] != second["message_id"] {
		t.Fatalf("work repository can see %v, want only the second message", visible)
	}
	refusedWith(t, s, "ui_read", Request{Key: thread, RepositoryID: workRepo, LastMessageID: first["message_id"].(string)}, "not_found")
	if unread := call(t, s, "ui_overview", Request{})["human_unread"]; unread != 1 {
		t.Fatalf("refused high-water read changed unread count to %v", unread)
	}
}
