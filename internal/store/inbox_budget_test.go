package store

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// An inbox page must fit what a client can read. A page of 100 maximal bodies made only of
// characters that JSON escapes six-fold would be about 10 MiB, ten times the client's 1 MiB
// limit, so pages are bounded by their encoded size too, say whether more mail is waiting,
// and continue once the page is acknowledged (gap review A11).
func TestInboxPagesFitTheirByteBudgetAndContinue(t *testing.T) {
	s, _, _ := testStore(t)
	sender, _ := session(t, s, 1)
	reader, out := session(t, s, 2)
	name := out["agent"].(map[string]any)["name"].(string)
	worst := strings.Repeat("\x01", 16384)
	const sent = 30
	for i := range sent {
		call(t, s, "messages", Request{ExecutionID: sender.ExecutionID, Generation: sender.Generation, Key: fmt.Sprint("k", i), To: []string{name}, Subject: fmt.Sprint("s", i), Body: worst})
	}
	seen := map[string]bool{}
	for pages := 0; ; pages++ {
		if pages > sent {
			t.Fatal("the inbox never drained")
		}
		page := call(t, s, "inbox", Request{ExecutionID: reader.ExecutionID, Limit: 100})
		raw, err := json.Marshal(page)
		if err != nil {
			t.Fatal(err)
		}
		deliveries := page["deliveries"].([]map[string]any)
		if len(raw) > inboxBudget {
			t.Fatalf("page of %d deliveries is %d bytes, over the %d budget", len(deliveries), len(raw), inboxBudget)
		}
		if len(deliveries) == 0 {
			if page["more"] != false {
				t.Fatalf("an empty page says more: %v", page["more"])
			}
			break
		}
		for _, d := range deliveries {
			id := d["id"].(string)
			if seen[id] {
				t.Fatalf("delivery %s came twice", id)
			}
			seen[id] = true
			call(t, s, "ack", Request{ExecutionID: reader.ExecutionID, Generation: reader.Generation, Attempt: d["attempt"].(int64), Key: id})
		}
		if want := len(seen) < sent; page["more"] != want {
			t.Fatalf("after %d of %d, more is %v", len(seen), sent, page["more"])
		}
	}
	if len(seen) != sent {
		t.Fatalf("read %d of %d", len(seen), sent)
	}
}
