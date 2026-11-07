package store

import (
	"errors"
	"strings"
	"testing"
)

// ended registers a session in its own repository and ends it, as a measurement run would.
func ended(t *testing.T, s *Store, n string) (Request, map[string]any) {
	t.Helper()
	out := call(t, s, "sessions", Request{Client: "codex", Host: "probe", SessionID: n, Cwd: "/" + n, RemoteURL: "https://" + n + ".example/r.git"})
	r := Request{ExecutionID: out["execution_id"].(string), Generation: out["generation"].(int64)}
	call(t, s, "end", r)
	return r, out
}

func count(t *testing.T, s *Store, q string, args ...any) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestForgetRemovesARepositoryAndEverythingInIt(t *testing.T) {
	s, _, _ := testStore(t)
	keep, _ := session(t, s, 1)
	_, gone := ended(t, s, "probe")
	repo := gone["repository_id"].(string)
	out := call(t, s, "forget", Request{RepositoryID: repo})
	counts := out["counts"].(map[string]int)
	if counts["Repository"] != 1 || counts["Agent"] != 1 || counts["Execution"] != 1 || out["dry_run"] != false {
		t.Fatal(out)
	}
	if n := count(t, s, "SELECT COUNT(*) FROM Repository WHERE id=?", repo); n != 0 {
		t.Fatal("repository kept")
	}
	if n := count(t, s, "SELECT COUNT(*) FROM RepositoryAlias WHERE repository_id=?", repo); n != 0 {
		t.Fatal("aliases kept")
	}
	// The other repository is untouched and still works.
	call(t, s, "claim", claimRequest(keep, "after", "file", "exact"))
}

func TestForgetDryRunChangesNothing(t *testing.T) {
	s, _, _ := testStore(t)
	_, gone := ended(t, s, "probe")
	before := count(t, s, "SELECT COUNT(*) FROM Execution")
	out := call(t, s, "forget", Request{RepositoryID: gone["repository_id"].(string), DryRun: true})
	if out["dry_run"] != true || out["counts"].(map[string]int)["Execution"] != 1 {
		t.Fatal(out)
	}
	if count(t, s, "SELECT COUNT(*) FROM Execution") != before {
		t.Fatal("a dry run removed something")
	}
}

func TestForgetAnAgentTakesItsMailBothWays(t *testing.T) {
	s, _, _ := testStore(t)
	a, ar := session(t, s, 1)
	b, br := session(t, s, 2)
	aName := ar["agent"].(map[string]any)["name"].(string)
	bName := br["agent"].(map[string]any)["name"].(string)
	call(t, s, "messages", Request{ExecutionID: a.ExecutionID, Generation: a.Generation, Key: "to-b", To: []string{bName}, Subject: "findme", Body: "to b"})
	call(t, s, "messages", Request{ExecutionID: b.ExecutionID, Generation: b.Generation, Key: "to-a", To: []string{aName}, Subject: "findme", Body: "from b"})
	call(t, s, "end", b)
	out := call(t, s, "forget", Request{Agent: bName})
	counts := out["counts"].(map[string]int)
	if counts["Agent"] != 1 || counts["Message"] != 1 || counts["Delivery"] != 2 {
		t.Fatal(out)
	}
	// a's inbox loses b's message, and a still exists with its own sent message.
	if got := call(t, s, "inbox", a)["deliveries"].([]map[string]any); len(got) != 0 {
		t.Fatal(got)
	}
	if count(t, s, "SELECT COUNT(*) FROM Message") != 1 {
		t.Fatal("a's own message went too")
	}
	// The search index lost exactly the removed message.
	if n := count(t, s, "SELECT COUNT(*) FROM MessageSearch WHERE MessageSearch MATCH 'findme'"); n != 1 {
		t.Fatalf("search index holds %d", n)
	}
	if _, err := s.db.Exec("INSERT INTO MessageSearch(MessageSearch) VALUES('integrity-check')"); err != nil {
		t.Fatal("search index inconsistent:", err)
	}
	// a's event about the delivery that is gone no longer asks for attention.
	events := call(t, s, "events", a)["events"].([]map[string]any)
	if len(events) != 1 || events[0]["settled"] != true {
		t.Fatal(events)
	}
}

func TestForgetDetachesAnotherAgentsSubagent(t *testing.T) {
	s, _, _ := testStore(t)
	parent, _ := ended(t, s, "parent")
	child, _ := session(t, s, 1)
	if _, err := s.db.Exec("UPDATE Execution SET parent_id=? WHERE id=?", parent.ExecutionID, child.ExecutionID); err != nil {
		t.Fatal(err)
	}
	var repo string
	if err := s.db.QueryRow("SELECT repository_id FROM Execution WHERE id=?", parent.ExecutionID).Scan(&repo); err != nil {
		t.Fatal(err)
	}
	out := call(t, s, "forget", Request{RepositoryID: repo})
	if out["detached_children"] != 1 {
		t.Fatal(out)
	}
	if n := count(t, s, "SELECT COUNT(*) FROM Execution WHERE id=? AND parent_id IS NULL", child.ExecutionID); n != 1 {
		t.Fatal("child kept a dangling parent")
	}
}

func TestForgetRefusesWhatIsInUse(t *testing.T) {
	s, _, _ := testStore(t)
	a, ar := session(t, s, 1)
	name := ar["agent"].(map[string]any)["name"].(string)
	expectError(t, s, "forget", Request{RepositoryID: ar["repository_id"].(string)}, "conflict")
	call(t, s, "claim", claimRequest(a, "held", "file", "exact"))
	_, err := s.Call(t.Context(), "forget", Request{Agent: name})
	var e *Error
	if !errors.As(err, &e) || e.Code != "conflict" || !strings.Contains(e.Message, "claims") {
		t.Fatal(err)
	}
	if d := e.Details.(map[string]any); d["active_executions"] != 1 || d["live_claims"] != 1 {
		t.Fatal(d)
	}
	// Nothing was removed by the refusal.
	if count(t, s, "SELECT COUNT(*) FROM Agent WHERE name=?", name) != 1 {
		t.Fatal("refused, but removed")
	}
}

func TestForgetNeedsExactlyOneKnownTarget(t *testing.T) {
	s, _, _ := testStore(t)
	expectError(t, s, "forget", Request{}, "bad_request")
	expectError(t, s, "forget", Request{RepositoryID: "r", Agent: "a"}, "bad_request")
	expectError(t, s, "forget", Request{RepositoryID: "missing"}, "not_found")
	expectError(t, s, "forget", Request{Agent: "missing"}, "not_found")
}
