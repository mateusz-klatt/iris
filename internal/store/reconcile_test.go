package store

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestShellReconciliation(t *testing.T) {
	s, _, _ := testStore(t)
	holder, _ := session(t, s, 1)
	shell, _ := session(t, s, 2)
	call(t, s, "claim", claimRequest(holder, "held", "src", "prefix"))
	shell.Kind = "before"
	shell.Status = map[string]string{"old": " M"}
	call(t, s, "reconcile", shell)
	shell.Kind = "after"
	shell.Status = map[string]string{"old": " M", "src/new name": "??"}
	if out := call(t, s, "reconcile", shell); out["message"] == "" {
		t.Fatal("missing shell conflict", out)
	}
	if out := call(t, s, "reconcile", shell); out["message"] != "" {
		t.Fatal("unchanged status warned", out)
	}
	// A second Execution in this same worktree makes attribution impossible.
	call(t, s, "sessions", Request{Client: "claude", Host: "host", Cwd: "/repo/w2", Worktree: "/repo/w2", RemoteURL: "https://example.com/team/project.git", RootCommit: "root", SessionID: "shared"})
	shell.Status = map[string]string{"src/other": "??"}
	if out := call(t, s, "reconcile", shell); out["message"] != "" {
		t.Fatal("shared worktree warned", out)
	}
}
func TestShellFirstPostOnlyEstablishesBaseline(t *testing.T) {
	s, _, _ := testStore(t)
	holder, _ := session(t, s, 1)
	shell, _ := session(t, s, 2)
	call(t, s, "claim", claimRequest(holder, "held", "src", "prefix"))
	shell.Kind = "after"
	shell.Status = map[string]string{"src/new": "??"}
	if out := call(t, s, "reconcile", shell); out["message"] != "" {
		t.Fatal("unattributed initial state warned", out)
	}
}

// A write found by reconciliation is claimed for the session, as an edit tool's would be (gap C7).
// A path someone else holds stays theirs and is reported, a path that left the status is not
// claimed, and the session's own automatic claim is renewed rather than duplicated.
func TestReconciliationClaimsWhatTheSessionWrote(t *testing.T) {
	s, clock, _ := testStore(t)
	holder, _ := session(t, s, 1)
	shell, joined := session(t, s, 2)
	repo := Request{RepositoryID: joined["repository_id"].(string)}
	call(t, s, "claim", claimRequest(holder, "held", "theirs.txt", "exact"))
	shell.Kind = "before"
	shell.Status = map[string]string{"committed.txt": " M", "node_modules/x.js": "??"}
	call(t, s, "reconcile", shell)
	shell.Kind = "after"
	shell.Status = map[string]string{"mine.txt": "??", "theirs.txt": " M", "node_modules/x.js": " M"}
	out := call(t, s, "reconcile", shell)
	claimed, _ := out["claimed"].([]string)
	if len(claimed) != 1 || claimed[0] != "mine.txt" || out["message"] == "" {
		t.Fatalf("claimed %v, message %q", out["claimed"], out["message"])
	}
	// Another session is now refused mine.txt, which nobody had claimed before.
	other, _ := session(t, s, 3)
	if got := call(t, s, "touch", touchRequest(other, "t", "mine.txt")); got["action"] != "deny" {
		t.Fatalf("mine.txt not protected: %v", got)
	}
	// A later write to the same path renews the automatic claim instead of adding one.
	expiry := func() (string, int) {
		t.Helper()
		var at string
		n := 0
		for _, c := range call(t, s, "claims", repo)["claims"].([]map[string]any) {
			if c["path"] == "mine.txt" {
				at, n = c["expires_at"].(string), n+1
			}
		}
		return at, n
	}
	first, _ := expiry()
	clock.advance(30 * time.Second)
	for _, r := range []Request{holder, shell, other} {
		r.Source = "shim"
		call(t, s, "heartbeat", r)
	}
	shell.Kind = "before"
	call(t, s, "reconcile", shell)
	shell.Kind = "after"
	shell.Status = map[string]string{"mine.txt": " M", "theirs.txt": " M", "node_modules/x.js": " M"}
	out = call(t, s, "reconcile", shell)
	if c, _ := out["claimed"].([]string); len(c) != 1 || c[0] != "mine.txt" {
		t.Fatalf("renewal: %v", out["claimed"])
	}
	if second, n := expiry(); n != 1 || second <= first {
		t.Fatalf("%d claims on mine.txt, expiry %s then %s", n, first, second)
	}
}

func TestReconciliationRespectsTheClaimLimit(t *testing.T) {
	s, _, _ := testStore(t)
	holder, _ := session(t, s, 1)
	shell, joined := session(t, s, 2)
	repo := joined["repository_id"].(string)
	call(t, s, "claim", claimRequest(holder, "held", "held.txt", "exact"))

	// Seed an existing auto claim that stays (renewed) and one that leaves (released),
	// so the test proves the reserve skip keeps the rest of reconcile atomic.
	call(t, s, "touch", touchRequest(shell, "seed", "kept.txt", "leaving.txt"))
	shell.Kind = "before"
	shell.Status = map[string]string{"kept.txt": " M", "leaving.txt": " M"}
	call(t, s, "reconcile", shell)

	shell.Kind = "after"
	shell.Status = map[string]string{"kept.txt": "M ", "held.txt": " M"}
	// kept.txt already uses 1 slot and leaving.txt frees 1 slot, so 399 of 405 fresh paths fit
	// before reaching reconcileMaxClaims (400), leaving 6 skipped.
	for i := range reconcileMaxClaims + 5 {
		shell.Status[fmt.Sprintf("f%03d", i)] = "??"
	}
	out := call(t, s, "reconcile", shell)
	claimed, _ := out["claimed"].([]string)
	if len(claimed) != reconcileMaxClaims || out["skipped"] != 6 || out["skip_reason"] != skipReasonClaimReserve {
		t.Fatalf("claimed=%d skipped=%v reason=%v msg=%q", len(claimed), out["skipped"], out["skip_reason"], out["message"])
	}
	msg := out["message"].(string)
	if !strings.Contains(msg, "left 6 unclaimed") || !strings.Contains(msg, "held.txt") {
		t.Fatalf("message missing skip count or conflict warning: %q", msg)
	}
	if _, stillHeld := liveAuto(t, s, shell.ExecutionID, "leaving.txt"); stillHeld {
		t.Fatal("leaving.txt auto claim was not released atomically")
	}

	// Headroom up to maxClaims (500) remains available for pre-edit touches.
	if got := call(t, s, "touch", touchRequest(shell, "post-shell-edit", "edit-after.go")); got["action"] != "allow" {
		t.Fatalf("pre-edit touch failed after reconcile reserve: %v", got)
	}

	// Explicit claims can use the remaining headroom all the way up to 500.
	for i := range reconcileReserve - 1 {
		call(t, s, "claim", claimRequest(shell, fmt.Sprintf("exp-%03d", i), fmt.Sprintf("explicit-%03d", i), "exact"))
	}
	// The 500 hard cap still refuses new grants with too_large (claim_limit).
	expectTooLargeLimit(t, s, "claim", claimRequest(shell, "over-claim", "over-claim.go", "exact"), "claim_limit")
	expectTooLargeLimit(t, s, "touch", touchRequest(shell, "over-touch", "over-touch.go"), "claim_limit")

	// Existing claims can still be touched, and a further shell reconcile skips new paths atomically.
	if got := call(t, s, "touch", touchRequest(shell, "reuse-edit", "edit-after.go")); got["action"] != "allow" {
		t.Fatalf("reusing held claim at 500 failed: %v", got)
	}
	shell.Kind = "before"
	call(t, s, "reconcile", shell)
	shell.Kind = "after"
	shell.Status["new-at-cap-a.go"] = "??"
	shell.Status["new-at-cap-b.go"] = "??"
	atCap := call(t, s, "reconcile", shell)
	if atCap["skipped"] != 2 || atCap["skip_reason"] != skipReasonClaimReserve {
		t.Fatalf("atCap skipped=%v reason=%v", atCap["skipped"], atCap["skip_reason"])
	}
	rows := call(t, s, "claims", Request{RepositoryID: repo})["claims"].([]map[string]any)
	// holder holds 1 explicit claim; shell holds maxClaims (500).
	if len(rows) != maxClaims+1 {
		t.Fatalf("claims count = %d, want %d", len(rows), maxClaims+1)
	}
}

func TestScopedReconciliationRespectsReserve(t *testing.T) {
	s, _, _ := testStore(t)
	shell, _ := session(t, s, 1)
	call(t, s, "reconcile_scoped", Request{
		ExecutionID: shell.ExecutionID, Generation: shell.Generation,
		Kind: "before", Status: map[string]string{}, Scope: &ShellScope{Mode: "dirty"},
	})
	status := map[string]string{}
	for i := range reconcileMaxClaims + 10 {
		status[fmt.Sprintf("s%03d", i)] = "??"
	}
	out := call(t, s, "reconcile_scoped", Request{
		ExecutionID: shell.ExecutionID, Generation: shell.Generation,
		Kind: "after", Status: status, Scope: &ShellScope{Mode: "dirty"},
	})
	claimed, _ := out["claimed"].([]string)
	if len(claimed) != reconcileMaxClaims || out["skipped"] != 10 || out["skip_reason"] != skipReasonClaimReserve {
		t.Fatalf("scoped claimed=%d skipped=%v reason=%v", len(claimed), out["skipped"], out["skip_reason"])
	}
}

// A path that left the status was committed, stashed or restored by git in this worktree only.
// Nothing changed in the holder's worktree, so it is no shell edit to warn about; only paths
// still changed after the command are checked.
func TestReconciliationWarnsOnlyAboutPathsStillChanged(t *testing.T) {
	s, _, _ := testStore(t)
	holder, _ := session(t, s, 1)
	shell, _ := session(t, s, 2)
	call(t, s, "claim", claimRequest(holder, "held", "theirs.txt", "exact"))
	shell.Kind = "before"
	shell.Status = map[string]string{"theirs.txt": " M"}
	call(t, s, "reconcile", shell)
	shell.Kind = "after"
	shell.Status = map[string]string{}
	if out := call(t, s, "reconcile", shell); out["message"] != "" || len(out["conflicts"].([]map[string]any)) != 0 {
		t.Fatalf("a path git restored warned: %v", out)
	}
}

// An auto claim means "this session has uncommitted changes here" (D10). When a shell command
// takes a path out of the status, by commit or by stash, the session's own auto claim on it is
// released and whoever waited for the path is woken. An explicit claim stays, and so does any
// claim of another session.
func TestAPathThatLeavesTheStatusReleasesItsAutoClaim(t *testing.T) {
	s, _, _ := testStore(t)
	shell, joined := session(t, s, 1)
	other, _ := session(t, s, 2)
	repo := Request{RepositoryID: joined["repository_id"].(string)}
	call(t, s, "claim", claimRequest(shell, "explicit", "explicit.txt", "exact"))
	call(t, s, "claim", claimRequest(other, "theirs", "theirs.txt", "exact"))
	shell.Kind = "before"
	shell.Status = map[string]string{}
	call(t, s, "reconcile", shell)
	shell.Kind = "after"
	shell.Status = map[string]string{"committed.txt": "??", "stashed.txt": " M", "explicit.txt": " M", "theirs.txt": " M"}
	call(t, s, "reconcile", shell)
	// The other session waits for committed.txt, which the shell session now holds.
	if got := call(t, s, "touch", touchRequest(other, "wait", "committed.txt")); got["action"] != "deny" {
		t.Fatalf("committed.txt was not claimed: %v", got)
	}
	held := func() map[string]string {
		by := map[string]string{}
		for _, c := range call(t, s, "claims", repo)["claims"].([]map[string]any) {
			by[c["path"].(string)] = c["origin"].(string)
		}
		return by
	}
	if h := held(); h["committed.txt"] != "auto" || h["stashed.txt"] != "auto" {
		t.Fatalf("before the commit: %v", h)
	}
	available := func() int {
		n := 0
		for _, e := range call(t, s, "events", Request{ExecutionID: other.ExecutionID, After: 0})["events"].([]map[string]any) {
			if e["kind"] == "claim_available" {
				n++
			}
		}
		return n
	}
	before := available()
	// git commit and git stash: every path leaves the status.
	shell.Kind = "before"
	call(t, s, "reconcile", shell)
	shell.Kind = "after"
	shell.Status = map[string]string{}
	call(t, s, "reconcile", shell)
	h := held()
	if _, ok := h["committed.txt"]; ok {
		t.Fatalf("a committed path kept its auto claim: %v", h)
	}
	if _, ok := h["stashed.txt"]; ok {
		t.Fatalf("a stashed path kept its auto claim: %v", h)
	}
	if h["explicit.txt"] != "explicit" || h["theirs.txt"] != "explicit" {
		t.Fatalf("an explicit claim or another session's was released: %v", h)
	}
	if available() != before+1 {
		t.Fatal("the session waiting for committed.txt was not woken")
	}
}
