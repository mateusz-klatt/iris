package hub

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestMergeRequiresOwnerAndPreservesBindings(t *testing.T) {
	_, s := setup(t)
	a := register(t, s, 1)
	b := request(t, s, "POST", "/v1/sessions", map[string]any{"client": "claude", "session_id": "split", "host": "second", "cwd": "/repo", "remote_url": "https://example.com/repo.git", "root_commit": "rewritten"}, 200)
	from, into := b["repository_id"].(string), a["repository_id"].(string)
	body := map[string]any{"from_repository_id": from, "into_repository_id": into, "dry_run": true}
	raw, _ := json.Marshal(body)
	host := request(t, s, "POST", "/v1/tokens", map[string]string{"host": "laptop"}, 200)["token"].(string)
	for _, auth := range []struct {
		token  string
		status int
	}{{host, http.StatusForbidden}, {"", http.StatusUnauthorized}} {
		r, _ := http.NewRequest("POST", s.URL+"/v1/admin/merge", strings.NewReader(string(raw)))
		if auth.token != "" {
			r.Header.Set("Authorization", "Bearer "+auth.token)
		}
		resp, err := s.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != auth.status {
			t.Fatalf("merge authority: got %d want %d", resp.StatusCode, auth.status)
		}
	}
	dry := request(t, s, "POST", "/v1/admin/merge", body, 200)
	if dry["dry_run"] != true || dry["counts"].(map[string]any)["Execution"] != float64(1) {
		t.Fatal(dry)
	}
	before := request(t, s, "POST", "/v1/bind", map[string]any{"execution": b["execution_id"], "host": "second", "client": "claude"}, 200)
	if before["repository_id"] != from {
		t.Fatal("dry run changed binding", before)
	}
	body["dry_run"] = false
	merged := request(t, s, "POST", "/v1/admin/merge", body, 200)
	if merged["dry_run"] != false {
		t.Fatal(merged)
	}
	after := request(t, s, "POST", "/v1/bind", map[string]any{"execution": b["execution_id"], "host": "second", "client": "claude"}, 200)
	if after["repository_id"] != into || after["execution_id"] != b["execution_id"] || after["generation"] != b["generation"] || after["agent"].(map[string]any)["id"] != b["agent"].(map[string]any)["id"] {
		t.Fatal("merge lost binding", after)
	}
	request(t, s, "POST", "/v1/admin/merge", body, 404)
	request(t, s, "POST", "/v1/admin/merge", "not an object", 400)
}
