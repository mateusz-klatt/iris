package hub

import (
	"net/http"
	"strings"
	"testing"
)

// A session outside git registers on its own route and takes no claim, and only the owner moves a
// folder project into its host's context (ADR 034).
func TestSessionsOutsideGitHaveTheirRoutes(t *testing.T) {
	_, s := setup(t)
	outside := request(t, s, "POST", "/v1/sessions/outside", map[string]any{"client": "codex", "session_id": "o", "host": "home", "cwd": "/home/u/tmp", "source": "hook"}, 200)
	if outside["outside_git"] != true {
		t.Fatal(outside)
	}
	refused := request(t, s, "POST", "/v1/sessions/outside", map[string]any{"client": "codex", "session_id": "c", "host": "home", "cwd": "/r", "worktree": "/r"}, http.StatusBadRequest)
	if refused["error"].(map[string]any)["code"] != "bad_request" {
		t.Fatal(refused)
	}
	touch := request(t, s, "POST", "/v1/claims/touch", map[string]any{"execution_id": outside["execution_id"], "generation": outside["generation"], "tool_call_id": "t", "paths": []string{"x"}}, http.StatusBadRequest)
	if touch["error"].(map[string]any)["code"] != "outside_git" {
		t.Fatal(touch)
	}
	folder := request(t, s, "POST", "/v1/sessions", map[string]any{"client": "codex", "session_id": "f", "host": "home", "cwd": "/home/u/scripts", "worktree": "/home/u/scripts", "source": "mcp"}, 200)
	request(t, s, "POST", "/v1/executions/"+folder["execution_id"].(string)+"/end", map[string]any{"generation": folder["generation"]}, 200)
	body := `{"from_repository_id":"` + folder["repository_id"].(string) + `","into_repository_id":"` + outside["repository_id"].(string) + `","dry_run":true}`
	host := request(t, s, "POST", "/v1/tokens", map[string]string{"host": "laptop"}, 200)["token"].(string)
	r, _ := http.NewRequest("POST", s.URL+"/v1/admin/merge/outside", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+host)
	resp, err := s.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("a host token repaired: %d", resp.StatusCode)
	}
	plan := request(t, s, "POST", "/v1/admin/merge/outside", map[string]any{"from_repository_id": folder["repository_id"], "into_repository_id": outside["repository_id"], "dry_run": true}, 200)
	if plan["outside"] != true || plan["dry_run"] != true {
		t.Fatal(plan)
	}
}
