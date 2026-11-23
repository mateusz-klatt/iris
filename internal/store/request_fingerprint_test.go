package store

import (
	"crypto/sha256"
	"encoding/json"
	"testing"
)

// A v0.1.13 grouped refusal hashed the checkout path and no branch. After the
// upgrade the hook sends both, and a branch can change between deliveries.
// Either difference used to be key_reused, which the hook fails open, so the
// refusal became an allow. Display metadata must not change the cached decision,
// and the hash v0.1.13 stored must still hit.
func TestAGroupedRefusalStaysCachedWhenDisplayMetadataChanges(t *testing.T) {
	s, _, _ := testStore(t)
	holder, _ := session(t, s, 1)
	editor, _ := session(t, s, 2)
	call(t, s, "touch", touchRequest(holder, "hold", "src/api.go"))

	group := func(worktree, id, branch, path string) ClaimGroup {
		return ClaimGroup{
			Worktree:   worktree,
			WorktreeID: id,
			RemoteURL:  "https://example.com/team/project.git",
			RootCommit: "root",
			Branch:     branch,
			Paths:      []string{path},
		}
	}
	first := filesTouch(editor, "edit", group("/repo/w2", "dev:editor", "main", "src/api.go"))
	denied := call(t, s, "touch_files", first)
	if denied["action"] != "deny" || denied["message"] == "" {
		t.Fatalf("refusal: %v", denied)
	}
	same := func(got map[string]any) {
		t.Helper()
		if got["action"] != "deny" || got["message"] != denied["message"] {
			t.Fatalf("cached refusal: action %v message %v", got["action"], got["message"])
		}
	}
	branched := filesTouch(editor, "edit", group("/repo/w2", "dev:editor", "feature", "src/api.go"))
	same(call(t, s, "touch_files", branched))
	renamed := filesTouch(editor, "edit", group("/home/editor/checkout", "dev:editor", "feature", "src/api.go"))
	same(call(t, s, "touch_files", renamed))

	// A different checkout id is a different request. A different file is too.
	expectError(t, s, "touch_files", filesTouch(editor, "edit", group("/repo/w2", "dev:other", "feature", "src/api.go")), "key_reused")

	legacy := filesTouch(editor, "legacy", group("/repo/w2", "dev:editor", "main", "src/api.go"))
	legacyDenied := call(t, s, "touch_files", legacy)
	if legacyDenied["action"] != "deny" || legacyDenied["message"] == "" {
		t.Fatalf("legacy refusal: %v", legacyDenied)
	}
	stored := legacy
	stored.Groups = []ClaimGroup{group("/repo/w2", "dev:editor", "", "src/api.go")}
	hash := hashAsV0113("touch_files", stored)
	res, err := s.db.Exec(`UPDATE Operation SET request_hash=? WHERE key=?`, hash, "touch_files:legacy")
	if err != nil {
		t.Fatal(err)
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		t.Fatalf("stored v0.1.13 hash: %d %v", n, err)
	}
	again := call(t, s, "touch_files", filesTouch(editor, "legacy", group("/repo/w2", "dev:editor", "release", "src/api.go")))
	if again["action"] != "deny" || again["message"] != legacyDenied["message"] {
		t.Fatalf("v0.1.13 retry: action %v message %v", again["action"], again["message"])
	}
	expectError(t, s, "touch_files", filesTouch(editor, "legacy", group("/repo/w2", "dev:editor", "release", "src/other.go")), "key_reused")
}

// hashAsV0113 is the operation fingerprint v0.1.13 stored: the request JSON
// with an empty branch omitted, and the checkout path still present.
func hashAsV0113(route string, r Request) []byte {
	raw, err := json.Marshal(struct {
		Route   string
		Request Request
	}{route, r})
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(raw)
	return sum[:]
}
