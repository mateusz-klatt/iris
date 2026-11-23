package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

const scopeRemote = "https://example.com/team/project.git"

type liveClaim struct {
	fence, expires     int64
	exec, path, origin string
	key, wpath, branch sql.NullString
}

func scopeSession(t *testing.T, s *Store, host, id, worktree, wtID, branch string) (Request, map[string]any) {
	t.Helper()
	return checkoutSession(t, s, "codex", host, id, worktree, wtID, scopeRemote, "root", branch)
}

func checkoutSession(t *testing.T, s *Store, client, host, id, worktree, wtID, remote, root, branch string) (Request, map[string]any) {
	t.Helper()
	if host == "" {
		host = "host"
	}
	out := call(t, s, "sessions", Request{Client: client, Host: host, SessionID: id, Cwd: worktree, Worktree: worktree, WorktreeID: wtID, RemoteURL: remote, RootCommit: root, Branch: branch, Source: "hook"})
	return Request{ExecutionID: out["execution_id"].(string), Generation: out["generation"].(int64)}, out
}

func setScope(t *testing.T, s *Store, repo, scope string) {
	t.Helper()
	call(t, s, "policy", Request{RepositoryID: repo, ClaimAutoScope: scope})
}

func wtGroup(worktree, id, branch, path string) ClaimGroup {
	return ClaimGroup{Worktree: worktree, WorktreeID: id, RemoteURL: scopeRemote, RootCommit: "root", Branch: branch, Paths: []string{path}}
}

func liveClaims(t *testing.T, s *Store, repo string) []liveClaim {
	t.Helper()
	rows, err := s.db.Query(`SELECT fence,execution_id,path,origin,worktree_key,worktree_path,branch,COALESCE(expires_ms,0) FROM Claim WHERE repository_id=? AND revoked_ms IS NULL ORDER BY fence`, repo)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []liveClaim
	for rows.Next() {
		var c liveClaim
		if err = rows.Scan(&c.fence, &c.exec, &c.path, &c.origin, &c.key, &c.wpath, &c.branch, &c.expires); err != nil {
			t.Fatal(err)
		}
		out = append(out, c)
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func claimsOn(cs []liveClaim, path string) []liveClaim {
	var out []liveClaim
	for _, c := range cs {
		if c.path == path {
			out = append(out, c)
		}
	}
	return out
}

func executionKey(t *testing.T, s *Store, id string) sql.NullString {
	t.Helper()
	var key sql.NullString
	if err := s.db.QueryRow("SELECT worktree_key FROM Execution WHERE id=?", id).Scan(&key); err != nil {
		t.Fatal(err)
	}
	return key
}

func waiterCount(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM Waiter").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// The default policy stores worktree keys and then ignores them. Reuse, release,
// conflict, check, wait and replay stay path-wide, including when one Execution
// has touched two checkouts.
func TestRepositoryScopeIgnoresWorktreeKeys(t *testing.T) {
	s, clock, _ := testStore(t)
	a, ao := scopeSession(t, s, "", "a", "/repo/wt1", "dev:1", "main")
	repo := ao["repository_id"].(string)
	if executionKey(t, s, a.ExecutionID).String == "" {
		t.Fatal("registration dropped the session root key")
	}
	b, _ := scopeSession(t, s, "", "b", "/repo/wt2", "dev:2", "other")
	got := call(t, s, "touch", touchRequest(a, "a1", "a.go"))
	if _, ok := got["advisories"]; got["action"] != "allow" || ok {
		t.Fatal(got)
	}
	denied := call(t, s, "touch", touchRequest(b, "b1", "a.go"))
	if _, ok := denied["advisories"]; denied["action"] != "deny" || len(denied["granted"].([]string)) != 0 || ok || waiterCount(t, s) != 1 {
		t.Fatalf("different worktrees were exempt under repository: %v waiters=%d", denied, waiterCount(t, s))
	}
	// One Execution's two checkouts are still one claim.
	folded := call(t, s, "touch_files", filesTouch(a, "fold", wtGroup("/repo/wt1", "dev:1", "main", "fold.go"), wtGroup("/repo/wt2", "dev:2", "other", "fold.go")))
	if len(folded["granted"].([]map[string]any)) != 1 || len(claimsOn(liveClaims(t, s, repo), "fold.go")) != 1 {
		t.Fatalf("repository dedup split a path: granted %v claims %v", folded["granted"], liveClaims(t, s, repo))
	}
	// Keys are kept, and a later check still reports a real conflict with an empty advisory list.
	checked := call(t, s, "check", Request{Host: "elsewhere", Cwd: "/repo/wt2", Worktree: "/repo/wt2", WorktreeID: "dev:2", RemoteURL: scopeRemote, RootCommit: "root", Paths: []string{"a.go"}})
	if _, ok := checked["advisories"]; checked["message"] == "" || ok || len(checked["conflicts"].([]map[string]any)) != 1 {
		t.Fatal(checked)
	}
	call(t, s, "release", releaseAll(a))
	if waiterCount(t, s) != 0 || woken(t, s, b) != 1 {
		t.Fatalf("path-wide release did not wake the waiter: waiters=%d wakes=%d", waiterCount(t, s), woken(t, s, b))
	}

	// Build two keyed rows, then return to repository. Reuse renews across keys and
	// a status release drops every matching automatic row.
	setScope(t, s, repo, "worktree")
	call(t, s, "touch", touchRequest(a, "again", "p.go"))
	call(t, s, "touch_files", filesTouch(a, "p2", wtGroup("/repo/wt2", "dev:2", "other", "p.go")))
	setScope(t, s, repo, "repository")
	rows := claimsOn(liveClaims(t, s, repo), "p.go")
	if len(rows) != 2 || !rows[0].key.Valid || !rows[1].key.Valid || rows[0].key.String == rows[1].key.String {
		t.Fatalf("scope change rewrote or dropped keys: %+v", rows)
	}
	clock.advance(time.Minute)
	call(t, s, "touch", touchRequest(a, "renew", "p.go"))
	renewed := claimsOn(liveClaims(t, s, repo), "p.go")
	if len(renewed) != 2 || renewed[0].expires != rows[0].expires || renewed[1].expires <= rows[1].expires {
		t.Fatalf("repository reuse did not renew across keys: before %+v after %+v", rows, renewed)
	}
	a.Kind, a.Status = "before", map[string]string{"p.go": " M"}
	call(t, s, "reconcile", a)
	a.Kind, a.Status = "after", map[string]string{}
	if out := call(t, s, "reconcile", a); len(claimsOn(liveClaims(t, s, repo), "p.go")) != 0 || out["message"] != "" {
		t.Fatalf("repository release left a keyed row: %v %v", out, liveClaims(t, s, repo))
	}
}

func TestWorktreeExemptsOnlyDifferentKeyedAutomaticClaims(t *testing.T) {
	cases := []struct {
		heldOrigin, heldID, incomingID string
		allow                          bool
	}{
		{"auto", "dev:1", "dev:2", true},
		{"auto", "dev:1", "dev:1", false},
		{"auto", "", "dev:2", false},
		{"auto", "dev:1", "", false},
		{"auto", "", "", false},
		{"explicit", "dev:1", "dev:2", false},
	}
	for _, tc := range cases {
		t.Run(tc.heldOrigin+"/"+tc.heldID+"/"+tc.incomingID, func(t *testing.T) {
			s, _, _ := testStore(t)
			heldPath, incomingPath := "/repo/held", "/repo/incoming"
			heldTree, incomingTree := tc.heldID, tc.incomingID
			if tc.heldID == tc.incomingID {
				heldPath, incomingPath = "/repo/same", "/repo/same"
				if tc.heldID == "" {
					heldTree, incomingTree = "", ""
				}
			}
			held, out := scopeSession(t, s, "", "held", heldPath, heldTree, "held-branch")
			repo := out["repository_id"].(string)
			setScope(t, s, repo, "worktree")
			incoming, _ := scopeSession(t, s, "", "incoming", incomingPath, incomingTree, "incoming-branch")
			if tc.heldOrigin == "explicit" {
				call(t, s, "claim", claimRequest(held, "held", "a.go", "exact"))
			} else {
				call(t, s, "touch", touchRequest(held, "held", "a.go"))
			}
			got := call(t, s, "touch", touchRequest(incoming, "incoming", "a.go"))
			if tc.allow {
				advisories, _ := got["advisories"].([]map[string]any)
				if got["action"] != "allow" || len(got["granted"].([]string)) != 1 || len(advisories) != 1 || waiterCount(t, s) != 0 {
					t.Fatalf("exempt pair was refused: %v", got)
				}
				holder := out["agent"].(map[string]any)["name"].(string)
				if advisories[0]["holder"] != holder || advisories[0]["worktree"] != heldPath || advisories[0]["branch"] != "held-branch" || advisories[0]["path"] != "a.go" {
					t.Fatalf("advisory data: %v", advisories[0])
				}
				want := fmt.Sprintf("Iris: a.go is also auto-claimed by %s in worktree %s on branch held-branch. This repository allows overlapping automatic claims across worktrees; this edit is allowed, and the overlap can meet immediately in a shared filesystem tree or surface at rebase or merge.", holder, heldPath)
				if got["message"] != want {
					t.Fatalf("message %q", got["message"])
				}
				if len(claimsOn(liveClaims(t, s, repo), "a.go")) != 2 {
					t.Fatal(liveClaims(t, s, repo))
				}
				return
			}
			if got["action"] != "deny" || len(got["granted"].([]string)) != 0 || got["advisories"] != nil || waiterCount(t, s) != 1 {
				t.Fatalf("non-exempt pair was allowed: %v waiters=%d", got, waiterCount(t, s))
			}
		})
	}
}

func TestSameExecutionReuseFollowsScope(t *testing.T) {
	for _, scope := range []string{"repository", "worktree"} {
		t.Run(scope, func(t *testing.T) {
			s, clock, _ := testStore(t)
			a, ao := scopeSession(t, s, "", "a", "/repo/wt1", "dev:1", "main")
			repo := ao["repository_id"].(string)
			setScope(t, s, repo, "worktree")
			call(t, s, "touch", touchRequest(a, "one", "a.go"))
			call(t, s, "touch_files", filesTouch(a, "two", wtGroup("/repo/wt2", "dev:2", "other", "a.go")))
			setScope(t, s, repo, scope)
			before := claimsOn(liveClaims(t, s, repo), "a.go")
			if len(before) != 2 {
				t.Fatal(before)
			}
			clock.advance(time.Minute)
			call(t, s, "touch", touchRequest(a, "renew", "a.go"))
			after := claimsOn(liveClaims(t, s, repo), "a.go")
			if len(after) != 2 {
				t.Fatalf("%s reuse inserted a claim: %+v", scope, after)
			}
			same, other := after[0], after[1]
			if scope == "worktree" {
				if same.expires <= before[0].expires || other.expires != before[1].expires {
					t.Fatalf("worktree renewed the other checkout: before %+v after %+v", before, after)
				}
			} else if other.expires <= before[1].expires || same.expires != before[0].expires {
				t.Fatalf("repository did not renew across keys: before %+v after %+v", before, after)
			}
		})
	}
}

func TestOwnNullKeyClaimStaysRepositoryWide(t *testing.T) {
	s, _, _ := testStore(t)
	a, ao := scopeSession(t, s, "", "a", "/repo/wt1", "dev:1", "main")
	setScope(t, s, ao["repository_id"].(string), "worktree")
	call(t, s, "touch", touchRequest(a, "first", "a.go"))
	if _, err := s.db.Exec("UPDATE Claim SET worktree_key=NULL WHERE execution_id=?", a.ExecutionID); err != nil {
		t.Fatal(err)
	}
	got := call(t, s, "touch", touchRequest(a, "second", "a.go"))
	rows := claimsOn(liveClaims(t, s, ao["repository_id"].(string)), "a.go")
	if got["action"] != "deny" || waiterCount(t, s) != 0 || len(rows) != 1 || rows[0].key.Valid || rows[0].origin != "auto" {
		t.Fatalf("null-key claim was replaced: %v %+v", got, rows)
	}
}

func TestExplicitClaimFoldsSiblingAutomaticRows(t *testing.T) {
	s, _, _ := testStore(t)
	a, ao := scopeSession(t, s, "", "a", "/repo/wt1", "dev:1", "main")
	repo := ao["repository_id"].(string)
	setScope(t, s, repo, "worktree")
	call(t, s, "touch_files", filesTouch(a, "both", wtGroup("/repo/wt1", "dev:1", "main", "a.go"), wtGroup("/repo/wt2", "dev:2", "other", "a.go")))
	other, _ := scopeSession(t, s, "", "other", "/repo/wt3", "dev:3", "third")
	call(t, s, "touch", touchRequest(other, "other", "a.go"))
	before := liveClaims(t, s, repo)
	expectError(t, s, "claim", claimRequest(a, "upgrade", "a.go", "exact"), "conflict")
	if fmt.Sprint(liveClaims(t, s, repo)) != fmt.Sprint(before) {
		t.Fatalf("refused upgrade changed claims\nbefore %+v\nafter  %+v", before, liveClaims(t, s, repo))
	}

	// The other session is not holding the path the new grant uses. Drop it, then
	// upgrade the wt1 row: the wt2 sibling is revoked in the same transaction.
	call(t, s, "release", releaseAll(other))
	up := call(t, s, "claim", claimRequest(a, "upgrade-ok", "a.go", "exact"))
	rows := liveClaims(t, s, repo)
	if len(rows) != 1 || rows[0].origin != "explicit" || rows[0].key.Valid || rows[0].wpath.String != "/repo/wt1" || rows[0].branch.String != "main" || rows[0].fence != number(up["fence"]) {
		t.Fatalf("upgrade: %+v", rows)
	}
	var revoked int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM Claim WHERE execution_id=? AND origin='auto' AND revoked_ms IS NOT NULL AND path='a.go'", a.ExecutionID).Scan(&revoked); err != nil || revoked != 1 {
		t.Fatal("sibling was not revoked", revoked, err)
	}
	// The same Execution reuses its explicit claim from any checkout. That reuse
	// does not grant the revoked automatic sibling again.
	again := call(t, s, "touch_files", filesTouch(a, "sibling", wtGroup("/repo/wt2", "dev:2", "other", "a.go")))
	rows = liveClaims(t, s, repo)
	var liveAuto int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM Claim WHERE execution_id=? AND origin='auto' AND revoked_ms IS NULL AND path='a.go'", a.ExecutionID).Scan(&liveAuto); err != nil {
		t.Fatal(err)
	}
	if again["action"] != "allow" || liveAuto != 0 || len(rows) != 1 || rows[0].origin != "explicit" || rows[0].key.Valid || rows[0].fence != number(up["fence"]) {
		t.Fatal(again, rows, liveAuto)
	}
}

func TestNewExplicitGrantRevokesAnOtherWorktreeAutomaticRow(t *testing.T) {
	s, _, _ := testStore(t)
	a, ao := scopeSession(t, s, "", "a", "/repo/wt1", "dev:1", "main")
	repo := ao["repository_id"].(string)
	setScope(t, s, repo, "worktree")
	call(t, s, "touch_files", filesTouch(a, "only-wt2", wtGroup("/repo/wt2", "dev:2", "other", "a.go")))
	waiter, _ := scopeSession(t, s, "", "waiter", "/repo/wt3", "dev:3", "wait")
	// A conflicting claim returns an error and still commits the waiter.
	expectError(t, s, "claim", claimRequest(waiter, "blocked", "a.go", "exact"), "conflict")
	if waiterCount(t, s) != 1 {
		t.Fatal("explicit overlap left no waiter")
	}
	grant := call(t, s, "claim", claimRequest(a, "wide", "a.go", "exact"))
	rows := liveClaims(t, s, repo)
	if len(rows) != 1 || rows[0].origin != "explicit" || rows[0].key.Valid || rows[0].wpath.String != "/repo/wt1" || rows[0].fence != number(grant["fence"]) {
		t.Fatalf("new explicit grant: %+v", rows)
	}
	if waiterCount(t, s) != 1 || woken(t, s, waiter) != 0 {
		t.Fatalf("explicit grant woke a waiter the new row still blocks: waiters=%d wakes=%d", waiterCount(t, s), woken(t, s, waiter))
	}
	call(t, s, "release", releaseAll(a))
	if waiterCount(t, s) != 0 || woken(t, s, waiter) != 1 {
		t.Fatal("releasing the explicit claim did not wake the waiter")
	}
}

func TestGroupedRenameGrantsBothEndsOrNeither(t *testing.T) {
	s, _, _ := testStore(t)
	a, ao := scopeSession(t, s, "", "a", "/repo/wt1", "dev:1", "main")
	repo := ao["repository_id"].(string)
	setScope(t, s, repo, "worktree")
	other, _ := scopeSession(t, s, "", "other", "/repo/wt3", "dev:3", "third")
	call(t, s, "touch", touchRequest(other, "end", "new.go"))
	grouped := call(t, s, "touch_files", filesTouch(a, "see-other", wtGroup("/repo/wt1", "dev:1", "main", "new.go")))
	groupedNotes, _ := grouped["advisories"].([]map[string]any)
	if grouped["action"] != "allow" || len(groupedNotes) != 1 || len(grouped["granted"].([]map[string]any)) != 1 {
		t.Fatal(grouped)
	}
	renamed := touchRequest(a, "rename", "old.go")
	renamed.RenameFrom, renamed.RenameTo = "old.go", "new.go"
	got := call(t, s, "touch", renamed)
	if got["action"] != "allow" || len(got["granted"].([]string)) != 2 || got["advisories"] == nil {
		t.Fatal(got)
	}
	call(t, s, "claim", claimRequest(other, "real", "blocked.go", "exact"))
	blocked := touchRequest(a, "rename-blocked")
	blocked.RenameFrom, blocked.RenameTo = "free.go", "blocked.go"
	refused := call(t, s, "touch", blocked)
	if refused["action"] != "deny" || len(claimsOn(liveClaims(t, s, repo), "free.go")) != 0 || len(claimsOn(liveClaims(t, s, repo), "blocked.go")) != 1 {
		t.Fatalf("rename granted one end: %v %+v", refused, liveClaims(t, s, repo))
	}
	split := call(t, s, "touch_files", filesTouch(a, "split", wtGroup("/repo/wt1", "dev:1", "main", "start.go"), wtGroup("/repo/wt2", "dev:2", "other", "blocked.go")))
	if split["action"] != "deny" || len(claimsOn(liveClaims(t, s, repo), "start.go")) != 0 {
		t.Fatal(split, liveClaims(t, s, repo))
	}
}

func TestRepositoryPolicyDoesNotCrossASubmoduleBoundary(t *testing.T) {
	s, _, _ := testStore(t)
	parent, po := checkoutSession(t, s, "codex", "", "parent", "/src/snapper", "dev:super", superRemote, "snapper-root", "main")
	parent2, _ := checkoutSession(t, s, "codex", "", "parent2", "/src/snapper-wt", "dev:super2", superRemote, "snapper-root", "side")
	child, co := checkoutSession(t, s, "codex", "", "child", "/src/frontend", "dev:fe", frontendRemote, "frontend-root", "main")
	child2, _ := checkoutSession(t, s, "codex", "", "child2", "/src/frontend-wt", "dev:fe2", frontendRemote, "frontend-root", "side")
	if po["repository_id"] == co["repository_id"] {
		t.Fatal("superproject and submodule share a repository")
	}
	setScope(t, s, po["repository_id"].(string), "worktree")
	call(t, s, "touch", touchRequest(child, "fe", "src/a.ts"))
	// The superproject opted in. The submodule did not, so its file still conflicts.
	held := call(t, s, "touch_files", filesTouch(parent, "into-fe", submodule("src/a.ts")))
	if held["action"] != "deny" {
		t.Fatal(held)
	}
	if second := call(t, s, "touch", touchRequest(child2, "fe2", "src/a.ts")); second["action"] != "deny" {
		t.Fatal(second)
	}
	setScope(t, s, po["repository_id"].(string), "repository")
	setScope(t, s, co["repository_id"].(string), "worktree")
	call(t, s, "touch", touchRequest(parent, "super", "main.go"))
	if got := call(t, s, "touch", touchRequest(parent2, "super2", "main.go")); got["action"] != "deny" {
		t.Fatal(got)
	}
	call(t, s, "touch", touchRequest(child, "lib", "lib.go"))
	if got := call(t, s, "touch", touchRequest(child2, "lib2", "lib.go")); got["action"] != "allow" || got["advisories"] == nil {
		t.Fatal(got)
	}
}

func TestShellReleaseAndRenewalFollowScope(t *testing.T) {
	for _, scope := range []string{"repository", "worktree"} {
		t.Run(scope, func(t *testing.T) {
			s, _, _ := testStore(t)
			a, ao := scopeSession(t, s, "", "a", "/repo/wt1", "dev:1", "main")
			repo := ao["repository_id"].(string)
			setScope(t, s, repo, "worktree")
			other, _ := scopeSession(t, s, "", "other", "/repo/wt3", "dev:3", "third")
			call(t, s, "touch", touchRequest(a, "mine", "p.go"))
			call(t, s, "touch_files", filesTouch(a, "other-wt", wtGroup("/repo/wt2", "dev:2", "other", "p.go")))
			call(t, s, "claim", claimRequest(a, "explicit", "q.go", "exact"))
			call(t, s, "touch", touchRequest(other, "theirs", "r.go"))
			setScope(t, s, repo, scope)
			// The status checkout is the session root, so renewal reuses that key
			// under worktree and the later fence under repository. Neither scope
			// inserts a third row for a path this Execution already holds.
			a.Kind, a.Status = "before", map[string]string{}
			call(t, s, "reconcile", a)
			a.Kind, a.Status = "after", map[string]string{"p.go": " M"}
			out := call(t, s, "reconcile", a)
			claimed, _ := out["claimed"].([]string)
			rows := claimsOn(liveClaims(t, s, repo), "p.go")
			if len(claimed) != 1 || claimed[0] != "p.go" || len(rows) != 2 {
				t.Fatalf("shell renewal changed the row count: %v %+v", out, rows)
			}
			a.Kind, a.Status = "before", map[string]string{"p.go": " M", "q.go": " M", "r.go": " M"}
			call(t, s, "reconcile", a)
			a.Kind, a.Status = "after", map[string]string{}
			call(t, s, "reconcile", a)
			left := liveClaims(t, s, repo)
			p, q, r := claimsOn(left, "p.go"), claimsOn(left, "q.go"), claimsOn(left, "r.go")
			if len(q) != 1 || q[0].origin != "explicit" || len(r) != 1 || r[0].exec == a.ExecutionID {
				t.Fatalf("release touched an explicit or foreign claim: %+v", left)
			}
			if scope == "worktree" && len(p) != 1 {
				t.Fatalf("worktree release was path-wide: %+v", p)
			}
			if scope == "repository" && len(p) != 0 {
				t.Fatalf("repository release left a row: %+v", p)
			}
		})
	}
}

func TestCheckPathsReportsAnExemptOverlapAsAnAdvisory(t *testing.T) {
	s, _, _ := testStore(t)
	held, ho := scopeSession(t, s, "", "held", "/repo/wt1", "dev:1", "feature")
	repo := ho["repository_id"].(string)
	setScope(t, s, repo, "worktree")
	call(t, s, "touch", touchRequest(held, "held", "src/api.go"))
	bare := scopeSessionOnly(t, s, "bare")
	call(t, s, "touch", touchRequest(bare, "legacy", "legacy.go"))
	exempt := call(t, s, "check", Request{Host: "other", Cwd: "/repo/wt2", Worktree: "/repo/wt2", WorktreeID: "dev:2", RemoteURL: scopeRemote, RootCommit: "root", Paths: []string{"src/api.go"}})
	advisories, _ := exempt["advisories"].([]map[string]any)
	if exempt["message"] != "" || len(advisories) != 1 || len(exempt["conflicts"].([]map[string]any)) != 0 {
		t.Fatal(exempt)
	}
	refused := call(t, s, "check", Request{Host: "other", Cwd: "/repo/wt2", Worktree: "/repo/wt2", RemoteURL: scopeRemote, RootCommit: "root", Paths: []string{"src/api.go"}})
	if refused["message"] == "" || refused["advisories"] != nil {
		t.Fatal("missing key was exempt", refused)
	}
	call(t, s, "claim", claimRequest(held, "wide", "src", "prefix"))
	explicit := call(t, s, "check", Request{Host: "other", Cwd: "/elsewhere", Worktree: "/elsewhere", WorktreeID: "dev:3", RemoteURL: scopeRemote, RootCommit: "root", Paths: []string{"src/file.go"}})
	if explicit["message"] == "" || explicit["advisories"] != nil {
		t.Fatal("explicit prefix was exempt", explicit)
	}
	legacy := call(t, s, "check", Request{Host: "other", Cwd: "/elsewhere", Worktree: "/elsewhere", WorktreeID: "dev:3", RemoteURL: scopeRemote, RootCommit: "root", Paths: []string{"legacy.go"}})
	if legacy["message"] == "" || legacy["advisories"] != nil {
		t.Fatal("null-key claim was exempt", legacy)
	}
}

func scopeSessionOnly(t *testing.T, s *Store, id string) Request {
	t.Helper()
	out := call(t, s, "sessions", Request{Client: "codex", Host: "host", SessionID: id, Cwd: "/repo/" + id, Worktree: "/repo/" + id, RemoteURL: scopeRemote, RootCommit: "root", Source: "hook"})
	return Request{ExecutionID: out["execution_id"].(string), Generation: out["generation"].(int64)}
}

func TestWaitAndReplayUseTheKeyOnlyUnderWorktree(t *testing.T) {
	s, _, _ := testStore(t)
	holder, ho := scopeSession(t, s, "", "holder", "/repo/wt1", "dev:1", "main")
	repo := ho["repository_id"].(string)
	setScope(t, s, repo, "worktree")
	same, _ := scopeSession(t, s, "", "same", "/repo/wt1", "dev:1", "main")
	other, _ := scopeSession(t, s, "", "other", "/repo/wt2", "dev:2", "other")
	call(t, s, "touch", touchRequest(holder, "hold", "a.go"))
	// Same key conflicts and waits. A different key is advisory and waits for nothing.
	if got := call(t, s, "touch", touchRequest(same, "same", "a.go")); got["action"] != "deny" || waiterCount(t, s) != 1 {
		t.Fatal(got)
	}
	if got := call(t, s, "touch", touchRequest(other, "other", "a.go")); got["action"] != "allow" || waiterCount(t, s) != 1 {
		t.Fatal(got)
	}
	var sameKey string
	if err := s.db.QueryRow("SELECT COALESCE(worktree_key,'') FROM Waiter WHERE execution_id=?", same.ExecutionID).Scan(&sameKey); err != nil || sameKey == "" {
		t.Fatal("same-worktree wait lost its key", sameKey, err)
	}
	call(t, s, "release", releaseAll(holder))
	// The other worktree's automatic claim does not keep the same-key waiter asleep.
	if waiterCount(t, s) != 0 || woken(t, s, same) != 1 || woken(t, s, other) != 0 {
		t.Fatalf("wake waiters=%d same=%d other=%d", waiterCount(t, s), woken(t, s, same), woken(t, s, other))
	}
}

func TestFreedRewindExemptsOnlyTheWorktreePair(t *testing.T) {
	s, _, _ := testStore(t)
	holder, ho := scopeSession(t, s, "", "holder", "/repo/wt1", "dev:1", "main")
	repo := ho["repository_id"].(string)
	setScope(t, s, repo, "worktree")
	waiter, _ := scopeSession(t, s, "", "waiter", "/repo/wt1", "dev:1", "main")
	other, _ := scopeSession(t, s, "", "other", "/repo/wt2", "dev:2", "other")
	freed := freeFor(t, s, holder, waiter, "a", "a.go")
	call(t, s, "touch", touchRequest(other, "exempt", "a.go"))
	if got := call(t, s, "monitor_cursor", waiter)["cursor"]; got != freed-1 {
		t.Fatalf("exempt overlap blocked rewind: %v freed %d", got, freed)
	}
	// The other worktree holds the path, so the explicit claim is that session's
	// upgrade. A null-key explicit row is never the exempt pair.
	call(t, s, "claim", claimRequest(other, "explicit", "a.go", "exact"))
	if got := call(t, s, "monitor_cursor", waiter)["cursor"]; got == freed-1 {
		t.Fatal("explicit claim did not block rewind")
	}

	legacy := scopeSessionOnly(t, s, "legacy")
	call(t, s, "touch", touchRequest(holder, "b", "b.go"))
	if got := call(t, s, "touch", touchRequest(legacy, "legacy", "b.go")); got["action"] != "deny" {
		t.Fatal(got)
	}
	call(t, s, "release", releaseAll(holder))
	nullEvent := newestEvent(t, s, "claim_available")
	var eventKey sql.NullString
	if err := s.db.QueryRow("SELECT worktree_key FROM Event WHERE seq=?", nullEvent).Scan(&eventKey); err != nil || eventKey.Valid {
		t.Fatal("null-key wait recorded a key", eventKey, err)
	}
	call(t, s, "touch", touchRequest(other, "covers", "b.go"))
	if got := call(t, s, "monitor_cursor", legacy)["cursor"]; got == nullEvent-1 {
		t.Fatal("null-key event was treated as worktree-scoped")
	}
}

func TestRepositoryReplayStaysPathWideWhenKeysDiffer(t *testing.T) {
	s, _, _ := testStore(t)
	holder, ho := scopeSession(t, s, "", "holder", "/repo/wt1", "dev:1", "main")
	var scope string
	if err := s.db.QueryRow("SELECT auto_claim_scope FROM Repository WHERE id=?", ho["repository_id"]).Scan(&scope); err != nil || scope != "repository" {
		t.Fatal("default policy is not repository", scope, err)
	}
	waiter, _ := scopeSession(t, s, "", "waiter", "/repo/wt2", "dev:2", "other")
	freed := freeFor(t, s, holder, waiter, "a", "a.go")
	call(t, s, "touch", touchRequest(holder, "again", "a.go"))
	if got := call(t, s, "monitor_cursor", waiter)["cursor"]; got == freed-1 {
		t.Fatal("repository replay ignored a different worktree key")
	}
}

func TestTookClearsOnlyTheMatchingKeyUnderWorktree(t *testing.T) {
	for _, scope := range []string{"repository", "worktree"} {
		t.Run(scope, func(t *testing.T) {
			s, _, _ := testStore(t)
			a, ao := scopeSession(t, s, "", "a", "/repo/wt1", "dev:1", "main")
			repo := ao["repository_id"].(string)
			setScope(t, s, repo, scope)
			key := executionKey(t, s, a.ExecutionID).String
			until := int64(1800000000000 + 3600000)
			if _, err := s.db.Exec("INSERT INTO Waiter(execution_id,repository_id,path_key,kind,expires_ms,worktree_key) VALUES(?,?,'free.go','exact',?,'wt_other')", a.ExecutionID, repo, until); err != nil {
				t.Fatal(err)
			}
			call(t, s, "touch", touchRequest(a, "take", "free.go"))
			var left int
			if err := s.db.QueryRow("SELECT COUNT(*) FROM Waiter WHERE execution_id=? AND path_key='free.go'", a.ExecutionID).Scan(&left); err != nil {
				t.Fatal(err)
			}
			if scope == "worktree" && left != 1 {
				t.Fatal("worktree took cleared a different key")
			}
			if scope == "repository" && left != 0 {
				t.Fatal("repository took kept a keyed waiter")
			}
			if _, err := s.db.Exec("INSERT INTO Waiter(execution_id,repository_id,path_key,kind,expires_ms,worktree_key) VALUES(?,?,'next.go','exact',?,?)", a.ExecutionID, repo, until, key); err != nil {
				t.Fatal(err)
			}
			call(t, s, "touch", touchRequest(a, "take-same", "next.go"))
			if err := s.db.QueryRow("SELECT COUNT(*) FROM Waiter WHERE path_key='next.go'").Scan(&left); err != nil || left != 0 {
				t.Fatal("matching key was not cleared", left, err)
			}
		})
	}
}

func TestReplayOfAGroupedTouchUsesTheGrantKey(t *testing.T) {
	s, _, _ := testStore(t)
	a, ao := scopeSession(t, s, "", "a", "/repo/wt1", "dev:1", "main")
	repo := ao["repository_id"].(string)
	setScope(t, s, repo, "worktree")
	req := filesTouch(a, "both", wtGroup("/repo/wt1", "dev:1", "main", "a.go"), wtGroup("/repo/wt2", "dev:2", "other", "a.go"))
	if len(call(t, s, "touch_files", req)["granted"].([]map[string]any)) != 2 {
		t.Fatal("two checkouts did not both grant")
	}
	var wt1 int64
	if err := s.db.QueryRow("SELECT fence FROM Claim WHERE worktree_path=? AND revoked_ms IS NULL", "/repo/wt1").Scan(&wt1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("UPDATE Claim SET revoked_ms=1 WHERE fence=?", wt1); err != nil {
		t.Fatal(err)
	}
	setScope(t, s, repo, "repository")
	if got := call(t, s, "touch_files", req); got["action"] != "allow" {
		t.Fatal("repository replay ignored the surviving checkout", got)
	}
	setScope(t, s, repo, "worktree")
	expectError(t, s, "touch_files", req, "lost")
}

func TestPathIdentityIsUnchangedByTheScope(t *testing.T) {
	for _, scope := range []string{"repository", "worktree"} {
		t.Run(scope, func(t *testing.T) {
			s, _, _ := testStore(t)
			a, ao := scopeSession(t, s, "", "a", "/repo/wt1", "dev:1", "main")
			repo := ao["repository_id"].(string)
			if err := s.SetRepositoryPolicy(context.Background(), repo, "folded", "deny"); err != nil {
				t.Fatal(err)
			}
			setScope(t, s, repo, scope)
			b, _ := scopeSession(t, s, "", "b", "/repo/wt2", "dev:2", "other")
			call(t, s, "touch", touchRequest(a, "case", "SRC/A.go"))
			got := call(t, s, "touch", touchRequest(b, "case2", "src/a.go"))
			want := "deny"
			if scope == "worktree" {
				want = "allow"
			}
			if got["action"] != want {
				t.Fatalf("%s case fold: %v", scope, got)
			}
			boundary := call(t, s, "touch", touchRequest(b, "boundary", "src2/a.go"))
			if boundary["action"] != "allow" {
				t.Fatal(boundary)
			}
			call(t, s, "release", releaseAll(a))
			call(t, s, "release", releaseAll(b))
			wide := claimRequest(a, "wide", "", "prefix")
			wide.RepoWide = true
			call(t, s, "claim", wide)
			if covered := call(t, s, "touch", touchRequest(b, "covered", "anywhere.go")); covered["action"] != "deny" {
				t.Fatal(covered)
			}
		})
	}
}

func TestAskOnARealConflictBecomesDenyForCodex(t *testing.T) {
	for _, scope := range []string{"repository", "worktree"} {
		t.Run(scope, func(t *testing.T) {
			s, _, _ := testStore(t)
			holder, ho := scopeSession(t, s, "", "holder", "/repo/wt1", "dev:1", "main")
			repo := ho["repository_id"].(string)
			if err := s.SetRepositoryPolicy(context.Background(), repo, "sensitive", "ask"); err != nil {
				t.Fatal(err)
			}
			setScope(t, s, repo, scope)
			call(t, s, "touch", touchRequest(holder, "held", "a.go"))
			codex, _ := scopeSession(t, s, "", "codex", "/repo/wt1", "dev:1", "main")
			claude, _ := checkoutSession(t, s, "claude", "", "claude", "/repo/wt1", "dev:1", scopeRemote, "root", "main")
			if got := call(t, s, "touch", touchRequest(codex, "codex", "a.go")); got["action"] != "deny" || !strings.Contains(got["message"].(string), "Ask the human") {
				t.Fatal(got)
			}
			if got := call(t, s, "touch", touchRequest(claude, "claude", "a.go")); got["action"] != "ask" {
				t.Fatal(got)
			}
		})
	}
}

func TestTwoHostsGetDifferentKeysAndMayBothHoldAnAutomaticClaim(t *testing.T) {
	s, _, _ := testStore(t)
	wsl, wo := scopeSession(t, s, "wsl", "wsl", "/repo/app", "dev:1", "main")
	win, _ := scopeSession(t, s, "windows", "win", "/repo/app", "dev:1", "main")
	repo := wo["repository_id"].(string)
	if executionKey(t, s, wsl.ExecutionID).String == executionKey(t, s, win.ExecutionID).String {
		t.Fatal("two hosts share a worktree key")
	}
	call(t, s, "touch", touchRequest(wsl, "wsl", "a.go"))
	if got := call(t, s, "touch", touchRequest(win, "win", "a.go")); got["action"] != "deny" {
		t.Fatal("repository allowed two hosts", got)
	}
	call(t, s, "release", releaseAll(wsl))
	setScope(t, s, repo, "worktree")
	call(t, s, "touch", touchRequest(wsl, "wsl2", "a.go"))
	if got := call(t, s, "touch", touchRequest(win, "win2", "a.go")); got["action"] != "allow" || got["advisories"] == nil {
		t.Fatal(got)
	}
}

func TestFamilyExemptionIsNotAnAdvisory(t *testing.T) {
	s, _, _ := testStore(t)
	parent, po := scopeSession(t, s, "", "parent", "/repo/wt1", "dev:1", "main")
	setScope(t, s, po["repository_id"].(string), "worktree")
	call(t, s, "touch", touchRequest(parent, "parent", "a.go"))
	// ParentSessionID is the parent's session id, which scopeSession used as id.
	childReg := call(t, s, "sessions", Request{Client: "codex", Host: "host", SessionID: "child", ParentSessionID: "parent", Cwd: "/repo/wt2", Worktree: "/repo/wt2", WorktreeID: "dev:2", RemoteURL: scopeRemote, RootCommit: "root", Branch: "side", Source: "hook"})
	child := Request{ExecutionID: childReg["execution_id"].(string), Generation: childReg["generation"].(int64)}
	got := call(t, s, "touch", touchRequest(child, "child", "a.go"))
	if got["action"] != "allow" || got["advisories"] != nil || len(claimsOn(liveClaims(t, s, po["repository_id"].(string)), "a.go")) != 2 {
		t.Fatal(got, liveClaims(t, s, po["repository_id"].(string)))
	}
}

func TestConcurrentGrantsStayExclusiveExceptForTheExemptPair(t *testing.T) {
	for _, tc := range []struct {
		scope, left, right string
		winners            int
	}{
		{"repository", "dev:1", "dev:2", 1},
		{"worktree", "dev:1", "dev:2", 2},
		{"worktree", "dev:same", "dev:same", 1},
	} {
		t.Run(tc.scope+"/"+tc.left, func(t *testing.T) {
			s, _, _ := testStore(t)
			a, ao := scopeSession(t, s, "", "a", "/repo/"+tc.left, tc.left, "a")
			b, _ := scopeSession(t, s, "", "b", "/repo/"+tc.right, tc.right, "b")
			setScope(t, s, ao["repository_id"].(string), tc.scope)
			var winners, denied, failed atomicInt
			start := make(chan struct{})
			var wg sync.WaitGroup
			for _, r := range []Request{touchRequest(a, "a", "shared.go"), touchRequest(b, "b", "shared.go")} {
				wg.Add(1)
				go func(r Request) {
					defer wg.Done()
					<-start
					out, err := s.Call(context.Background(), "touch", r)
					switch {
					case err != nil:
						failed.add()
					case out["action"] == "allow":
						winners.add()
					case out["action"] == "deny":
						denied.add()
					default:
						failed.add()
					}
				}(r)
			}
			close(start)
			wg.Wait()
			if winners.n != tc.winners || denied.n != 2-tc.winners || failed.n != 0 {
				t.Fatalf("winners=%d denied=%d failed=%d", winners.n, denied.n, failed.n)
			}
		})
	}
}

type atomicInt struct{ n int }

func (a *atomicInt) add() {
	a.mu()
}

func (a *atomicInt) mu() {
	// The two goroutines only record a tally. A mutex keeps the count honest
	// without importing more than the test already needs.
	atomicMu.Lock()
	a.n++
	atomicMu.Unlock()
}

var atomicMu sync.Mutex

func TestResumeCopiesTheWorktreeIdentity(t *testing.T) {
	s, clock, file := testStore(t)
	reg := Request{Client: "codex", Host: "host", SessionID: "keep", Cwd: "/repo/wt1", Worktree: "/repo/wt1", WorktreeID: "dev:1", RemoteURL: scopeRemote, RootCommit: "root", Branch: "feature", Source: "hook"}
	out := call(t, s, "sessions", reg)
	a := Request{ExecutionID: out["execution_id"].(string), Generation: out["generation"].(int64)}
	key := executionKey(t, s, a.ExecutionID)
	if !key.Valid || len(key.String) < 4 {
		t.Fatal(key)
	}
	call(t, s, "touch", touchRequest(a, "kept", "src/api.go"))
	bare := call(t, s, "sessions", Request{Client: "codex", Host: "host", SessionID: "bare", Cwd: "/repo/bare", Worktree: "/repo/bare", RemoteURL: scopeRemote, RootCommit: "root", Source: "hook"})
	if executionKey(t, s, bare["execution_id"].(string)).Valid {
		t.Fatal("missing worktree id stored a key")
	}
	outside := call(t, s, "sessions_outside", Request{Client: "codex", Host: "home", SessionID: "out", Cwd: "/home/tmp", Source: "hook"})
	if executionKey(t, s, outside["execution_id"].(string)).Valid {
		t.Fatal("a session outside git stored a worktree key")
	}
	again := reg
	again.SessionID, again.WorktreeID = "bare", "dev:later"
	call(t, s, "sessions", again)
	if executionKey(t, s, bare["execution_id"].(string)).Valid {
		t.Fatal("resume backfilled a key onto an execution that already had claims without one")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s.lock = nil
	reopened, err := Open(file, clock)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	reg.Generation = out["generation"].(int64)
	resumed := call(t, reopened, "sessions", reg)
	if resumed["execution_id"] != a.ExecutionID {
		t.Fatal("recovery replaced the execution", resumed)
	}
	if executionKey(t, reopened, a.ExecutionID) != key {
		t.Fatal("recovery changed the execution key")
	}
	var livePath, liveKey, liveWorktree, liveBranch string
	var liveN, revokedN int
	if err = reopened.db.QueryRow("SELECT COUNT(*),path,worktree_key,worktree_path,branch FROM Claim WHERE execution_id=? AND revoked_ms IS NULL", a.ExecutionID).Scan(&liveN, &livePath, &liveKey, &liveWorktree, &liveBranch); err != nil {
		t.Fatal(err)
	}
	if err = reopened.db.QueryRow("SELECT COUNT(*) FROM Claim WHERE execution_id=? AND revoked_ms IS NOT NULL AND worktree_key=? AND worktree_path=? AND branch=?", a.ExecutionID, key.String, "/repo/wt1", "feature").Scan(&revokedN); err != nil {
		t.Fatal(err)
	}
	if liveN != 1 || revokedN != 1 || livePath != "src/api.go" || liveKey != key.String || liveWorktree != "/repo/wt1" || liveBranch != "feature" {
		t.Fatalf("copied claim live=%d revoked=%d %s %s %s %s", liveN, revokedN, livePath, liveKey, liveWorktree, liveBranch)
	}
}

func TestAdvisoryNamesAnUnknownBranchAndKeepsTheIgnoredNote(t *testing.T) {
	s, _, _ := testStore(t)
	held, ho := scopeSession(t, s, "", "held", "/repo/wt1", "dev:1", "")
	setScope(t, s, ho["repository_id"].(string), "worktree")
	incoming, _ := scopeSession(t, s, "", "incoming", "/repo/wt2", "dev:2", "side")
	call(t, s, "touch", touchRequest(held, "held", "a.go"))
	got := call(t, s, "touch", touchRequest(incoming, "incoming", "a.go", "go.sum"))
	message, _ := got["message"].(string)
	if got["action"] != "allow" || !strings.Contains(message, "on branch unknown.") || !strings.Contains(message, "ignored paths are advisory only") || !strings.HasPrefix(message, "Iris: a.go is also auto-claimed") {
		t.Fatal(got)
	}
	if !strings.Contains(message, "This repository allows overlapping automatic claims across worktrees") {
		t.Fatal(message)
	}
}

func TestShellAutoClaimSkipsANullKeyAndTakesAnExemptPath(t *testing.T) {
	s, _, _ := testStore(t)
	a, ao := scopeSession(t, s, "", "a", "/repo/wt1", "dev:1", "main")
	repo := ao["repository_id"].(string)
	setScope(t, s, repo, "worktree")
	other, _ := scopeSession(t, s, "", "other", "/repo/wt2", "dev:2", "other")
	call(t, s, "touch", touchRequest(other, "held", "held.go"))
	if _, err := s.db.Exec("INSERT INTO Claim(execution_id,repository_id,kind,path,path_key,origin,boot_epoch,expires_ms) SELECT ?,?,'exact','legacy.go','legacy.go','auto',boot_epoch,? FROM Execution WHERE id=?", a.ExecutionID, repo, int64(1800000000000+3600000), a.ExecutionID); err != nil {
		t.Fatal(err)
	}
	a.Kind, a.Status = "before", map[string]string{}
	call(t, s, "reconcile", a)
	a.Kind, a.Status = "after", map[string]string{"held.go": " M", "legacy.go": " M"}
	out := call(t, s, "reconcile", a)
	claimed := map[string]bool{}
	for _, p := range out["claimed"].([]string) {
		claimed[p] = true
	}
	legacy := claimsOn(liveClaims(t, s, repo), "legacy.go")
	if !claimed["held.go"] || claimed["legacy.go"] || len(claimsOn(liveClaims(t, s, repo), "held.go")) != 2 || len(legacy) != 1 || legacy[0].key.Valid {
		t.Fatalf("shell claim: %v legacy %+v", out, legacy)
	}
}

func TestAnOlderClaimInsertStaysRepositoryWide(t *testing.T) {
	s, _, _ := testStore(t)
	a, ao := scopeSession(t, s, "", "a", "/repo/wt1", "dev:1", "main")
	repo := ao["repository_id"].(string)
	setScope(t, s, repo, "worktree")
	b := scopeSessionOnly(t, s, "old")
	if _, err := s.db.Exec("INSERT INTO Claim(execution_id,repository_id,kind,path,path_key,origin,boot_epoch,expires_ms) SELECT ?,?,'exact','old.go','old.go','auto',boot_epoch,? FROM Execution WHERE id=?", b.ExecutionID, repo, int64(1800000000000+3600000), b.ExecutionID); err != nil {
		t.Fatal(err)
	}
	if got := call(t, s, "touch", touchRequest(a, "old", "old.go")); got["action"] != "deny" || got["advisories"] != nil {
		t.Fatal("legacy insert was exempt", got)
	}
}
