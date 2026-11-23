package store

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"
)

// freeFor makes a refused, then freed, path for waiter and returns the claim_available cursor.
func freeFor(t *testing.T, s *Store, holder, waiter Request, call1 string, path string) int64 {
	t.Helper()
	call(t, s, "touch", touchAt(holder, call1+"h", path))
	if out := call(t, s, "touch", touchAt(waiter, call1+"w", path)); out["action"] != "deny" {
		t.Fatal(out)
	}
	call(t, s, "release", releaseAll(holder))
	return newestEvent(t, s, "claim_available")
}

func high(t *testing.T, s *Store) int64 {
	t.Helper()
	var n int64
	if err := s.db.QueryRow("SELECT seq FROM sqlite_sequence WHERE name='Event'").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// A first monitor starts just before the newest path freed for its Execution, so the ordinary
// replay announces it. Without one it starts at the newest event, as before.
func TestAFirstMonitorStartsBeforeAPathFreedForIt(t *testing.T) {
	s, _, _ := testStore(t)
	holder, _ := session(t, s, 1)
	waiter, _ := session(t, s, 2)
	bystander, _ := session(t, s, 3)
	freed := freeFor(t, s, holder, waiter, "a", "a.go")
	call(t, s, "touch", touchAt(holder, "later", "b.go"))
	if got := call(t, s, "monitor_cursor", waiter)["cursor"]; got != freed-1 {
		t.Fatalf("waiter starts at %v, freed at %d", got, freed)
	}
	if got := call(t, s, "monitor_cursor", bystander)["cursor"]; got != high(t, s) {
		t.Fatalf("bystander starts at %v, high %d", got, high(t, s))
	}
}

// A path claimed again since it was freed, by anyone and the waiter included, is not a reason to
// rewind, and an older path that is still free is. The comparison is by prefix: a claim inside a
// freed directory counts as taking it.
func TestAFirstMonitorSkipsAPathThatWasTakenAgain(t *testing.T) {
	s, _, _ := testStore(t)
	holder, _ := session(t, s, 1)
	waiter, _ := session(t, s, 2)
	older := freeFor(t, s, holder, waiter, "a", "a.go")
	freeFor(t, s, holder, waiter, "b", "b.go")
	call(t, s, "touch", touchAt(holder, "again", "b.go"))
	if got := call(t, s, "monitor_cursor", waiter)["cursor"]; got != older-1 {
		t.Fatalf("after another session took b.go: %v, want %d", got, older-1)
	}
	call(t, s, "touch", touchAt(waiter, "own", "a.go"))
	if got := call(t, s, "monitor_cursor", waiter)["cursor"]; got != high(t, s) {
		t.Fatalf("after the waiter took a.go itself: %v, want %d", got, high(t, s))
	}

	other, _ := session(t, s, 4)
	call(t, s, "touch", touchAt(holder, "in", "src/x.go"))
	expectError(t, s, "claim", claimRequest(other, "dir", "src", "prefix"), "conflict")
	call(t, s, "release", releaseAll(holder))
	if got := call(t, s, "monitor_cursor", other)["cursor"]; got != newestEvent(t, s, "claim_available")-1 {
		t.Fatalf("the freed directory did not rewind: %v", got)
	}
	call(t, s, "touch", touchAt(waiter, "inside", "src/y.go"))
	if got := call(t, s, "monitor_cursor", other)["cursor"]; got != high(t, s) {
		t.Fatalf("a claim inside the freed directory still rewound: %v", got)
	}
}

// A path freed longer ago than a wait lasts is not replayed: that bounds the replay to the last
// hour of the agent's events.
func TestAFirstMonitorSkipsAPathFreedOverAnHourAgo(t *testing.T) {
	s, clock, _ := testStore(t)
	holder, _ := session(t, s, 1)
	waiter, _ := session(t, s, 2)
	freed := freeFor(t, s, holder, waiter, "a", "a.go")
	// Age the event rather than the clock, which would also expire the waiter's lease.
	age := func(d time.Duration) {
		if _, err := s.db.Exec("UPDATE Event SET created_ms=? WHERE seq=?", clock.Now().Add(-d).UnixMilli(), freed); err != nil {
			t.Fatal(err)
		}
	}
	age(waitLimit)
	if got := call(t, s, "monitor_cursor", waiter)["cursor"]; got != freed-1 {
		t.Fatalf("at exactly the limit: %v", got)
	}
	age(waitLimit + time.Millisecond)
	if got := call(t, s, "monitor_cursor", waiter)["cursor"]; got != high(t, s) {
		t.Fatalf("past the limit: %v", got)
	}
}

// Waiting mail moves the start before its own newest event, so the replay announces it and the
// monitor needs no reset. Mail whose event is older than the bound, or gone, keeps the newest
// event as the start and the reset, as before.
func TestAFirstMonitorReplaysWaitingMailInsteadOfAReset(t *testing.T) {
	s, clock, _ := testStore(t)
	holder, _ := session(t, s, 1)
	waiter, waiterOut := session(t, s, 2)
	call(t, s, "messages", sendTo(holder, "early", "x", agentName(waiterOut)))
	early := newestEvent(t, s, "inbox_changed")
	freed := freeFor(t, s, holder, waiter, "a", "a.go")
	call(t, s, "messages", sendTo(holder, "late", "x", agentName(waiterOut)))
	out := call(t, s, "monitor_cursor", waiter)
	if out["cursor"] != early-1 || out["pending"] != 0 {
		t.Fatalf("mail before the freed path: %v, early %d freed %d", out, early, freed)
	}
	if _, err := s.db.Exec("UPDATE Event SET created_ms=? WHERE seq=?", clock.Now().Add(-waitLimit-time.Millisecond).UnixMilli(), early); err != nil {
		t.Fatal(err)
	}
	if out = call(t, s, "monitor_cursor", waiter); out["cursor"] != high(t, s) || out["pending"] != 2 {
		t.Fatalf("mail older than the bound: %v", out)
	}
	if _, err := s.db.Exec("DELETE FROM Event WHERE seq=?", early); err != nil {
		t.Fatal(err)
	}
	if out = call(t, s, "monitor_cursor", waiter); out["cursor"] != high(t, s) || out["pending"] != 2 {
		t.Fatalf("mail whose event is gone: %v", out)
	}
}

// A first monitor whose freed paths cannot be checked against the claims gets an error, not a
// start that might skip them.
func TestAFirstMonitorFailsWhenTheClaimsCannotBeRead(t *testing.T) {
	s, _, _ := testStore(t)
	holder, _ := session(t, s, 1)
	waiter, _ := session(t, s, 2)
	freeFor(t, s, holder, waiter, "a", "a.go")
	// Every call first expires claims, which does not read origin; reading the claims does.
	if _, err := s.db.Exec("ALTER TABLE Claim RENAME COLUMN origin TO renamed"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Call(context.Background(), "monitor_cursor", waiter); err == nil || !strings.Contains(err.Error(), "origin") {
		t.Fatalf("a monitor started without checking the claims: %v", err)
	}
}

// A superproject Execution waits on a submodule path. The two repositories use different
// policies and both have a live claim of the same path name. Replay follows the repository
// that owned the wait: the superproject's claim does not suppress a free submodule path, an
// exempt automatic overlap in the submodule does not either, and a real claim there does.
// Asking again returns the same cursor. An event with no repository, or one whose repository
// is gone, is not rewound: waking could meet a claim the replay did not see.
func TestFreedRewindFollowsTheRepositoryThatOwnedTheWait(t *testing.T) {
	s, _, _ := testStore(t)
	parent, po := checkoutSession(t, s, "codex", "", "parent", "/src/snapper", "dev:super", superRemote, "snapper-root", "main")
	parentRepo := po["repository_id"].(string)
	holder, ho := checkoutSession(t, s, "codex", "", "holder", "/src/frontend", "dev:fe", frontendRemote, "frontend-root", "main")
	subRepo := ho["repository_id"].(string)
	if parentRepo == subRepo {
		t.Fatal("the superproject and the submodule share a repository")
	}
	setScope(t, s, parentRepo, "repository")
	setScope(t, s, subRepo, "worktree")
	call(t, s, "touch", touchRequest(parent, "super-name", "src/x.ts"))
	call(t, s, "touch", touchRequest(holder, "sub-holds", "src/x.ts"))
	group := submodule("src/x.ts")
	group.WorktreeID = "dev:fe"
	group.Branch = "feature"
	if denied := call(t, s, "touch_files", filesTouch(parent, "wait-in-sub", group)); denied["action"] != "deny" {
		t.Fatal(denied)
	}
	var waitRepo, waitKey string
	if err := s.db.QueryRow("SELECT repository_id,COALESCE(worktree_key,'') FROM Waiter WHERE execution_id=?", parent.ExecutionID).Scan(&waitRepo, &waitKey); err != nil || waitRepo != subRepo || waitKey == "" {
		t.Fatalf("parent waits in %q with key %q, %v", waitRepo, waitKey, err)
	}
	call(t, s, "release", releaseAll(holder))
	freed := newestEvent(t, s, "claim_available")
	var eventRepo sql.NullString
	if err := s.db.QueryRow("SELECT repository_id FROM Event WHERE seq=?", freed).Scan(&eventRepo); err != nil || eventRepo.String != subRepo {
		t.Fatalf("event repository %v, %v", eventRepo, err)
	}
	// Another worktree of the submodule may hold the path. That pair is exempt only
	// because the submodule opted into worktree. The superproject did not, and it still
	// holds the same path name, which would block a replay that read the wrong repository.
	other, _ := checkoutSession(t, s, "codex", "", "other", "/src/frontend-wt", "dev:fe2", frontendRemote, "frontend-root", "side")
	if got := call(t, s, "touch", touchRequest(other, "exempt", "src/x.ts")); got["action"] != "allow" {
		t.Fatal(got)
	}
	cursor := func() any { return call(t, s, "monitor_cursor", parent)["cursor"] }
	if got := cursor(); got != freed-1 {
		t.Fatalf("submodule path did not rewind: %v, freed %d", got, freed)
	}
	if again := cursor(); again != freed-1 {
		t.Fatalf("replay changed the cursor from %d to %v", freed-1, again)
	}
	call(t, s, "claim", claimRequest(other, "blocks", "src/x.ts", "exact"))
	if got := cursor(); got == freed-1 {
		t.Fatal("a claim in the owning repository did not block the rewind")
	}
	call(t, s, "release", releaseAll(other))
	call(t, s, "release", releaseAll(parent))
	if got := cursor(); got != freed-1 {
		t.Fatalf("the freed submodule path stayed blocked after both claims were released: %v", got)
	}
	if _, err := s.db.Exec("UPDATE Event SET repository_id=NULL WHERE seq=?", freed); err != nil {
		t.Fatal(err)
	}
	if got := cursor(); got == freed-1 {
		t.Fatal("a legacy event with no repository rewound")
	}
	if _, err := s.db.Exec("UPDATE Event SET repository_id='rp_gone' WHERE seq=?", freed); err != nil {
		t.Fatal(err)
	}
	if got := cursor(); got == freed-1 {
		t.Fatal("an event whose repository is gone rewound")
	}
}

// A monitor does not start when the owning repository's policy cannot be read. Starting
// would rewind without knowing whether a live claim still blocks the path.
func TestAFirstMonitorFailsWhenTheOwningRepositoryCannotBeRead(t *testing.T) {
	s, _, _ := testStore(t)
	holder, _ := session(t, s, 1)
	waiter, _ := session(t, s, 2)
	freeFor(t, s, holder, waiter, "a", "a.go")
	if _, err := s.db.Exec("ALTER TABLE Repository RENAME COLUMN auto_claim_scope TO renamed"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Call(context.Background(), "monitor_cursor", waiter); err == nil || !strings.Contains(err.Error(), "auto_claim_scope") {
		t.Fatalf("a monitor started without the owning repository's policy: %v", err)
	}
}

// A saved cursor wins: a monitor that has printed past the freed path never hears it again.
func TestASavedMonitorCursorIgnoresFreedPaths(t *testing.T) {
	s, _, _ := testStore(t)
	holder, _ := session(t, s, 1)
	waiter, _ := session(t, s, 2)
	freeFor(t, s, holder, waiter, "a", "a.go")
	r := waiter
	r.ConsumerID = "iris-watch"
	r.After = high(t, s)
	call(t, s, "events", r)
	if got := call(t, s, "monitor_cursor", waiter)["cursor"]; got != high(t, s) {
		t.Fatalf("saved cursor ignored: %v", got)
	}
}
