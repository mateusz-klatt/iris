package store

import (
	"context"
	"testing"
	"time"
)

// Attention counts what the session has not handled, and says whether its monitor beats.
func TestAttentionCountsMailAndFreedPathsAndSeesTheMonitor(t *testing.T) {
	s, clock, _ := testStore(t)
	a, _ := session(t, s, 1)
	b, joinedB := session(t, s, 2)
	read := func() map[string]any {
		t.Helper()
		var out map[string]any
		err := s.transact(context.Background(), func(tx *transaction) error {
			e, err := tx.live(b.ExecutionID, b.Generation)
			if err != nil {
				return err
			}
			out, err = tx.attention(e)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	if got := read(); got["unread"] != 0 || got["available"] != 0 || got["monitored"] != false {
		t.Fatal(got)
	}
	mail := a
	mail.To = []string{joinedB["agent"].(map[string]any)["name"].(string)}
	mail.Body = "hello"
	mail.Key = "m1"
	call(t, s, "messages", mail)
	call(t, s, "claim", claimRequest(a, "held", "f.txt", "exact"))
	expectError(t, s, "claim", claimRequest(b, "want", "f.txt", "exact"), "conflict")
	call(t, s, "release", Request{ExecutionID: a.ExecutionID, Generation: a.Generation, All: true})
	if got := read(); got["unread"] != 1 || got["available"] != 1 || got["monitored"] != false {
		t.Fatal(got)
	}
	if err := s.transact(context.Background(), func(tx *transaction) error { return tx.watchSeen(b.ExecutionID) }); err != nil {
		t.Fatal(err)
	}
	if got := read(); got["monitored"] != true {
		t.Fatal(got)
	}
	clock.advance(watchFresh - time.Second)
	call(t, s, "heartbeat", Request{ExecutionID: b.ExecutionID, Generation: b.Generation, Source: "shim"})
	call(t, s, "heartbeat", Request{ExecutionID: a.ExecutionID, Generation: a.Generation, Source: "shim"})
	clock.advance(time.Second)
	if got := read(); got["monitored"] != false {
		t.Fatalf("a stale watch still counts as a monitor: %v", got)
	}
}
