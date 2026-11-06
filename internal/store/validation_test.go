package store

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func TestInvalidOperationsLeaveStateUnchanged(t *testing.T) {
	s, _, _ := testStore(t)
	a, ar := session(t, s, 1)
	for _, tc := range []struct {
		op   string
		r    Request
		code string
	}{
		{"unknown", Request{}, "not_found"}, {"claim", Request{}, "bad_request"}, {"events", Request{ExecutionID: a.ExecutionID, After: -1}, "bad_request"},
		{"check", Request{}, "bad_request"}, {"check", Request{Host: "host", Worktree: "/repo/w1", Paths: []string{"../bad"}}, "bad_request"},
		{"touch", a, "bad_request"}, {"touch", touchRequest(a, "empty"), "bad_request"}, {"touch", touchRequest(a, "invalid", "../bad"), "bad_request"},
		{"claim", claimRequest(a, "kind", "file", "glob"), "bad_request"}, {"release", Request{ExecutionID: a.ExecutionID, Generation: 1, Fence: 99999}, "conflict"},
		{"messages", Request{ExecutionID: a.ExecutionID, Generation: 1, Key: "empty"}, "bad_request"},
		{"messages", Request{ExecutionID: a.ExecutionID, Generation: 1, Key: "huge", Body: strings.Repeat("x", 16385)}, "too_large"},
		{"reconcile", a, "bad_request"}, {"reconcile", Request{ExecutionID: a.ExecutionID, Generation: 1, Kind: "before", Status: map[string]string{"../bad": " M"}}, "bad_request"},
		{"sessions", Request{}, "bad_request"}, {"sessions", Request{Client: "codex", SessionID: "new", Host: "host", Cwd: "/new", Generation: 9}, "lost"},
		{"sessions", Request{Client: "codex", SessionID: "1", Host: "host", Cwd: "/repo/w1", Generation: 9}, "lost"},
		{"sessions", Request{Client: "codex", SessionID: "child", Host: "host", Cwd: "/repo/w1", ParentSessionID: "missing"}, "bad_request"},
		{"validate", Request{ExecutionID: "missing"}, "lost"}, {"bind", Request{Client: "codex", Host: "host", Execution: "missing"}, "lost"},
		{"bind", Request{Client: "wrong", Host: "host", Execution: a.ExecutionID}, "unbound"}, {"bind", Request{Client: "codex", Host: "host"}, "unbound"},
		{"heartbeat", Request{ExecutionID: a.ExecutionID, Generation: 1, Source: "bad"}, "bad_request"}, {"end", Request{ExecutionID: a.ExecutionID, Generation: 9}, "lost"},
	} {
		before := databaseState(t, s)
		expectError(t, s, tc.op, tc.r, tc.code)
		if after := databaseState(t, s); before != after {
			t.Fatalf("invalid %s changed state", tc.op)
		}
	}
	repo := ar["repository_id"].(string)
	for _, policy := range []struct{ casePolicy, action string }{{"bad", "deny"}, {"sensitive", "bad"}} {
		if s.SetRepositoryPolicy(context.Background(), repo, policy.casePolicy, policy.action) == nil {
			t.Fatal("invalid policy accepted")
		}
	}
	if s.SetRepositoryPolicy(context.Background(), "missing", "sensitive", "deny") == nil {
		t.Fatal("unknown repository policy accepted")
	}
	call(t, s, "claim", claimRequest(a, "held", "file", "exact"))
	if s.SetRepositoryPolicy(context.Background(), repo, "folded", "deny") == nil {
		t.Fatal("namespace folded after claims existed")
	}
	r := claimRequest(a, "ttl", "other", "exact")
	r.TTLS = -1
	expectError(t, s, "claim", r, "bad_request")
	r.Key = "timed"
	r.TTLS = 60
	out := call(t, s, "claim", r)
	if out["expires_at"] == nil {
		t.Fatal("TTL omitted")
	}
	if got := call(t, s, "bind", Request{Client: "codex", Host: "host", Execution: a.ExecutionID})["execution_id"]; got != a.ExecutionID {
		t.Fatal(got)
	}
	if namePart(strings.Repeat("x", 80)) != strings.Repeat("x", 64) || namePart("...") != "unknown" {
		t.Fatal("unsafe display name")
	}
	if remoteAlias("ftp://example.com/repo") != "" {
		t.Fatal("unsupported remote accepted")
	}
	if boundedLimit(200, 20, 100) != 100 {
		t.Fatal("unbounded inbox limit")
	}
	if _, err := s.transactHeartbeatWithoutGeneration(a); err == nil {
		t.Fatal("missing heartbeat generation accepted")
	}
}
func (s *Store) transactHeartbeatWithoutGeneration(r Request) (map[string]any, error) {
	r.Generation = 0
	var out map[string]any
	err := s.transact(context.Background(), func(tx *transaction) error { var err error; out, err = tx.heartbeat(r); return err })
	return out, err
}
func TestTouchDeduplicatesAndAllowsFamily(t *testing.T) {
	s, _, _ := testStore(t)
	a, _ := session(t, s, 1)
	call(t, s, "claim", claimRequest(a, "parent", "shared", "exact"))
	child := call(t, s, "sessions", Request{Client: "codex", SessionID: "child", Host: "host", Cwd: "/repo/w1", ParentSessionID: "1"})
	r := Request{ExecutionID: child["execution_id"].(string), Generation: 1, ToolCallID: "family", Paths: []string{"shared", "shared"}}
	if out := call(t, s, "touch", r); out["action"] != "allow" || len(out["granted"].([]string)) != 1 {
		t.Fatal(out)
	}
	snap := a
	snap.Kind = "before"
	snap.Status = map[string]string{"removed": " M"}
	call(t, s, "reconcile", snap)
	snap.Kind = "after"
	snap.Status = map[string]string{}
	if out := call(t, s, "reconcile", snap); out["message"] != "" {
		t.Fatal(out)
	}
	// A strong shim registration shortens the lease, including reattachment.
	strong := call(t, s, "sessions", Request{Client: "codex", SessionID: "strong", Host: "host", Cwd: "/strong", Source: "mcp"})
	if strong["lease_ms"] != int64(time.Minute/time.Millisecond) {
		t.Fatal(strong)
	}
	strong = call(t, s, "sessions", Request{Client: "codex", SessionID: "strong", Host: "host", Cwd: "/strong", Source: "shim"})
	if strong["lease_ms"] != int64(60000) {
		t.Fatal(strong)
	}
}

func TestClaimExpiryAndRepositoryDisplayCollisions(t *testing.T) {
	s, _, _ := testStore(t)
	a, ar := session(t, s, 1)
	b, _ := session(t, s, 2)
	r := claimRequest(a, "brief", "shared", "exact")
	r.TTLS = 1
	call(t, s, "claim", r)
	out := call(t, s, "touch", touchRequest(b, "collision", "shared"))
	conflicts := out["conflicts"].([]map[string]any)
	if out["action"] != "deny" || conflicts[0]["expires_at"] == nil {
		t.Fatal(out)
	}
	other := call(t, s, "sessions", Request{Client: "codex", Host: "host", SessionID: "other", Cwd: "/different/w1", RemoteURL: "https://different.example/other.git"})
	firstName := ar["agent"].(map[string]any)["name"].(string)
	secondName := other["agent"].(map[string]any)["name"].(string)
	if firstName == secondName || !strings.HasPrefix(secondName, firstName+"-") {
		t.Fatal("mailbox name collision", firstName, secondName)
	}
	snap := a
	snap.Kind = "before"
	snap.Status = map[string]string{"removed": " M"}
	call(t, s, "reconcile", snap)
	snap.Kind = "after"
	snap.Status = map[string]string{}
	if out := call(t, s, "reconcile", snap); out["message"] != "" {
		t.Fatal(out)
	}
	r = claimRequest(a, "", "new", "exact")
	expectError(t, s, "claim", r, "bad_request")
}

func TestDatabaseParentDisappearingFailsSafely(t *testing.T) {
	calls := 0
	_, err := resolveDatabasePath(t.TempDir()+"/new.db", func(string) (string, error) {
		calls++
		if calls == 1 {
			return "", os.ErrNotExist
		}
		return "", os.ErrPermission
	})
	if !errors.Is(err, os.ErrPermission) || calls != 2 {
		t.Fatal(calls, err)
	}
}
