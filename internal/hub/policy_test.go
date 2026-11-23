package hub

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// Only the hub's own token can set claim.auto_scope. A host token and a missing
// token are refused, and the route rejects a value outside the two settings.
func TestPolicyRequiresOwner(t *testing.T) {
	_, s := setup(t)
	registered := register(t, s, 1)
	repo := registered["repository_id"].(string)
	body := map[string]any{"repository_id": repo, "claim_auto_scope": "worktree"}
	raw, _ := json.Marshal(body)
	host := request(t, s, "POST", "/v1/tokens", map[string]string{"host": "laptop"}, 200)["token"].(string)
	for _, auth := range []struct {
		token  string
		status int
	}{{host, http.StatusForbidden}, {"", http.StatusUnauthorized}} {
		r, _ := http.NewRequest("POST", s.URL+"/v1/admin/policy", strings.NewReader(string(raw)))
		if auth.token != "" {
			r.Header.Set("Authorization", "Bearer "+auth.token)
		}
		resp, err := s.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != auth.status {
			t.Fatalf("policy authority: got %d want %d", resp.StatusCode, auth.status)
		}
	}
	set := request(t, s, "POST", "/v1/admin/policy", body, 200)
	if set["repository_id"] != repo || set["claim_auto_scope"] != "worktree" {
		t.Fatal(set)
	}
	body["claim_auto_scope"] = "repository"
	back := request(t, s, "POST", "/v1/admin/policy", body, 200)
	if back["claim_auto_scope"] != "repository" {
		t.Fatal(back)
	}
	body["claim_auto_scope"] = "seat"
	request(t, s, "POST", "/v1/admin/policy", body, 400)
	request(t, s, "POST", "/v1/admin/policy", map[string]any{"repository_id": "rp_missing", "claim_auto_scope": "worktree"}, 404)
	request(t, s, "POST", "/v1/admin/policy", "not an object", 400)
	// A session that carries the field is not an administration route and cannot
	// select the policy. The owner route above is what changed it, and it is back
	// at repository; this registration must still be accepted.
	joined := request(t, s, "POST", "/v1/sessions", map[string]any{"client": "codex", "session_id": "payload", "host": "host", "cwd": "/repo/1", "worktree": "/repo/1", "remote_url": "https://example.com/repo.git", "root_commit": "root", "claim_auto_scope": "worktree"}, 200)
	if joined["repository_id"] != repo {
		t.Fatal("a session payload split the repository", joined)
	}
}
