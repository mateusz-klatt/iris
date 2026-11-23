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
	ordered := slices.Clone(ids)
	slices.Sort(ordered)
	if !slices.Equal(ids, ordered) {
		t.Fatalf("thread detail order is %v, want canonical id order %v for equal timestamps", ids, ordered)
	}

	refusedWith(t, s, "ui_read", Request{Key: thread, LastMessageID: "ms_unknown"}, "not_found")
	refusedWith(t, s, "ui_read", Request{Key: thread, LastMessageID: other["message_id"].(string)}, "not_found")
	read := call(t, s, "ui_read", Request{Key: thread, LastMessageID: ordered[1]})
	if read["read"] != 2 {
		t.Fatalf("high-water read acknowledged %v deliveries, want the two at or before %s", read, ordered[1])
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
