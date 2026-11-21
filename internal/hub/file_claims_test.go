package hub

import "testing"

// A grouped touch has a route of its own (ADR 033), so a hook can tell an older hub, which
// refuses the route, from a newer one, and one tool call's paths in two checkouts are answered
// in one transaction.
func TestAGroupedTouchHasARouteOfItsOwn(t *testing.T) {
	_, s := setup(t)
	a, b := register(t, s, 1), register(t, s, 2)
	r := authority(a)
	r["tool_call_id"] = "grouped"
	r["groups"] = []map[string]any{
		{"worktree": "/repo/1", "remote_url": "https://example.com/repo.git", "root_commit": "root", "paths": []string{"src/a.go"}},
		{"worktree": "/repo/1/frontend", "common_dir": "/repo/1/.git/modules/frontend", "remote_url": "https://example.com/team/frontend.git", "root_commit": "frontend-root", "paths": []string{"src/x.ts"}},
	}
	out := request(t, s, "POST", "/v1/claims/touch/files", r, 200)
	granted := out["granted"].([]any)
	if out["action"] != "allow" || len(granted) != 2 {
		t.Fatalf("answer: %v", out)
	}
	own, frontend := granted[0].(map[string]any)["repository_id"], granted[1].(map[string]any)["repository_id"]
	if own != a["repository_id"] || frontend == own || frontend == "" {
		t.Fatalf("claimed in %v and %v, the session's repository is %v", own, frontend, a["repository_id"])
	}
	other := authority(b)
	other["tool_call_id"] = "other"
	other["groups"] = []map[string]any{{"worktree": "/elsewhere/frontend", "remote_url": "https://example.com/team/frontend.git", "root_commit": "frontend-root", "paths": []string{"src/x.ts"}}}
	if refused := request(t, s, "POST", "/v1/claims/touch/files", other, 200); refused["action"] != "deny" {
		t.Fatalf("another checkout of the submodule was not refused: %v", refused)
	}
}

// An explicit claim on another checkout's path has a route of its own too, and a session's binding
// names its worktree and the hub's capabilities.
func TestAnExplicitFileClaimHasARouteOfItsOwn(t *testing.T) {
	_, s := setup(t)
	a := register(t, s, 1)
	bound := request(t, s, "POST", "/v1/bind", map[string]any{"execution": a["execution_id"], "client": "codex", "host": "host"}, 200)
	if bound["worktree"] != "/repo/1" || len(bound["capabilities"].([]any)) != 2 {
		t.Fatalf("binding: %v", bound)
	}
	r := authority(a)
	r["key"] = "planned"
	r["kind"] = "prefix"
	r["path"] = "frontend/src"
	r["checkout"] = map[string]any{"worktree": "/repo/1/frontend", "remote_url": "https://example.com/team/frontend.git", "root_commit": "frontend-root", "prefix": "frontend"}
	if out := request(t, s, "POST", "/v1/claims/files", r, 200); out["fence"] == nil {
		t.Fatalf("answer: %v", out)
	}
}
