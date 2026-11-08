package hub

import (
	"net/http"
	"strings"
	"testing"
)

// Only the owner chooses who a repository's repo: mail reaches: a host token could
// otherwise send another repository's reports to itself (ADR 004).
func TestContactIsForTheOwnerOnly(t *testing.T) {
	_, s := setup(t)
	agent := register(t, s, 1)
	repo := agent["repository_id"].(string)
	name := agent["agent"].(map[string]any)["name"].(string)
	host := request(t, s, "POST", "/v1/tokens", map[string]string{"host": "laptop"}, 200)["token"].(string)
	r, _ := http.NewRequest("POST", s.URL+"/v1/admin/contact", strings.NewReader(`{"repository_id":"`+repo+`","agent":"`+name+`"}`))
	r.Header.Set("Authorization", "Bearer "+host)
	resp, err := s.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatal(resp.StatusCode)
	}
	out := request(t, s, "POST", "/v1/admin/contact", map[string]any{"repository_id": repo, "agent": name}, 200)
	if out["contact"] != name {
		t.Fatal(out)
	}
	request(t, s, "POST", "/v1/admin/contact", map[string]any{"repository_id": "rp_none", "agent": name}, 404)
	request(t, s, "POST", "/v1/admin/contact", "not an object", 400)
}

// peers can list every repository's agents when asked.
func TestAgentsAcrossRepositories(t *testing.T) {
	_, s := setup(t)
	a := register(t, s, 1)
	all := request(t, s, "GET", "/v1/agents?repository_id="+a["repository_id"].(string)+"&all=1", nil, 200)["agents"].([]any)
	if len(all) == 0 || all[0].(map[string]any)["repository_id"] == nil {
		t.Fatal(all)
	}
}
