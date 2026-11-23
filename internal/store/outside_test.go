package store

import (
	"slices"
	"testing"
)

// outsideSession registers a session outside git on the route a new client uses for one.
func outsideSession(t *testing.T, s *Store, host, session, cwd string) (Request, map[string]any) {
	t.Helper()
	out := call(t, s, "sessions_outside", Request{Client: "codex", Host: host, SessionID: session, Cwd: cwd, Source: "hook"})
	return Request{ExecutionID: out["execution_id"].(string), Generation: out["generation"].(int64)}, out
}

func aliasesOf(t *testing.T, s *Store, repo string) []string {
	t.Helper()
	rows, err := s.db.Query("SELECT alias FROM RepositoryAlias WHERE repository_id=? ORDER BY alias", repo)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			t.Fatal(err)
		}
		out = append(out, a)
	}
	return out
}

func TestSessionsOutsideGitShareTheirHostsContext(t *testing.T) {
	s, _, _ := testStore(t)
	_, first := outsideSession(t, s, "home", "a", "/home/u/tmp")
	context := first["repository_id"].(string)
	if first["outside_git"] != true || first["worktree"] != "/home/u/tmp" || first["agent"].(map[string]any)["name"] != "codex-home-tmp" {
		t.Fatalf("first outside session: %v", first)
	}
	// An older client sends the same session on the session route, naming no checkout.
	legacy := call(t, s, "sessions", Request{Client: "codex", Host: "home", SessionID: "b", Cwd: "/home/u/notes", Source: "mcp"})
	if legacy["repository_id"] != context || legacy["outside_git"] != true || legacy["agent"].(map[string]any)["name"] != "codex-home-notes" {
		t.Fatalf("legacy outside session: %v", legacy)
	}
	peers := legacy["peers"].([]map[string]any)
	if len(peers) != 2 {
		t.Fatalf("peers: %v", peers)
	}
	_, other := outsideSession(t, s, "laptop", "c", "/Users/u/tmp")
	if other["repository_id"] == context {
		t.Fatal("two hosts share one context")
	}
	var name, root string
	if err := s.db.QueryRow("SELECT name,root_commit FROM Repository WHERE id=?", context).Scan(&name, &root); err != nil {
		t.Fatal(err)
	}
	if name != "outside git on home" || root != "" {
		t.Fatalf("context row: %q %q", name, root)
	}
	if got := aliasesOf(t, s, context); !slices.Equal(got, []string{"outside:home"}) {
		t.Fatalf("a context learned aliases: %v", got)
	}
	if n := count(t, s, "SELECT COUNT(*) FROM Repository"); n != 2 {
		t.Fatalf("a folder became a project: %d records", n)
	}
	// A git session's binding does not say outside git.
	_, git := session(t, s, 1)
	if _, ok := git["outside_git"]; ok {
		t.Fatal(git)
	}
}

func TestTheOutsideRouteRefusesACheckout(t *testing.T) {
	s, _, _ := testStore(t)
	for _, r := range []Request{{Worktree: "/r"}, {CommonDir: "/r/.git"}, {WorktreeID: "1:2"}, {RemoteURL: "https://example.com/r.git"}, {RootCommit: "root"}} {
		r.Client, r.Host, r.SessionID, r.Cwd = "codex", "home", "s", "/r"
		expectError(t, s, "sessions_outside", r, "bad_request")
	}
}

func TestAnActiveExecutionKeepsItsRecordAcrossKinds(t *testing.T) {
	s, _, _ := testStore(t)
	_, git := session(t, s, 1)
	repo := git["repository_id"].(string)
	before := aliasesOf(t, s, repo)
	// The same session, now seen without a checkout, stays in its repository.
	again := call(t, s, "sessions", Request{Client: "codex", Host: "host", SessionID: "1", Cwd: "/repo/w1", Source: "hook"})
	if again["repository_id"] != repo || again["execution_id"] != git["execution_id"] {
		t.Fatalf("a repository's session moved: %v", again)
	}
	if got := aliasesOf(t, s, repo); !slices.Equal(got, before) {
		t.Fatalf("an outside registration taught a repository: %v", got)
	}
	_, out := outsideSession(t, s, "home", "o", "/home/u/tmp")
	context := out["repository_id"].(string)
	// git init in that folder: the active session stays, learns nothing, and is not refused for
	// a root it never had.
	inited := call(t, s, "sessions", Request{Client: "codex", Host: "home", SessionID: "o", Cwd: "/home/u/tmp", Worktree: "/home/u/tmp", CommonDir: "/home/u/tmp/.git", RemoteURL: "https://example.com/new.git", RootCommit: "new-root", Source: "hook"})
	if inited["repository_id"] != context || inited["outside_git"] != true {
		t.Fatalf("an outside session moved: %v", inited)
	}
	if got := aliasesOf(t, s, context); !slices.Equal(got, []string{"outside:home"}) {
		t.Fatalf("a context learned git identity: %v", got)
	}
	// Generation checks apply as before.
	expectError(t, s, "sessions_outside", Request{Client: "codex", Host: "home", SessionID: "o", Cwd: "/home/u/tmp", Generation: 9}, "lost")
	// A new session in the folder, now a checkout, registers in a repository of its own.
	fresh := call(t, s, "sessions", Request{Client: "codex", Host: "home", SessionID: "fresh", Cwd: "/home/u/tmp", Worktree: "/home/u/tmp", CommonDir: "/home/u/tmp/.git", RemoteURL: "https://example.com/new.git", RootCommit: "new-root", Source: "hook"})
	if fresh["repository_id"] == context {
		t.Fatal("a checkout joined the context")
	}
}

func TestGitResolutionNeverFindsAContext(t *testing.T) {
	s, _, _ := testStore(t)
	_, out := outsideSession(t, s, "home", "o", "/home/u/tmp")
	context := out["repository_id"].(string)
	// An older hub could have taught a context a folder's path alias.
	if _, err := s.db.Exec("INSERT INTO RepositoryAlias VALUES('local:home:/home/u/tmp',?)", context); err != nil {
		t.Fatal(err)
	}
	git := call(t, s, "sessions", Request{Client: "codex", Host: "home", SessionID: "g", Cwd: "/home/u/tmp", Worktree: "/home/u/tmp", CommonDir: "/home/u/tmp/.git", Source: "hook"})
	if git["repository_id"] == context {
		t.Fatal("git resolution chose a context")
	}
}

func TestTwoContextsForOneHostAreRefused(t *testing.T) {
	s, _, _ := testStore(t)
	outsideSession(t, s, "home", "o", "/home/u/tmp")
	if _, err := s.db.Exec("INSERT INTO Repository(id,case_policy,root_commit,name) VALUES('rp_second','folded','','second')"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("INSERT INTO RepositoryAlias VALUES('outside:home','rp_second')"); err != nil {
		t.Fatal(err)
	}
	expectError(t, s, "sessions_outside", Request{Client: "codex", Host: "home", SessionID: "p", Cwd: "/home/u/other"}, "conflict")
}

func TestASessionOutsideGitTakesNoClaims(t *testing.T) {
	s, _, _ := testStore(t)
	o, _ := outsideSession(t, s, "home", "o", "/home/u/tmp")
	expectError(t, s, "touch", touchRequest(o, "t1", "x"), "outside_git")
	files := o
	files.ToolCallID = "t2"
	files.Groups = []ClaimGroup{{Worktree: "/home/u/tmp/repo", Paths: []string{"x"}}}
	expectError(t, s, "touch_files", files, "outside_git")
	expectError(t, s, "claim", claimRequest(o, "c1", "x", "exact"), "outside_git")
	explicit := claimRequest(o, "c2", "x", "exact")
	explicit.Checkout = &ClaimGroup{Worktree: "/home/u/tmp/repo"}
	expectError(t, s, "claim_files", explicit, "outside_git")
	shell := o
	shell.Kind, shell.Status = "before", map[string]string{"x": " M"}
	expectError(t, s, "reconcile", shell, "outside_git")
	// A shell hook that registers its session refuses as a whole.
	registering := Request{Client: "codex", Host: "home", SessionID: "o", Cwd: "/home/u/tmp", Worktree: "/home/u/tmp", CommonDir: "/home/u/tmp/.git", Source: "hook", Kind: "before", Status: map[string]string{"x": " M"}}
	expectError(t, s, "reconcile", registering, "outside_git")
	if n := count(t, s, "SELECT (SELECT COUNT(*) FROM Claim)+(SELECT COUNT(*) FROM Waiter)+(SELECT COUNT(*) FROM ShellSnapshot)+(SELECT COUNT(*) FROM ReducedShellSnapshot)"); n != 0 {
		t.Fatalf("an outside session left %d claims, waits or baselines", n)
	}
	// Release and end stay open.
	release := o
	release.All = true
	call(t, s, "release", release)
	call(t, s, "end", o)
}

func TestMailReachesASessionOutsideGit(t *testing.T) {
	s, _, _ := testStore(t)
	o, out := outsideSession(t, s, "home", "o", "/home/u/tmp")
	a, _ := session(t, s, 1)
	mail := a
	mail.Key, mail.To, mail.Body = "hello", []string{out["agent"].(map[string]any)["name"].(string)}, "hello"
	call(t, s, "messages", mail)
	deliveries := call(t, s, "inbox", o)["deliveries"].([]map[string]any)
	if len(deliveries) != 1 || deliveries[0]["cross_repository"] != true {
		t.Fatalf("inbox: %v", deliveries)
	}
	call(t, s, "ack", Request{ExecutionID: o.ExecutionID, Generation: o.Generation, Key: deliveries[0]["id"].(string), Attempt: 1, Handled: true})
}

func TestUnaliasKeepsAContextsMarker(t *testing.T) {
	s, _, _ := testStore(t)
	_, out := outsideSession(t, s, "home", "o", "/home/u/tmp")
	expectError(t, s, "unalias", Request{RepositoryID: out["repository_id"].(string), Aliases: []string{"outside:home"}, DryRun: true}, "conflict")
}

func TestTheConsoleNamesAContextsKindAndHost(t *testing.T) {
	s, _, _ := testStore(t)
	outsideSession(t, s, "home", "o", "/home/u/tmp")
	session(t, s, 1)
	kinds := map[string]string{}
	for _, r := range call(t, s, "ui_overview", Request{})["repositories"].([]map[string]any) {
		host, _ := r["host"].(string)
		kinds[r["kind"].(string)] = host
	}
	if len(kinds) != 2 || kinds["outside"] != "home" || kinds["repository"] != "" {
		t.Fatalf("overview kinds: %v", kinds)
	}
}

// folderProject makes what a hub before ADR 034 made of a session outside git, and ends it.
func folderProject(t *testing.T, s *Store, host, session, dir string) (string, map[string]any) {
	t.Helper()
	out := call(t, s, "sessions", Request{Client: "codex", Host: host, SessionID: session, Cwd: dir, Worktree: dir, Source: "mcp"})
	return out["repository_id"].(string), out
}

func TestOrdinaryMergeRefusesAContext(t *testing.T) {
	s, _, _ := testStore(t)
	folder, _ := folderProject(t, s, "home", "f", "/home/u/scripts")
	_, out := outsideSession(t, s, "home", "o", "/home/u/tmp")
	context := out["repository_id"].(string)
	expectError(t, s, "merge", Request{FromRepository: folder, IntoRepository: context, DryRun: true}, "conflict")
	expectError(t, s, "merge", Request{FromRepository: context, IntoRepository: folder, DryRun: true}, "conflict")
	expectError(t, s, "merge_outside", Request{FromRepository: context, IntoRepository: folder, DryRun: true}, "bad_request")
	_, laptop := outsideSession(t, s, "laptop", "l", "/Users/u/tmp")
	expectError(t, s, "merge_outside", Request{FromRepository: laptop["repository_id"].(string), IntoRepository: context, DryRun: true}, "bad_request")
}

func TestRepairMovesAQuietFolderProjectIntoItsContext(t *testing.T) {
	s, _, _ := testStore(t)
	folder, f := folderProject(t, s, "home", "f", "/home/u/scripts")
	fr := Request{ExecutionID: f["execution_id"].(string), Generation: f["generation"].(int64)}
	a, _ := session(t, s, 1)
	mail := a
	mail.Key, mail.To, mail.Body = "hi", []string{f["agent"].(map[string]any)["name"].(string)}, "hi"
	sent := call(t, s, "messages", mail)
	_, out := outsideSession(t, s, "home", "o", "/home/u/tmp")
	context := out["repository_id"].(string)
	repair := Request{FromRepository: folder, IntoRepository: context, DryRun: true}
	expectError(t, s, "merge_outside", repair, "conflict") // its session is still active
	call(t, s, "end", fr)
	// A wait and a shell baseline left by the folder's session are dropped, not moved.
	if _, err := s.db.Exec("INSERT INTO Waiter(execution_id,repository_id,path_key,kind,expires_ms) VALUES(?,?,'x','exact',0)", fr.ExecutionID, folder); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("INSERT INTO ShellSnapshot VALUES(?,'{}')", fr.ExecutionID); err != nil {
		t.Fatal(err)
	}
	plan := call(t, s, "merge_outside", repair)
	dropped := plan["dropped"].(map[string]any)
	if plan["outside"] != true || dropped["waiters"] != 1 || dropped["shell_snapshots"] != 1 || len(dropped["aliases"].([]string)) != 2 || plan["coalesced_aliases"] != 0 {
		t.Fatalf("plan: %v", plan)
	}
	if n := count(t, s, "SELECT COUNT(*) FROM Repository WHERE id=?", folder); n != 1 {
		t.Fatal("a dry run changed records")
	}
	repair.DryRun = false
	call(t, s, "merge_outside", repair)
	if got := aliasesOf(t, s, context); !slices.Equal(got, []string{"outside:home"}) {
		t.Fatalf("the context took a folder's aliases: %v", got)
	}
	if n := count(t, s, "SELECT (SELECT COUNT(*) FROM Repository WHERE id=?1)+(SELECT COUNT(*) FROM Waiter)+(SELECT COUNT(*) FROM ShellSnapshot)", folder); n != 0 {
		t.Fatalf("%d folder records, waits or baselines remain", n)
	}
	var agentRepo, messageRepo string
	if err := s.db.QueryRow("SELECT repository_id FROM Agent WHERE id=?", f["agent"].(map[string]any)["id"]).Scan(&agentRepo); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow("SELECT repository_id FROM Message WHERE id=?", sent["message_id"]).Scan(&messageRepo); err != nil {
		t.Fatal(err)
	}
	if agentRepo != context || messageRepo == "" {
		t.Fatalf("the folder's mailbox did not move: agent %s", agentRepo)
	}
	// Its directory's next session gets its mailbox back in the context.
	back := call(t, s, "sessions_outside", Request{Client: "codex", Host: "home", SessionID: "f", Cwd: "/home/u/scripts", Source: "hook"})
	if back["agent"].(map[string]any)["id"] != f["agent"].(map[string]any)["id"] {
		t.Fatalf("mailbox not returned: %v", back["agent"])
	}
}

func TestRepairRefusesWhatIsNotAQuietFolderProjectOfThisHost(t *testing.T) {
	s, _, _ := testStore(t)
	_, out := outsideSession(t, s, "home", "o", "/home/u/tmp")
	context := out["repository_id"].(string)
	ended := func(host, session, dir string, extra Request) string {
		extra.Client, extra.Host, extra.SessionID, extra.Cwd, extra.Source = "codex", host, session, dir, "mcp"
		if extra.Worktree == "" {
			extra.Worktree = dir
		}
		r := call(t, s, "sessions", extra)
		call(t, s, "end", Request{ExecutionID: r["execution_id"].(string), Generation: r["generation"].(int64)})
		return r["repository_id"].(string)
	}
	for name, repo := range map[string]string{
		"a root":         ended("home", "root", "/home/u/rooted", Request{RootCommit: "root"}),
		"a git dir":      ended("home", "gitdir", "/home/u/checkout", Request{CommonDir: "/home/u/checkout/.git"}),
		"a remote":       ended("home", "remote", "/home/u/remote", Request{RemoteURL: "https://example.com/r.git"}),
		"another host":   ended("laptop", "laptop", "/Users/u/scripts", Request{}),
		"isolated ok":    ended("home", "plain", "/home/u/plain", Request{}),
		"a foreign seat": ended("home", "seat", "/home/u/seat", Request{}),
	} {
		switch name {
		case "isolated ok":
			// Its own isolation marker is a folder's alias; the repair takes it.
			if _, err := s.db.Exec("INSERT INTO RepositoryAlias VALUES(?,?)", isolatedAlias(repo), repo); err != nil {
				t.Fatal(err)
			}
			call(t, s, "merge_outside", Request{FromRepository: repo, IntoRepository: context, DryRun: true})
			continue
		case "a foreign seat":
			if _, err := s.db.Exec("UPDATE Execution SET host_id='laptop' WHERE repository_id=?", repo); err != nil {
				t.Fatal(err)
			}
		}
		expectError(t, s, "merge_outside", Request{FromRepository: repo, IntoRepository: context, DryRun: true}, "conflict")
	}
	// Another repository's live session holding a path in the folder blocks the repair.
	held := ended("home", "held", "/home/u/held", Request{})
	a, _ := session(t, s, 1)
	if _, err := s.db.Exec("INSERT INTO Claim(execution_id,repository_id,kind,path,path_key,origin,boot_epoch) SELECT ?,?,'exact','x','x','explicit',boot_epoch FROM Execution WHERE id=?", a.ExecutionID, held, a.ExecutionID); err != nil {
		t.Fatal(err)
	}
	expectError(t, s, "merge_outside", Request{FromRepository: held, IntoRepository: context, DryRun: true}, "conflict")
}

// After a hub start a session outside git reattaches with its generation, keeps its mailbox and
// mail, and recovery writes no claim, wait or fence for it (ADR 034, decision 5).
func TestASessionOutsideGitRecoversWithItsMailAndNoClaims(t *testing.T) {
	s, clock, file := testStore(t)
	o, out := outsideSession(t, s, "home", "o", "/home/u/tmp")
	a, _ := session(t, s, 1)
	mail := a
	mail.Key, mail.To, mail.Body = "kept", []string{out["agent"].(map[string]any)["name"].(string)}, "kept"
	call(t, s, "messages", mail)
	var fence int64
	if err := s.db.QueryRow("SELECT COALESCE(MAX(fence),0) FROM Claim").Scan(&fence); err != nil {
		t.Fatal(err)
	}
	reopened := restart(t, s, file, clock)
	back := call(t, reopened, "sessions_outside", Request{Client: "codex", Host: "home", SessionID: "o", Cwd: "/home/u/tmp", Generation: o.Generation, Source: "hook"})
	if back["execution_id"] != o.ExecutionID || back["outside_git"] != true || back["unread"] != 1 {
		t.Fatalf("recovery: %v", back)
	}
	var claims, waiters, after int64
	if err := reopened.db.QueryRow("SELECT (SELECT COUNT(*) FROM Claim WHERE execution_id=?1),(SELECT COUNT(*) FROM Waiter WHERE execution_id=?1),(SELECT COALESCE(MAX(fence),0) FROM Claim)", o.ExecutionID).Scan(&claims, &waiters, &after); err != nil {
		t.Fatal(err)
	}
	// a's one live claim, if any, is copied to a new fence when a recovers; o has none to copy.
	if claims != 0 || waiters != 0 || after != fence {
		t.Fatalf("recovery of an outside session wrote %d claims, %d waits, fence %d after %d", claims, waiters, after, fence)
	}
}

// A scoped shell reconciliation and a repeated claim call are refused like any other, and the
// refusal is not cached: a retry is refused again, not replayed (ADR 034, decision 5).
func TestEveryClaimRouteRefusesAgainOnRetry(t *testing.T) {
	s, _, _ := testStore(t)
	o, _ := outsideSession(t, s, "home", "o", "/home/u/tmp")
	scoped := reduced(o, "before", map[string]string{"x": " M"})
	for i := 0; i < 2; i++ {
		expectError(t, s, "reconcile_scoped", scoped, "outside_git")
		expectError(t, s, "touch", touchRequest(o, "same", "x"), "outside_git")
		expectError(t, s, "claim", claimRequest(o, "same", "x", "exact"), "outside_git")
	}
	if n := count(t, s, "SELECT COUNT(*) FROM Operation"); n != 0 {
		t.Fatalf("%d refusals were cached", n)
	}
}

// Only the console's person is no host's; an ordinary session whose client calls itself human,
// from another host, refuses the repair like any other (ADR 034, decision 9).
func TestRepairChecksAnOrdinaryHumanSessionsHost(t *testing.T) {
	s, _, _ := testStore(t)
	_, out := outsideSession(t, s, "home", "o", "/home/u/tmp")
	folder, f := folderProject(t, s, "home", "f", "/home/u/scripts")
	call(t, s, "end", Request{ExecutionID: f["execution_id"].(string), Generation: f["generation"].(int64)})
	if _, err := s.db.Exec("UPDATE Agent SET client='human',host='laptop' WHERE id=?", f["agent"].(map[string]any)["id"]); err != nil {
		t.Fatal(err)
	}
	expectError(t, s, "merge_outside", Request{FromRepository: folder, IntoRepository: out["repository_id"].(string), DryRun: true}, "conflict")
}
