package store

import (
	"fmt"
	"testing"
)

// Thread pages continue from a cursor that orders as the list does, by last message and then
// thread id. A cursor of the timestamp alone skipped every thread sharing the boundary
// thread's millisecond, so history disappeared from browsing (gap review A15).
func TestThreadPagesContinueThroughIdenticalTimestamps(t *testing.T) {
	s, _, _ := testStore(t)
	sender, _ := session(t, s, 1)
	_, out := session(t, s, 2)
	name := out["agent"].(map[string]any)["name"].(string)
	const threads = 7 // the clock does not move, so every thread has the same last_at
	for i := range threads {
		call(t, s, "messages", Request{ExecutionID: sender.ExecutionID, Generation: sender.Generation, Key: fmt.Sprint("t", i), To: []string{name}, Subject: fmt.Sprint("s", i), Body: "b"})
	}
	// The whole hub's view and one repository's view page alike.
	for _, repo := range []string{"", out["repository_id"].(string)} {
		all := call(t, s, "ui_threads", Request{Limit: 100, RepositoryID: repo})
		if _, more := all["next"]; more || len(all["threads"].([]map[string]any)) != threads {
			t.Fatalf("one page of everything: %v", all)
		}
		var order []string
		before := ""
		for pages := 0; ; pages++ {
			if pages > threads {
				t.Fatal("pages never ended")
			}
			page := call(t, s, "ui_threads", Request{Limit: 3, Before: before, RepositoryID: repo})
			for _, th := range page["threads"].([]map[string]any) {
				order = append(order, th["id"].(string))
			}
			next, more := page["next"].(string)
			if !more {
				break
			}
			before = next
		}
		want := []string{}
		for _, th := range all["threads"].([]map[string]any) {
			want = append(want, th["id"].(string))
		}
		if fmt.Sprint(order) != fmt.Sprint(want) {
			t.Fatalf("repository %q: paged %v, want %v", repo, order, want)
		}
	}
}
