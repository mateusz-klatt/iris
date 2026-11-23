package hubclient

import (
	"encoding/json"
	"testing"
)

// An older guard decodes only message. An advisory-only body leaves that empty,
// so the old guard allows the commit. A refusal stays in message either way.
func TestAnOlderGuardIgnoresAdvisories(t *testing.T) {
	body := []byte(`{"message":"","advisories":[{"path":"a.go","holder":"codex","worktree":"/wt","branch":"main","repository_id":"rp","fence":7}]}`)
	var old struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &old); err != nil || old.Message != "" {
		t.Fatal(old, err)
	}
	var now CheckResponse
	if err := json.Unmarshal(body, &now); err != nil || now.Message != "" || len(now.Advisories) != 1 || now.Advisories[0].Fence != 7 || now.Advisories[0].Path != "a.go" {
		t.Fatal(now, err)
	}
	refused := []byte(`{"message":"held","advisories":[{"path":"b.go","holder":"codex"}]}`)
	if err := json.Unmarshal(refused, &old); err != nil || old.Message != "held" {
		t.Fatal(old, err)
	}
	if err := json.Unmarshal(refused, &now); err != nil || now.Message != "held" || len(now.Advisories) != 1 {
		t.Fatal(now, err)
	}
}
