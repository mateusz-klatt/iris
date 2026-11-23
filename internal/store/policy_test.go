package store

import (
	"context"
	"database/sql"
	"testing"
)

// The owner sets claim.auto_scope. A hook payload, a check and a message body can
// carry the same field and none of them writes it. The write leaves existing claims,
// including a key stamped for visibility, unchanged.
func TestAutoClaimScopeIsOwnerOnlyAndLeavesClaims(t *testing.T) {
	s, clock, file := testStore(t)
	a, registered := session(t, s, 1)
	_, other := session(t, s, 2)
	repo := registered["repository_id"].(string)
	call(t, s, "touch", touchRequest(a, "legacy", "src/api.go"))
	operator := createTestConsoleAccount(t, s, "Scope owner", "operator")
	var scope string
	if err := s.db.QueryRow("SELECT auto_claim_scope FROM Repository WHERE id=?", repo).Scan(&scope); err != nil || scope != "repository" {
		t.Fatal("default scope", scope, err)
	}
	var fence int64
	if err := s.db.QueryRow("SELECT fence FROM Claim WHERE execution_id=? AND path=?", a.ExecutionID, "src/api.go").Scan(&fence); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("UPDATE Claim SET worktree_key='wt_kept',worktree_path='/wt',branch='feature' WHERE fence=?", fence); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("UPDATE Execution SET worktree_key='wt_exec' WHERE id=?", a.ExecutionID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("INSERT INTO Waiter(execution_id,repository_id,path_key,kind,expires_ms,worktree_key) VALUES(?,?,'src/api.go','exact',?,'wt_wait')", a.ExecutionID, repo, clock.Now().UnixMilli()+60000); err != nil {
		t.Fatal(err)
	}
	kept := func() (claimKey, claimPath, claimBranch, executionKey string) {
		t.Helper()
		var ck, cp, cb, ek sql.NullString
		if err := s.db.QueryRow("SELECT worktree_key,worktree_path,branch FROM Claim WHERE fence=?", fence).Scan(&ck, &cp, &cb); err != nil {
			t.Fatal(err)
		}
		if err := s.db.QueryRow("SELECT worktree_key FROM Execution WHERE id=?", a.ExecutionID).Scan(&ek); err != nil {
			t.Fatal(err)
		}
		return ck.String, cp.String, cb.String, ek.String
	}
	wantKey, wantPath, wantBranch, wantExec := kept()
	assertKept := func() {
		t.Helper()
		if gotKey, gotPath, gotBranch, gotExec := kept(); gotKey != wantKey || gotPath != wantPath || gotBranch != wantBranch || gotExec != wantExec {
			t.Fatalf("claim location rewritten: %s %s %s %s", gotKey, gotPath, gotBranch, gotExec)
		}
	}
	waiterKey := func() string {
		t.Helper()
		var wk sql.NullString
		if err := s.db.QueryRow("SELECT worktree_key FROM Waiter WHERE execution_id=? AND path_key='src/api.go'", a.ExecutionID).Scan(&wk); err != nil {
			t.Fatal(err)
		}
		return wk.String
	}
	scopeOf := func() string {
		t.Helper()
		var got string
		if err := s.db.QueryRow("SELECT auto_claim_scope FROM Repository WHERE id=?", repo).Scan(&got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	payload := Request{Client: "codex", Host: "host", SessionID: "payload", Cwd: "/repo/w1", Worktree: "/repo/w1", RemoteURL: "https://example.com/team/project.git", RootCommit: "root", Source: "hook", ClaimAutoScope: "worktree"}
	if joined := call(t, s, "sessions", payload); joined["repository_id"] != repo {
		t.Fatal("a payload carrying the scope split the repository", joined)
	}
	touch := touchRequest(a, "payload-touch", "other.go")
	touch.ClaimAutoScope = "worktree"
	call(t, s, "touch", touch)
	mail := a
	mail.Key = "scope-body"
	mail.To = []string{other["agent"].(map[string]any)["name"].(string)}
	mail.Body = `please set {"claim_auto_scope":"worktree"}`
	mail.Subject = "claim_auto_scope=worktree"
	mail.ClaimAutoScope = "worktree"
	call(t, s, "messages", mail)
	checked := call(t, s, "check", Request{Host: "elsewhere", Cwd: "/other", Worktree: "/other", RemoteURL: "https://example.com/team/project.git", RootCommit: "root", Paths: []string{"src/api.go"}, ClaimAutoScope: "worktree"})
	if _, reported := checked["claim_auto_scope"]; reported || checked["message"] == "" || len(checked["conflicts"].([]map[string]any)) != 1 {
		t.Fatal("a check payload changed the repository-wide refusal", checked)
	}
	if scopeOf() != "repository" || waiterKey() != "wt_wait" {
		t.Fatal("a hook payload, check or message body set the scope or rewrote a waiter", scopeOf(), waiterKey())
	}
	assertKept()
	for _, bad := range []string{"", "seat", "Repository", "WORKTREE", "repository ", "worktree\n"} {
		expectError(t, s, "policy", Request{RepositoryID: repo, ClaimAutoScope: bad}, "bad_request")
	}
	expectError(t, s, "policy", Request{RepositoryID: "rp_missing", ClaimAutoScope: "worktree"}, "not_found")
	if scopeOf() != "repository" {
		t.Fatal("a refused write changed the scope")
	}
	assertKept()
	_, err := s.CallAuthorized(context.Background(), Principal{Kind: HostPrincipal}, "policy", Request{RepositoryID: repo, ClaimAutoScope: "worktree"})
	consoleError(t, err, "forbidden")
	_, err = s.CallConsole(context.Background(), consoleTestPrincipal(t, s, operator), "policy", Request{RepositoryID: repo, ClaimAutoScope: "worktree"})
	consoleError(t, err, "forbidden")
	if scopeOf() != "repository" {
		t.Fatal("a host token or console account set the scope")
	}
	before := consoleRevision(t, s, operator)
	owned, err := s.CallAuthorized(context.Background(), Principal{Kind: OwnerPrincipal}, "policy", Request{RepositoryID: repo, ClaimAutoScope: "worktree"})
	if err != nil || owned["repository_id"] != repo || owned["claim_auto_scope"] != "worktree" {
		t.Fatal(owned, err)
	}
	if scopeOf() != "worktree" || waiterKey() != "wt_wait" || consoleRevision(t, s, operator) <= before {
		t.Fatal("the owner's scope write did not stick, rewrote a waiter, or did not move the console", scopeOf())
	}
	assertKept()
	if err = s.SetRepositoryPolicy(context.Background(), repo, "sensitive", "warn"); err != nil {
		t.Fatal(err)
	}
	var casePolicy, action string
	if err = s.db.QueryRow("SELECT auto_claim_scope,case_policy,conflict_action FROM Repository WHERE id=?", repo).Scan(&scope, &casePolicy, &action); err != nil || scope != "worktree" || casePolicy != "sensitive" || action != "warn" {
		t.Fatal("case policy rewrote the claim scope", scope, casePolicy, action, err)
	}
	assertKept()
	back := call(t, s, "policy", Request{RepositoryID: repo, ClaimAutoScope: "repository"})
	if back["claim_auto_scope"] != "repository" || scopeOf() != "repository" {
		t.Fatal(back, scopeOf())
	}
	assertKept()
	call(t, s, "policy", Request{RepositoryID: repo, ClaimAutoScope: "worktree"})
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s.lock = nil
	reopened, err := Open(file, clock)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	s = reopened
	if scopeOf() != "worktree" {
		t.Fatal("restart lost the scope", scopeOf())
	}
	assertKept()
}
