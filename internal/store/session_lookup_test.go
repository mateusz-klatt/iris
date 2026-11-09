package store

import (
	"strings"
	"testing"
)

// Registration returns every peer while holding the one store transaction. Looking
// up a peer's latest Execution must seek by agent, rather than walk all retained
// executions for every peer. Check SQLite's actual plan instead of a wall-clock
// threshold, which would depend on the host and race instrumentation.
func TestSessionPeersSeekEachAgentsLatestExecution(t *testing.T) {
	for name, reopen := range map[string]bool{"fresh": false, "existing": true} {
		t.Run(name, func(t *testing.T) { sessionPeerLookup(t, reopen) })
	}
}

func sessionPeerLookup(t *testing.T, reopen bool) {
	t.Helper()
	s, _, file := testStore(t)
	_, _ = session(t, s, 1)
	second, previous := session(t, s, 2)
	call(t, s, "end", second)
	req := Request{Client: "codex", Host: "host", SessionID: "replacement-session", Cwd: "/repo/w2", Worktree: "/repo/w2", Branch: "replacement-branch", RemoteURL: "https://example.com/team/project.git", RootCommit: "root"}
	replacement := call(t, s, "sessions", req)
	if replacement["agent"].(map[string]any)["id"] != previous["agent"].(map[string]any)["id"] {
		t.Fatal("the replacement did not reuse the ended session's mailbox")
	}
	if reopen {
		// The idempotent base schema is applied even to a current-version database.
		if _, err := s.db.Exec("DROP INDEX IF EXISTS execution_agent"); err != nil {
			t.Fatal(err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		var err error
		s, err = Open(file, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
	}

	fault := installFaultDriver(t, s, file)
	req.SessionID, req.Cwd, req.Worktree = "1", "/repo/w1", "/repo/w1"
	req.Generation = 1
	registered := call(t, s, "sessions", req)
	peers := registered["peers"].([]map[string]any)
	if len(peers) != 2 {
		t.Fatalf("peers: %v", peers)
	}
	seen := false
	for _, peer := range peers {
		if peer["id"] == replacement["agent"].(map[string]any)["id"] {
			seen = true
			if peer["branch"] != "replacement-branch" || peer["state"] != "active" {
				t.Fatalf("peer did not describe its latest execution: %v", peer)
			}
		}
	}
	if !seen {
		t.Fatalf("replacement's mailbox missing from peers: %v", peers)
	}
	// Observe the query registration really ran, including any future rewrite.
	var query string
	for _, q := range fault.trace {
		if strings.HasPrefix(q, "SELECT a.id,a.name,a.client,a.worktree,") {
			query = q
		}
	}
	if query == "" {
		t.Fatal("registration did not load its peers")
	}
	rows, err := s.db.Query("EXPLAIN QUERY PLAN "+query, registered["repository_id"])
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	seek := false
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
		seek = seek || (strings.Contains(detail, "SEARCH Execution ") && strings.Contains(detail, "(agent_id=?)"))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !seek {
		t.Fatalf("latest-execution lookup scans retained history per peer: %v", plan)
	}
}
