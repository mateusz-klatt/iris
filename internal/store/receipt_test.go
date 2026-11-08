package store

import (
	"testing"
	"time"
)

// A fetch that asks for a receipt keeps its mail needing attention until the receipt comes, so
// a response lost on its way leaves the mail recoverable: counted, and woken once (ADR 010).
// A receipt is not an acknowledgement, and the wake respects what happened since.
func TestAFetchReceiptKeepsALostResponseRecoverable(t *testing.T) {
	s, clock, _ := testStore(t)
	sender, _ := session(t, s, 1)
	reader, out := session(t, s, 2)
	name := out["agent"].(map[string]any)["name"].(string)
	send := func(key string) {
		mail := sender
		mail.Key, mail.To, mail.Subject, mail.Body = key, []string{name}, key, "b"
		call(t, s, "messages", mail)
	}
	pending := func() int64 {
		return int64(call(t, s, "events", Request{ExecutionID: reader.ExecutionID})["pending"].(int))
	}
	wakes := func() int {
		n := 0
		for _, e := range call(t, s, "events", Request{ExecutionID: reader.ExecutionID, After: 0})["events"].([]map[string]any) {
			if e["kind"] == "inbox_changed" {
				n++
			}
		}
		return n
	}
	send("lost")
	asked := reader
	asked.Receipt = true
	fetched := call(t, s, "inbox", asked)["deliveries"].([]map[string]any)
	if len(fetched) != 1 || pending() != 1 {
		t.Fatalf("a fetch awaiting its receipt is not counted: %v, pending %d", fetched, pending())
	}
	before := wakes()
	// The response is lost: no receipt. After 60 s the mail is woken, once.
	clock.advance(61 * time.Second)
	call(t, s, "status", Request{})
	call(t, s, "status", Request{})
	if got := wakes(); got != before+1 {
		t.Fatalf("wakes %d after the receipt was due, want %d", got, before+1)
	}
	// The next fetch returns the same delivery, and this time the receipt arrives: quiet, unacked.
	again := call(t, s, "inbox", asked)["deliveries"].([]map[string]any)
	if len(again) != 1 || again[0]["id"] != fetched[0]["id"] {
		t.Fatalf("the next fetch gave %v", again)
	}
	receipt := reader
	receipt.Receipts = []Receipt{{ID: again[0]["id"].(string), Attempt: again[0]["attempt"].(int64)}}
	call(t, s, "inbox_receipt", receipt)
	if pending() != 0 {
		t.Fatalf("a received fetch is still counted: %d", pending())
	}
	if got := call(t, s, "inbox", reader)["deliveries"].([]map[string]any); len(got) != 1 {
		t.Fatalf("the receipt settled the delivery: %v", got)
	}
	// No wake once received; a receipt for the old attempt, or for another Execution, changes
	// nothing and is not an error.
	clock.advance(61 * time.Second)
	call(t, s, "status", Request{})
	if got := wakes(); got != before+1 {
		t.Fatalf("a received fetch woke again: %d", got)
	}
	stale := reader
	stale.Receipts = []Receipt{{ID: fetched[0]["id"].(string), Attempt: fetched[0]["attempt"].(int64)}, {ID: "dl_unknown", Attempt: 1}}
	call(t, s, "inbox_receipt", stale)
}

// The wake fires only for a delivery still unacked and still on the fetch that set it: one acked,
// or fetched again, before its receipt is due gets none from that fetch.
func TestAMissedReceiptWakesOnlyWhatIsStillWaiting(t *testing.T) {
	s, clock, _ := testStore(t)
	sender, _ := session(t, s, 1)
	reader, out := session(t, s, 2)
	name := out["agent"].(map[string]any)["name"].(string)
	for _, key := range []string{"acked", "refetched"} {
		mail := sender
		mail.Key, mail.To, mail.Subject, mail.Body = key, []string{name}, key, "b"
		call(t, s, "messages", mail)
	}
	asked := reader
	asked.Receipt = true
	first := call(t, s, "inbox", asked)["deliveries"].([]map[string]any)
	for _, d := range first {
		if d["subject"] == "acked" {
			call(t, s, "ack", Request{ExecutionID: reader.ExecutionID, Generation: reader.Generation, Attempt: d["attempt"].(int64), Key: d["id"].(string)})
		}
	}
	events := func() int {
		return len(call(t, s, "events", Request{ExecutionID: reader.ExecutionID, After: 0})["events"].([]map[string]any))
	}
	clock.advance(30 * time.Second)
	call(t, s, "inbox", asked) // fetched again: the first fetch's receipt no longer matters
	before := events()
	clock.advance(40 * time.Second) // past the first fetch's due time, not the second's
	call(t, s, "status", Request{})
	if got := events(); got != before {
		t.Fatalf("%d wakes from the first fetch, want none", got-before)
	}
	clock.advance(30 * time.Second) // past the second fetch's due time
	call(t, s, "status", Request{})
	if got := events(); got != before+1 {
		t.Fatalf("%d wakes from the second fetch, want one, for the unacked delivery", got-before)
	}
}

// A fetch without receipt=1, from a client before ADR 010, quiets its mail at once, as before.
func TestAFetchWithoutAReceiptIsAsBefore(t *testing.T) {
	s, clock, _ := testStore(t)
	sender, _ := session(t, s, 1)
	reader, out := session(t, s, 2)
	mail := sender
	mail.Key, mail.To, mail.Subject, mail.Body = "k", []string{out["agent"].(map[string]any)["name"].(string)}, "s", "b"
	call(t, s, "messages", mail)
	call(t, s, "inbox", reader)
	if n := int64(call(t, s, "events", Request{ExecutionID: reader.ExecutionID})["pending"].(int)); n != 0 {
		t.Fatalf("pending %d", n)
	}
	before := len(call(t, s, "events", Request{ExecutionID: reader.ExecutionID, After: 0})["events"].([]map[string]any))
	clock.advance(2 * time.Minute)
	call(t, s, "status", Request{})
	if after := len(call(t, s, "events", Request{ExecutionID: reader.ExecutionID, After: 0})["events"].([]map[string]any)); after != before {
		t.Fatalf("an old fetch was woken: %d events after %d", after, before)
	}
}

// An ack settles a delivery, so it ends the wait for its fetch receipt too: the acked row keeps
// no receipt due time for the sweep to find.
func TestAnAckClearsTheReceiptDueTime(t *testing.T) {
	s, _, _ := testStore(t)
	sender, _ := session(t, s, 1)
	reader, out := session(t, s, 2)
	mail := sender
	mail.Key, mail.To, mail.Subject, mail.Body = "k", []string{out["agent"].(map[string]any)["name"].(string)}, "s", "b"
	call(t, s, "messages", mail)
	asked := reader
	asked.Receipt = true
	d := call(t, s, "inbox", asked)["deliveries"].([]map[string]any)[0]
	due := func() bool {
		var set bool
		if err := s.db.QueryRow("SELECT receipt_due_ms IS NOT NULL FROM Delivery WHERE id=?", d["id"]).Scan(&set); err != nil {
			t.Fatal(err)
		}
		return set
	}
	if !due() {
		t.Fatal("a fetch awaiting its receipt has no due time")
	}
	call(t, s, "ack", Request{ExecutionID: reader.ExecutionID, Generation: reader.Generation, Attempt: d["attempt"].(int64), Key: d["id"].(string)})
	if due() {
		t.Fatal("an acked delivery still waits for its receipt")
	}
}
