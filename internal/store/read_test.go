package store

import "testing"

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
