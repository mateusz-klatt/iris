package store

import (
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

func consoleExplainLines(t *testing.T, db *sql.DB, query string, args []any) []string {
	t.Helper()
	rows, err := db.Query("EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatalf("explain failed: %v\nsql: %s", err, query)
	}
	defer func() { _ = rows.Close() }()
	var lines []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return lines
}

func TestConsoleReadQueryPlansAvoidAckSubqueryScan(t *testing.T) {
	clock := newSyncFakeClock(time.Unix(1800000000, 0))
	tr := &sqlTracer{}
	s, err := openDatabase(t.TempDir()+"/plans.db", clock, registerTraceDriver(tr))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	regA := call(t, s, "sessions", Request{
		Client: "codex", Host: "host", SessionID: "plan-a",
		Cwd: "/repo/a", Worktree: "/repo/a",
		RemoteURL: "https://example.invalid/team/repo-a.git", RootCommit: "root-a",
	})
	execA := Request{ExecutionID: regA["execution_id"].(string), Generation: regA["generation"].(int64)}
	repoA := regA["repository_id"].(string)

	regB := call(t, s, "sessions", Request{
		Client: "claude", Host: "host", SessionID: "plan-b",
		Cwd: "/repo/b", Worktree: "/repo/b",
		RemoteURL: "https://example.invalid/team/repo-b.git", RootCommit: "root-b",
	})
	agentB := regB["agent"].(map[string]any)["name"].(string)

	// Create 110 threads so 55 involve human and ui_threads(Agent="human", limit=50) pages cleanly,
	// mixing human mail (acked and unacked) and agent mail across repositories.
	for i := range 110 {
		clock.advance(time.Second)
		to := []string{agentB}
		if i%2 == 0 {
			to = []string{"human", agentB}
		}
		sent := call(t, s, "messages", Request{
			ExecutionID: execA.ExecutionID, Generation: execA.Generation,
			Key: fmt.Sprintf("plan-msg-%03d", i),
			To:  to, Subject: fmt.Sprintf("Subject %03d", i), Body: fmt.Sprintf("needle body %03d", i),
		})
		if i%4 == 0 {
			call(t, s, "ui_read", Request{Key: sent["thread_id"].(string), RepositoryID: repoA})
		}
	}

	assertNoNotIn := func(op string, stmts []sqlStatementRecord) map[string]sqlStatementRecord {
		byLabel := map[string]sqlStatementRecord{}
		seenDeferred := false
		for _, st := range stmts {
			if st.Label == "begin_immediate" || strings.HasPrefix(st.Label, "expire_") {
				t.Fatalf("%s ran unexpected writer/expiry statement %s: %s", op, st.Label, st.SQL)
			}
			if st.Label == "begin_deferred" {
				seenDeferred = true
			}
			if strings.Contains(strings.ToUpper(st.SQL), "NOT IN") {
				t.Fatalf("%s executed unexpected NOT IN query: %s", op, st.SQL)
			}
			if _, exists := byLabel[st.Label]; !exists {
				byLabel[st.Label] = st
			}
		}
		if !seenDeferred {
			t.Fatalf("%s did not execute BEGIN DEFERRED on read connection", op)
		}
		return byLabel
	}

	// Trace ui_threads(Agent="human", Limit=50).
	tr.reset(true)
	threadsOut := call(t, s, "ui_threads", Request{RepositoryID: "", Agent: "human", Limit: 50})
	threadsStmts := tr.snapshot()
	tr.reset(false)

	byThreads := assertNoNotIn("ui_threads(human)", threadsStmts)
	if pageThreads := threadsOut["threads"].([]map[string]any); len(pageThreads) != 50 {
		t.Fatalf("expected 50 paged threads, got %d", len(pageThreads))
	}

	unreadCount := 0
	for _, st := range threadsStmts {
		if st.Label == "threads_unread_query" {
			unreadCount++
		}
	}
	if unreadCount != 1 {
		t.Fatalf("expected exactly 1 threads_unread_query per page, got %d", unreadCount)
	}

	threadsMain, ok := byThreads["threads_main_query"]
	if !ok {
		t.Fatalf("missing threads_main_query in ui_threads statements")
	}
	if strings.Contains(threadsMain.SQL, "Ack") {
		t.Fatalf("threads_main_query must not touch Ack: %s", threadsMain.SQL)
	}

	threadsUnread, ok := byThreads["threads_unread_query"]
	if !ok {
		t.Fatalf("missing threads_unread_query in ui_threads statements")
	}
	if !strings.Contains(threadsUnread.SQL, "m.thread_id IN (") {
		t.Fatalf("threads_unread_query must restrict to paged thread_ids: %s", threadsUnread.SQL)
	}
	inListStart := strings.Index(threadsUnread.SQL, "m.thread_id IN (") + len("m.thread_id IN (")
	inListEnd := strings.Index(threadsUnread.SQL[inListStart:], ")")
	inPlaceholders := 1 + strings.Count(threadsUnread.SQL[inListStart:inListStart+inListEnd], ",")
	if inPlaceholders != 50 {
		t.Fatalf("expected threads_unread_query IN list to have 50 paged ids, got %d (%s)", inPlaceholders, threadsUnread.SQL)
	}
	unreadPlan := strings.Join(consoleExplainLines(t, s.db, threadsUnread.SQL, threadsUnread.Args), "\n")
	if strings.Contains(unreadPlan, "SCAN Ack") || strings.Contains(unreadPlan, "LIST SUBQUERY") {
		t.Fatalf("threads_unread_query plan scanned Ack or built list subquery:\n%s", unreadPlan)
	}
	if !strings.Contains(unreadPlan, "sqlite_autoindex_Ack_1") || !strings.Contains(unreadPlan, "message_thread") {
		t.Fatalf("expected threads_unread_query plan to use message_thread and sqlite_autoindex_Ack_1:\n%s", unreadPlan)
	}

	// Trace ui_overview.
	tr.reset(true)
	call(t, s, "ui_overview", Request{})
	overviewStmts := tr.snapshot()
	tr.reset(false)

	byOverview := assertNoNotIn("ui_overview", overviewStmts)
	for _, label := range []string{"overview_agents", "overview_human_unread"} {
		st, ok := byOverview[label]
		if !ok {
			t.Fatalf("missing %s in ui_overview statements", label)
		}
		plan := strings.Join(consoleExplainLines(t, s.db, st.SQL, st.Args), "\n")
		if strings.Contains(plan, "SCAN Ack") || strings.Contains(plan, "LIST SUBQUERY") {
			t.Fatalf("%s plan scanned Ack or built list subquery:\n%s", label, plan)
		}
		if !strings.Contains(plan, "sqlite_autoindex_Ack_1") {
			t.Fatalf("expected %s plan to use sqlite_autoindex_Ack_1:\n%s", label, plan)
		}
	}

	// Also verify unfiltered and FTS search variants of ui_threads emit no NOT IN over Ack.
	for _, req := range []Request{{Limit: 50}, {Q: "needle", Agent: "human", Limit: 50}, {Agent: agentB, Limit: 50}} {
		tr.reset(true)
		call(t, s, "ui_threads", req)
		stmts := tr.snapshot()
		tr.reset(false)
		assertNoNotIn("ui_threads(variant)", stmts)
	}
}

func TestThreadsSplitPreservesVariantsTitlesParticipantsAuthorsUnreadAndCursor(t *testing.T) {
	clock := newSyncFakeClock(time.Unix(1800000000, 0))
	s, err := Open(t.TempDir()+"/threads-behavior.db", clock)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	regA := call(t, s, "sessions", Request{
		Client: "codex", Host: "host", SessionID: "beh-a",
		Cwd: "/repo/a", Worktree: "/repo/a",
		RemoteURL: "https://example.invalid/team/repo-a.git", RootCommit: "root-a",
	})
	execA := Request{ExecutionID: regA["execution_id"].(string), Generation: regA["generation"].(int64)}
	repoA := regA["repository_id"].(string)
	agentA := regA["agent"].(map[string]any)["name"].(string)

	regB := call(t, s, "sessions", Request{
		Client: "claude", Host: "host", SessionID: "beh-b",
		Cwd: "/repo/b", Worktree: "/repo/b",
		RemoteURL: "https://example.invalid/team/repo-b.git", RootCommit: "root-b",
	})
	execB := Request{ExecutionID: regB["execution_id"].(string), Generation: regB["generation"].(int64)}
	repoB := regB["repository_id"].(string)
	agentB := regB["agent"].(map[string]any)["name"].(string)

	admin := createTestConsoleAccount(t, s, "Thread Author", "operator")
	principal := readConsolePrincipal(t, s, admin)

	// Thread T0: initiated by operator account across human, agentA, agentB; second reply changes subject,
	// so earliest visible subject must stay "Initial Title".
	first, err := s.CallConsole(t.Context(), principal, "ui_messages", Request{
		RepositoryID: repoA,
		Key:          "beh-first",
		To:           []string{agentA},
		Subject:      "Initial Title",
		Body:         "first body",
	})
	if err != nil {
		t.Fatal(err)
	}
	thread0 := first["thread_id"].(string)
	clock.advance(time.Second)
	reply1 := call(t, s, "messages", Request{
		ExecutionID: execA.ExecutionID, Generation: execA.Generation,
		Key: "beh-reply-1", ReplyTo: first["message_id"].(string),
		To: []string{"human", agentB}, Subject: "Reply Subject Ignored", Body: "to both humans",
	})
	// Deliver to repoB human as well so repoA vs "" scopes differ on unread.
	clock.advance(time.Second)
	call(t, s, "messages", Request{
		ExecutionID: execB.ExecutionID, Generation: execB.Generation,
		Key: "beh-reply-2", ReplyTo: reply1["message_id"].(string),
		To: []string{"human"}, Subject: "RepoB Reply", Body: "to repoB human",
	})

	// Create 54 newer agent-only threads so pagination across 55 total threads exercises next cursor.
	expectedOrder := make([]string, 0, 55)
	for i := 1; i <= 54; i++ {
		clock.advance(time.Second)
		m := call(t, s, "messages", Request{
			ExecutionID: execA.ExecutionID, Generation: execA.Generation,
			Key: fmt.Sprintf("beh-extra-%02d", i),
			To:  []string{agentB}, Subject: fmt.Sprintf("Extra %02d", i), Body: "body",
		})
		expectedOrder = append(expectedOrder, m["thread_id"].(string))
	}
	// Reverse expectedOrder + append thread0 at end (oldest).
	for l, r := 0, len(expectedOrder)-1; l < r; l, r = l+1, r-1 {
		expectedOrder[l], expectedOrder[r] = expectedOrder[r], expectedOrder[l]
	}
	expectedOrder = append(expectedOrder, thread0)

	page1 := call(t, s, "ui_threads", Request{Limit: 50})
	list1 := page1["threads"].([]map[string]any)
	next, _ := page1["next"].(string)
	if len(list1) != 50 || next != list1[49]["id"] {
		t.Fatalf("unexpected page1 len=%d next=%q", len(list1), next)
	}
	page2 := call(t, s, "ui_threads", Request{Before: next, Limit: 50})
	list2 := page2["threads"].([]map[string]any)
	if len(list2) != 5 || page2["next"] != nil {
		t.Fatalf("unexpected page2 len=%d next=%v", len(list2), page2["next"])
	}
	var gotOrder []string
	for _, th := range append(list1, list2...) {
		gotOrder = append(gotOrder, th["id"].(string))
	}
	for i := range expectedOrder {
		if gotOrder[i] != expectedOrder[i] {
			t.Fatalf("thread order mismatch at %d: got %q want %q", i, gotOrder[i], expectedOrder[i])
		}
	}

	// Verify agent=human variant sees only thread0 with combined unread across repoA + repoB (2 unread).
	humanAll := call(t, s, "ui_threads", Request{Agent: "human", Limit: 50})["threads"].([]map[string]any)
	if len(humanAll) != 1 || humanAll[0]["id"] != thread0 {
		t.Fatalf("unexpected agent=human threads: %+v", humanAll)
	}
	t0 := humanAll[0]
	if t0["subject"] != "Initial Title" {
		t.Fatalf("expected earliest subject 'Initial Title', got %q", t0["subject"])
	}
	if t0["count"] != int64(3) || t0["unread"] != int64(2) {
		t.Fatalf("expected count=3 unread=2, got count=%v unread=%v", t0["count"], t0["unread"])
	}
	parts := t0["participants"].([]string)
	wantParts := []string{agentA, agentB, "human"}
	slices.Sort(wantParts)
	if !slices.Equal(parts, wantParts) {
		t.Fatalf("unexpected sorted participants: got %v want %v", parts, wantParts)
	}
	authors := t0["authors"].([]map[string]any)
	if len(authors) != 1 || authors[0]["sender_username"] != "Thread Author" || authors[0]["sender_display_name"] != "Display Thread Author" {
		t.Fatalf("unexpected authors: %+v", authors)
	}

	// Filtering to repoA keeps repoA's messages in thread0 (2 messages: first + reply-1, 1 unread human delivery in repoA).
	humanRepoA := call(t, s, "ui_threads", Request{RepositoryID: repoA, Agent: "human", Limit: 50})["threads"].([]map[string]any)
	if len(humanRepoA) != 1 || humanRepoA[0]["unread"] != int64(1) {
		t.Fatalf("expected repoA unread=1, got %+v", humanRepoA)
	}
	// Mark repoA read -> repoA unread becomes 0, repoB/global unread becomes 1, next ack -> 0.
	call(t, s, "ui_read", Request{Key: thread0, RepositoryID: repoA})
	humanRepoAAfter := call(t, s, "ui_threads", Request{RepositoryID: repoA, Agent: "human", Limit: 50})["threads"].([]map[string]any)
	if humanRepoAAfter[0]["unread"] != int64(0) {
		t.Fatalf("expected repoA unread=0 after ui_read, got %+v", humanRepoAAfter)
	}
	call(t, s, "ui_read", Request{Key: thread0, RepositoryID: repoB})
	humanAllAfter := call(t, s, "ui_threads", Request{Agent: "human", Limit: 50})["threads"].([]map[string]any)
	if humanAllAfter[0]["unread"] != int64(0) {
		t.Fatalf("expected global unread=0 after reading both repos, got %+v", humanAllAfter)
	}

	// Named-agent variant returns all 55 threads involving agentB (1 human reply + 54 extra messages).
	named := call(t, s, "ui_threads", Request{Agent: agentB, Limit: 100})["threads"].([]map[string]any)
	if len(named) != 55 {
		t.Fatalf("expected 55 threads for named agent %s, got %d", agentB, len(named))
	}

	// Empty search skips unread statement and returns empty slice cleanly.
	empty := call(t, s, "ui_threads", Request{Q: "no_such_search_term_xyz", Limit: 50})["threads"].([]map[string]any)
	if len(empty) != 0 {
		t.Fatalf("expected empty search result, got %+v", empty)
	}
}
