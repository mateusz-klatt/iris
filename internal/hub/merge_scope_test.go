package hub

import (
	"net/http"
	"net/url"
	"testing"
)

func TestMergedRepositoryReadsUseTheOldClientsExecution(t *testing.T) {
	_, s := setup(t)
	a := register(t, s, 1)
	b := request(t, s, "POST", "/v1/sessions", map[string]any{"client": "claude", "session_id": "split", "host": "second", "cwd": "/repo", "remote_url": "https://example.com/repo.git", "root_commit": "rewritten"}, http.StatusOK)
	c := request(t, s, "POST", "/v1/sessions", map[string]any{"client": "codex", "session_id": "unrelated", "host": "third", "cwd": "/elsewhere", "remote_url": "https://other.example/project.git", "root_commit": "unrelated"}, http.StatusOK)
	from, into := b["repository_id"].(string), a["repository_id"].(string)
	for path, who := range map[string]map[string]any{"destination.go": a, "source.go": b} {
		claim := authority(who)
		claim["key"], claim["kind"], claim["path"] = path, "exact", path
		request(t, s, "POST", "/v1/claims", claim, http.StatusOK)
	}
	// Existing MCP clients already send both fields, but cache the old repository
	// id. Reuse these exact query strings after the repair without a client bind.
	query := url.Values{"execution_id": {b["execution_id"].(string)}, "repository_id": {from}}
	peersPath, claimsPath := "/v1/agents?"+query.Encode(), "/v1/claims?"+query.Encode()
	if peers := request(t, s, "GET", peersPath, nil, http.StatusOK)["agents"].([]any); len(peers) != 1 || peers[0].(map[string]any)["id"] != b["agent"].(map[string]any)["id"] {
		t.Fatal("the pre-merge client did not see its source repository", peers)
	}
	if claims := request(t, s, "GET", claimsPath, nil, http.StatusOK)["claims"].([]any); len(claims) != 1 || claims[0].(map[string]any)["path"] != "source.go" {
		t.Fatal("the pre-merge client did not see its source claim", claims)
	}
	request(t, s, "POST", "/v1/admin/merge", map[string]any{"from_repository_id": from, "into_repository_id": into}, http.StatusOK)
	peers := request(t, s, "GET", peersPath, nil, http.StatusOK)["agents"].([]any)
	want := map[any]bool{a["agent"].(map[string]any)["id"]: true, b["agent"].(map[string]any)["id"]: true}
	if len(peers) != len(want) {
		t.Fatal("the old client's unchanged query did not see both hosts", peers)
	}
	for _, peer := range peers {
		id := peer.(map[string]any)["id"]
		if !want[id] {
			t.Fatal("the old client's local peer query included another repository", peer)
		}
		delete(want, id)
	}
	claims := request(t, s, "GET", claimsPath, nil, http.StatusOK)["claims"].([]any)
	paths := map[any]bool{}
	for _, claim := range claims {
		paths[claim.(map[string]any)["path"]] = true
	}
	if len(claims) != 2 || !paths["destination.go"] || !paths["source.go"] {
		t.Fatal("the old client's unchanged query missed merged claims", claims)
	}
	for _, endpoint := range []string{"agents", "claims"} {
		invalid := url.Values{"execution_id": {"missing"}, "repository_id": {into}}
		refused := request(t, s, "GET", "/v1/"+endpoint+"?"+invalid.Encode(), nil, http.StatusConflict)
		if refused["error"].(map[string]any)["code"] != "lost" {
			t.Fatal("an invalid Execution fell back to a repository id", refused)
		}
		bare := request(t, s, "GET", "/v1/"+endpoint+"?repository_id="+url.QueryEscape(from), nil, http.StatusOK)[endpoint].([]any)
		if len(bare) != 0 {
			t.Fatal("a retired explicit repository id was redirected", bare)
		}
	}
	query.Set("all", "1")
	all := request(t, s, "GET", "/v1/agents?"+query.Encode(), nil, http.StatusOK)["agents"].([]any)
	if len(all) != 3 {
		t.Fatal("all=true stopped being a global listing", all)
	}
	foundUnrelated := false
	for _, peer := range all {
		if peer.(map[string]any)["id"] == c["agent"].(map[string]any)["id"] {
			foundUnrelated = true
		}
	}
	if !foundUnrelated {
		t.Fatal("the global listing omitted the unrelated repository", all)
	}
}
