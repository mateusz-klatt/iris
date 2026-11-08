package store

import "testing"

func TestMergedExecutionDeterminesReadScope(t *testing.T) {
	s, _, _ := testStore(t)
	a, ar, b, br := mergeSplit(t, s)
	call(t, s, "claim", claimRequest(a, "destination", "destination.go", "exact"))
	call(t, s, "claim", claimRequest(b, "source", "source.go", "exact"))
	from, into := br["repository_id"].(string), ar["repository_id"].(string)
	call(t, s, "merge", mergeRequest(from, into, false))
	// Old MCP clients send their cached repository id together with the stable
	// Execution handle. The handle must win even before a heartbeat or rebind.
	stale := b
	stale.RepositoryID = from
	for _, op := range []string{"agents", "claims"} {
		if rows := call(t, s, op, stale)[op].([]map[string]any); len(rows) != 2 {
			t.Fatalf("%s queried the retired repository: %v", op, rows)
		}
		// A supplied unknown or wrong-generation Execution is a loss, never a
		// reason to fall back to the caller's repository field.
		invalid := stale
		invalid.ExecutionID = "ex_missing"
		expectError(t, s, op, invalid, "lost")
		invalid = stale
		invalid.Generation++
		expectError(t, s, op, invalid, "lost")
		// Repository-only operator reads retain their explicit scope.
		if rows := call(t, s, op, Request{RepositoryID: from})[op].([]map[string]any); len(rows) != 0 {
			t.Fatalf("%s redirected an explicit operator scope: %v", op, rows)
		}
	}
}

func TestHeartbeatReturnsCurrentRepositoryBinding(t *testing.T) {
	s, _, _ := testStore(t)
	_, ar, b, br := mergeSplit(t, s)
	call(t, s, "merge", mergeRequest(br["repository_id"].(string), ar["repository_id"].(string), false))
	b.Source = "shim"
	out := call(t, s, "heartbeat", b)
	if out["repository_id"] != ar["repository_id"] || out["execution_id"] != b.ExecutionID || out["generation"] != b.Generation || out["agent"].(map[string]any)["id"] != br["agent"].(map[string]any)["id"] || out["lease_ms"] != int64(60000) {
		t.Fatal("heartbeat omitted authoritative binding", out)
	}
}
