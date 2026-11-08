package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// woken counts the release wakes addressed to one session.
func woken(t *testing.T, s *Store, r Request) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM Event WHERE kind='claim_available' AND execution_id=?", r.ExecutionID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func waiting(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM Waiter").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func touchAt(r Request, call string, paths ...string) Request {
	r.ToolCallID = call
	r.Paths = paths
	return r
}

func releaseAll(r Request) Request {
	r.All = true
	return r
}

func TestAReleaseWithoutAWaiterWakesNobody(t *testing.T) {
	s, _, _ := testStore(t)
	holder, _ := session(t, s, 1)
	other, _ := session(t, s, 2)
	call(t, s, "touch", touchAt(holder, "t1", "a.go"))
	call(t, s, "release", releaseAll(holder))
	if woken(t, s, holder) != 0 || woken(t, s, other) != 0 {
		t.Fatal("a release woke a session that waited for nothing")
	}
	// The release is still an event for everyone, for the console and the claims tool.
	var all int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM Event WHERE kind='claim_released'").Scan(&all); err != nil || all != 2 {
		t.Fatal(all, err)
	}
}

func TestARefusedSessionIsWokenOnceWhenThePathIsFree(t *testing.T) {
	s, _, _ := testStore(t)
	holder, _ := session(t, s, 1)
	waiter, _ := session(t, s, 2)
	bystander, _ := session(t, s, 3)
	call(t, s, "touch", touchAt(holder, "h1", "a.go"))
	call(t, s, "touch", touchAt(holder, "h2", "b.go"))
	if out := call(t, s, "touch", touchAt(waiter, "w1", "a.go")); out["action"] != "deny" {
		t.Fatal(out)
	}
	call(t, s, "release", releaseAll(holder))
	if woken(t, s, waiter) != 1 || woken(t, s, bystander) != 0 || waiting(t, s) != 0 {
		t.Fatalf("waiter %d bystander %d left %d", woken(t, s, waiter), woken(t, s, bystander), waiting(t, s))
	}
	// Cleared once notified: a later release of the same path does not wake it again.
	call(t, s, "touch", touchAt(holder, "h3", "a.go"))
	call(t, s, "release", releaseAll(holder))
	if woken(t, s, waiter) != 1 {
		t.Fatal("woken twice")
	}
}

func TestAWaiterStillBlockedByAnotherHolderKeepsWaiting(t *testing.T) {
	s, _, _ := testStore(t)
	first, _ := session(t, s, 1)
	second, _ := session(t, s, 2)
	waiter, _ := session(t, s, 3)
	// Two holders inside the prefix the waiter was refused.
	call(t, s, "touch", touchAt(first, "f1", "src/a.go"))
	call(t, s, "touch", touchAt(second, "s1", "src/b.go"))
	expectError(t, s, "claim", claimRequest(waiter, "w", "src", "prefix"), "conflict")
	call(t, s, "release", releaseAll(first))
	if woken(t, s, waiter) != 0 {
		t.Fatal("woken while another session still held the path")
	}
	call(t, s, "release", releaseAll(second))
	if woken(t, s, waiter) != 1 {
		t.Fatal("not woken when the last holder released")
	}
}

func TestAPrefixOverlapWakesTheWaiter(t *testing.T) {
	s, _, _ := testStore(t)
	holder, _ := session(t, s, 1)
	waiter, _ := session(t, s, 2)
	// The holder's prefix covers the file the waiter was refused.
	call(t, s, "claim", claimRequest(holder, "p", "src", "prefix"))
	call(t, s, "touch", touchAt(waiter, "w1", "src/deep/a.go"))
	// And the waiter's refused prefix covers a file another session holds.
	other, _ := session(t, s, 3)
	call(t, s, "touch", touchAt(other, "o1", "lib/x.go"))
	_, err := s.Call(context.Background(), "claim", claimRequest(waiter, "lib", "lib", "prefix"))
	var refused *Error
	if !errors.As(err, &refused) || refused.Code != "conflict" {
		t.Fatalf("a claim over a held path was not refused: %v", err)
	}
	if waiting(t, s) != 2 {
		t.Fatalf("the refused claim kept no waiter: %d", waiting(t, s))
	}
	call(t, s, "release", releaseAll(holder))
	call(t, s, "release", releaseAll(other))
	if woken(t, s, waiter) != 2 {
		t.Fatalf("prefix overlaps woke %d times, want 2", woken(t, s, waiter))
	}
}

func TestAnExpiredWaiterIsNotWoken(t *testing.T) {
	s, clock, _ := testStore(t)
	holder, _ := session(t, s, 1)
	waiter, _ := session(t, s, 2)
	// An explicit claim with no expiry bounds the wait at an hour.
	call(t, s, "claim", claimRequest(holder, "p", "a.go", "exact"))
	call(t, s, "touch", touchAt(waiter, "w1", "a.go"))
	// Both sessions stay alive past the hour; only the wait runs out.
	for elapsed := time.Duration(0); elapsed <= waitLimit; elapsed += 30 * time.Second {
		clock.advance(30 * time.Second)
		call(t, s, "heartbeat", Request{ExecutionID: holder.ExecutionID, Generation: holder.Generation, Source: "shim"})
		call(t, s, "heartbeat", Request{ExecutionID: waiter.ExecutionID, Generation: waiter.Generation, Source: "shim"})
	}
	call(t, s, "release", releaseAll(holder))
	if woken(t, s, waiter) != 0 || waiting(t, s) != 0 {
		t.Fatal("an expired wait woke the session")
	}
}

func TestAWaitEndsWithTheBlockingClaimAndOnTakingThePath(t *testing.T) {
	s, clock, _ := testStore(t)
	holder, _ := session(t, s, 1)
	waiter, _ := session(t, s, 2)
	// An auto claim lasts ten minutes, so the wait does too, and its expiry frees the path.
	call(t, s, "touch", touchAt(holder, "h1", "a.go"))
	call(t, s, "touch", touchAt(waiter, "w1", "a.go"))
	var until int64
	if err := s.db.QueryRow("SELECT expires_ms FROM Waiter").Scan(&until); err != nil || until != clock.Now().Add(10*time.Minute).UnixMilli() {
		t.Fatal(until, err)
	}
	// Taking the path some other way clears the wait without a wake.
	call(t, s, "release", releaseAll(holder))
	call(t, s, "touch", touchAt(holder, "h2", "b.go"))
	call(t, s, "touch", touchAt(waiter, "w2", "b.go"))
	call(t, s, "release", releaseAll(holder))
	n := woken(t, s, waiter)
	call(t, s, "touch", touchAt(holder, "h3", "c.go"))
	call(t, s, "touch", touchAt(waiter, "w3", "c.go"))
	call(t, s, "touch", touchAt(waiter, "w4", "d.go"))
	if waiting(t, s) != 1 {
		t.Fatal(waiting(t, s))
	}
	call(t, s, "release", releaseAll(holder))
	call(t, s, "claim", claimRequest(waiter, "c", "c.go", "exact"))
	if woken(t, s, waiter) != n+1 || waiting(t, s) != 0 {
		t.Fatalf("woken %d, want %d; left %d", woken(t, s, waiter), n+1, waiting(t, s))
	}
}

func TestOperatorRevokeAndExpiryWakeTheWaiter(t *testing.T) {
	s, clock, _ := testStore(t)
	holder, _ := session(t, s, 1)
	waiter, _ := session(t, s, 2)
	claim := call(t, s, "claim", claimRequest(holder, "p", "a.go", "exact"))
	call(t, s, "touch", touchAt(waiter, "w1", "a.go"))
	call(t, s, "ui_revoke", Request{Fence: number(claim["fence"]), Key: "ui"})
	if woken(t, s, waiter) != 1 {
		t.Fatal("an operator revoke did not wake the waiter")
	}
	call(t, s, "touch", touchAt(holder, "h1", "b.go"))
	call(t, s, "touch", touchAt(waiter, "w2", "b.go"))
	clock.advance(11 * time.Minute)
	call(t, s, "heartbeat", Request{ExecutionID: waiter.ExecutionID, Generation: waiter.Generation, Source: "shim"})
	if woken(t, s, waiter) != 1 {
		// The auto claim and the wait expire together, so nothing is woken.
		t.Fatal("an expired wait was woken by the claim's own expiry")
	}
}

func TestAnEndedWaiterAndAFamilyHolderDoNotWake(t *testing.T) {
	s, _, _ := testStore(t)
	holder, _ := session(t, s, 1)
	waiter, _ := session(t, s, 2)
	call(t, s, "claim", claimRequest(holder, "p", "a.go", "exact"))
	call(t, s, "touch", touchAt(waiter, "w1", "a.go"))
	call(t, s, "end", waiter)
	call(t, s, "release", releaseAll(holder))
	if woken(t, s, waiter) != 0 {
		t.Fatal("an ended session was woken")
	}
	// A claim held by the waiter's own subagent does not keep the waiter blocked.
	parent := call(t, s, "sessions", Request{Client: "claude", Host: "host", SessionID: "parent", Cwd: "/repo/w9", Worktree: "/repo/w9", RemoteURL: "https://example.com/team/project.git", RootCommit: "root", Source: "hook"})
	child := call(t, s, "sessions", Request{Client: "claude", Host: "host", SessionID: "child", ParentSessionID: "parent", Cwd: "/repo/w9", Worktree: "/repo/w9", RemoteURL: "https://example.com/team/project.git", RootCommit: "root", Source: "hook"})
	p := Request{ExecutionID: parent["execution_id"].(string), Generation: parent["generation"].(int64)}
	c := Request{ExecutionID: child["execution_id"].(string), Generation: child["generation"].(int64)}
	call(t, s, "claim", claimRequest(holder, "q", "z.go", "exact"))
	call(t, s, "touch", touchAt(p, "p1", "z.go"))
	call(t, s, "claim", claimRequest(c, "c", "y.go", "exact"))
	// The parent waits on z.go only, and its child's y.go is no obstacle to anything.
	call(t, s, "claim", claimRequest(holder, "r", "y2.go", "exact"))
	call(t, s, "release", releaseAll(holder))
	if woken(t, s, p) != 1 {
		t.Fatal("the parent was not woken")
	}
}
