package store

import (
	"testing"
	"time"
)

// A session that dies holding no claims still changes what the console shows, so its expiry
// moves the event cursor the console refreshes on. Before, only expired claims did, and a
// claimless dead session stayed "active" until unrelated traffic arrived (gap review A16).
func TestAClaimlessSessionsExpiryRefreshesTheConsole(t *testing.T) {
	s, clock, _ := testStore(t)
	session(t, s, 1)
	before := call(t, s, "ui_events", Request{})["cursor"].(int64)
	if again := call(t, s, "ui_events", Request{})["cursor"].(int64); again != before {
		t.Fatalf("an idle poll moved the cursor: %d then %d", before, again)
	}
	clock.advance(time.Hour) // past the lease, with no claims and no other activity
	after := call(t, s, "ui_events", Request{})["cursor"].(int64)
	if after <= before {
		t.Fatalf("expiry left the cursor at %d", after)
	}
	agents := call(t, s, "ui_overview", Request{})["agents"].([]map[string]any)
	if len(agents) != 1 || agents[0]["state"] != "lost" {
		t.Fatalf("%v", agents)
	}
	// Announced once: the next idle poll finds nothing new.
	if again := call(t, s, "ui_events", Request{})["cursor"].(int64); again != after {
		t.Fatalf("the expiry was announced again: %d then %d", after, again)
	}
}

// A session that recovers within its window after a restart changes nothing visible, and one
// that does not recover is announced when its window closes.
func TestARecoveryWindowThatClosesRefreshesTheConsole(t *testing.T) {
	s, clock, file := testStore(t)
	session(t, s, 1)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(file, clock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	before := call(t, reopened, "ui_events", Request{})["cursor"].(int64)
	clock.advance(time.Minute) // inside the 120 s recovery window
	if mid := call(t, reopened, "ui_events", Request{})["cursor"].(int64); mid != before {
		t.Fatalf("a recovering session was announced as gone: %d then %d", before, mid)
	}
	clock.advance(2 * time.Minute)
	if after := call(t, reopened, "ui_events", Request{})["cursor"].(int64); after <= before {
		t.Fatalf("a closed recovery window left the cursor at %d", after)
	}
}
