package store

import (
	"encoding/json"
	"reflect"
	"testing"
)

// The editor needs one authorized snapshot of revisions and grants. A missing
// projection must never look like an instruction to remove someone's access.
func TestConsoleAccessEditorSnapshotsAndConflictPreserveGrants(t *testing.T) {
	s, _, _ := testStore(t)
	_, first := session(t, s, 1)
	_, second := thirdReadRepository(t, s, "access-editor-second")
	repoA, repoB := first["repository_id"].(string), second["repository_id"].(string)
	op := createTestConsoleAccount(t, s, "Operator", "operator")
	member := createTestConsoleAccount(t, s, "Member", "member")
	p := consoleTestPrincipal(t, s, op)

	snapshot := func() map[string]any {
		t.Helper()
		accounts, err := s.ListConsoleAccounts(t.Context(), p)
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(accounts)
		if err != nil {
			t.Fatal(err)
		}
		var projection []map[string]any
		if err = json.Unmarshal(encoded, &projection); err != nil {
			t.Fatal(err)
		}
		for _, account := range projection {
			if account["id"] == member.ID {
				return account
			}
		}
		t.Fatal("operator snapshot omitted member")
		return nil
	}
	initial := snapshot()
	if grants, ok := initial["grants"].([]any); !ok || len(grants) != 0 {
		t.Fatal("new member must explicitly project an empty grant list")
	}
	grants := []ConsoleGrant{{repoA, "ro"}, {repoB, "rw"}}
	updated, err := s.ChangeConsoleAccess(t.Context(), p, member.ID, member.EditRevision, "member", false, grants)
	if err != nil {
		t.Fatal(err)
	}
	if updated.PasswordHash != "" || updated.LoginKey != "" || len(updated.Grants) != 2 {
		t.Fatal("saved editor snapshot omitted grants or retained credential material")
	}
	after := snapshot()
	want := map[string]string{repoA: "ro", repoB: "rw"}
	got := map[string]string{}
	for _, value := range after["grants"].([]any) {
		grant := value.(map[string]any)
		got[grant["repository_id"].(string)] = grant["mode"].(string)
	}
	if !reflect.DeepEqual(got, want) || after["edit_revision"] != float64(updated.EditRevision) {
		t.Fatal("editor snapshot lost current modes or their revision")
	}
	// A second browser's stale form cannot clear a grant saved by the first.
	_, err = s.ChangeConsoleAccess(t.Context(), p, member.ID, member.EditRevision, "member", true, nil)
	consoleError(t, err, "conflict")
	if !reflect.DeepEqual(snapshot(), after) {
		t.Fatal("stale access edit changed the current projection")
	}
	// A repository forgotten while the form was open is a conflict the editor can reload from.
	_, err = s.ChangeConsoleAccess(t.Context(), p, member.ID, updated.EditRevision, "member", false, []ConsoleGrant{{repoA, "ro"}, {"rp_forgotten", "rw"}})
	consoleError(t, err, "conflict")
	if !reflect.DeepEqual(snapshot(), after) {
		t.Fatal("a refused grant changed the current projection")
	}
	_, err = s.ChangeConsoleAccess(t.Context(), p, member.ID, updated.EditRevision, "none", false, nil)
	if err != nil {
		t.Fatal(err)
	}
	quarantined := snapshot()
	if quarantined["disabled"] != true || quarantined["role"] != "none" || len(quarantined["grants"].([]any)) != 0 {
		t.Fatal("none must be disabled and retain no repository grants")
	}
}
