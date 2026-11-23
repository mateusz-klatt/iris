package store

import (
	"crypto/sha256"
	"testing"
)

// goldenV0113JSON is the operation document v0.1.13 hashed for one grouped touch.
// The digest below is sha256 of these exact bytes. It is not produced by hashing
// today's Request, which has fields that release did not.
const goldenV0113JSON = `{"Route":"touch_files","Request":{"common_dir":"","status":null,"reply_to":"","q":"","agent":"","dry_run":false,"before":"","checked_at":"","clients":null,"client":"","session_id":"","host":"","cwd":"","worktree":"","worktree_id":"","receipt":false,"receipts":null,"remote_url":"","root_commit":"","branch":"","parent_session_id":"","client_pid":0,"client_pid_start_ms":0,"single_session":false,"source":"","execution":"","exclude":"","hand_over":"","execution_id":"ex_golden","generation":1,"key":"","tool_call_id":"legacy","paths":null,"rename_from":"","rename_to":"","groups":[{"worktree":"/repo/w2","worktree_id":"dev:editor","common_dir":"/repo/w2/.git","remote_url":"https://example.com/team/project.git","root_commit":"root","paths":["src/api.go"]}],"kind":"","path":"","repo_wide":false,"ttl_s":0,"fence":0,"all":false,"token":"","to":null,"subject":"","body":"","attempt":0,"handled":false,"repository_id":"","after":0,"limit":0,"consumer_id":"","stream":"","attention":false}}`

const goldenV0113Digest = "a3d9fdf1c4834230aea94282bf367ec62e788d59f116a913781f89ed5ff82f09"

func TestLegacyFingerprintMatchesTheV0113Digest(t *testing.T) {
	sum := sha256.Sum256([]byte(goldenV0113JSON))
	if got := fmtHex(sum[:]); got != goldenV0113Digest {
		t.Fatalf("fixture digest %s", got)
	}
	modern := Request{
		ExecutionID: "ex_golden", Generation: 1, ToolCallID: "legacy",
		LastMessageID: "m1", ClaimAutoScope: "worktree",
		Groups: []ClaimGroup{{
			Worktree: "/repo/w2", WorktreeID: "dev:editor", CommonDir: "/repo/w2/.git",
			RemoteURL: "https://example.com/team/project.git", RootCommit: "root", Branch: "release",
			Paths: []string{"src/api.go"},
		}},
	}
	if got := fmtHex(legacyOperationHash("touch_files", modern)); got != goldenV0113Digest {
		t.Fatalf("legacy hash %s", got)
	}
}

func fmtHex(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, v := range b {
		out[i*2] = digits[v>>4]
		out[i*2+1] = digits[v&0x0f]
	}
	return string(out)
}

// A v0.1.13 grouped refusal hashed the checkout path and no branch. After the
// upgrade the hook sends a branch, and may spell the same checkout another way.
// Either used to be key_reused, which the hook fails open. A path with no
// worktree id still identifies the checkout, so it cannot replay another allow.
func TestAGroupedRefusalStaysCachedWhenDisplayMetadataChanges(t *testing.T) {
	s, _, _ := testStore(t)
	holder, _ := session(t, s, 1)
	editor, _ := session(t, s, 2)
	call(t, s, "touch", touchRequest(holder, "hold", "src/api.go"))

	group := func(worktree, id, common, remote, root, branch, path string) ClaimGroup {
		return ClaimGroup{
			Worktree: worktree, WorktreeID: id, CommonDir: common, RemoteURL: remote, RootCommit: root, Branch: branch, Paths: []string{path},
		}
	}
	remote := "https://example.com/team/project.git"
	first := filesTouch(editor, "edit", group("/repo/w2", "dev:editor", "/repo/w2/.git", remote, "root", "main", "src/api.go"))
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
	aliased := filesTouch(editor, "edit", group("/home/editor/checkout", "dev:editor", "/home/editor/checkout/.git", remote, "root", "feature", "src/api.go"))
	aliased.LastMessageID = "m1"
	aliased.ClaimAutoScope = "repository"
	same(call(t, s, "touch_files", aliased))

	// A different checkout id is a different request. So is a path with no id.
	expectError(t, s, "touch_files", filesTouch(editor, "edit", group("/repo/w2", "dev:other", "/repo/w2/.git", remote, "root", "feature", "src/api.go")), "key_reused")
	expectError(t, s, "touch_files", filesTouch(editor, "edit", group("/home/editor/checkout", "", "/home/editor/checkout/.git", remote, "root", "feature", "src/api.go")), "key_reused")
	// The id now names another history. The path stays in the hash.
	expectError(t, s, "touch_files", filesTouch(editor, "edit", group("/repo/w2", "dev:editor", "/repo/w2/.git", "https://example.com/other.git", "root", "feature", "src/api.go")), "key_reused")

	legacy := filesTouch(editor, "legacy", group("/repo/w2", "dev:editor", "/repo/w2/.git", remote, "root", "main", "src/api.go"))
	legacyDenied := call(t, s, "touch_files", legacy)
	if legacyDenied["action"] != "deny" || legacyDenied["message"] == "" {
		t.Fatalf("legacy refusal: %v", legacyDenied)
	}
	stored := legacy
	stored.Groups[0].Branch = ""
	hash := legacyOperationHash("touch_files", stored)
	res, err := s.db.Exec(`UPDATE Operation SET request_hash=? WHERE key=?`, hash, "touch_files:legacy")
	if err != nil {
		t.Fatal(err)
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		t.Fatalf("stored v0.1.13 hash: %d %v", n, err)
	}
	again := filesTouch(editor, "legacy", group("/repo/w2", "dev:editor", "/repo/w2/.git", remote, "root", "release", "src/api.go"))
	again.LastMessageID = "later"
	again.ClaimAutoScope = "worktree"
	got := call(t, s, "touch_files", again)
	if got["action"] != "deny" || got["message"] != legacyDenied["message"] {
		t.Fatalf("v0.1.13 retry: action %v message %v", got["action"], got["message"])
	}
	expectError(t, s, "touch_files", filesTouch(editor, "legacy", group("/repo/w2", "dev:editor", "/repo/w2/.git", remote, "root", "release", "src/other.go")), "key_reused")
}

// Two checkouts with no worktree id are different requests even when the tool
// call id is reused. Replaying the first allow would grant the second checkout.
func TestAnUnidentifiedCheckoutDoesNotReplayAnotherAllow(t *testing.T) {
	s, _, _ := testStore(t)
	editor, _ := session(t, s, 2)
	remote := "https://example.com/team/project.git"
	first := filesTouch(editor, "once", ClaimGroup{Worktree: "/repo/w2", CommonDir: "/repo/w2/.git", RemoteURL: remote, RootCommit: "root", Paths: []string{"a.go"}})
	allowed := call(t, s, "touch_files", first)
	if allowed["action"] != "allow" || len(allowed["granted"].([]map[string]any)) != 1 {
		t.Fatal(allowed)
	}
	other := filesTouch(editor, "once", ClaimGroup{Worktree: "/repo/other", CommonDir: "/repo/other/.git", RemoteURL: remote, RootCommit: "root", Paths: []string{"a.go"}})
	expectError(t, s, "touch_files", other, "key_reused")
}

// A submodule checkout already known to the hub keeps one hash when the hook
// spells its path and common dir another way. An id that names no repository
// does not.
func TestAnEstablishedCheckoutAliasKeepsTheCachedDecision(t *testing.T) {
	s, _, _ := testStore(t)
	parent, _ := register(t, s, "parent", "/src/snapper", superRemote, "snapper-root")
	holder, _ := register(t, s, "fe", "/src/snapper/frontend", frontendRemote, "frontend-root")
	call(t, s, "touch", touchRequest(holder, "hold", "src/a.ts"))
	group := submodule("src/a.ts")
	group.WorktreeID = "dev:fe"
	group.Branch = "main"
	denied := call(t, s, "touch_files", filesTouch(parent, "edit", group))
	if denied["action"] != "deny" {
		t.Fatal(denied)
	}
	alias := group
	alias.Worktree = "/alias/frontend"
	alias.CommonDir = "/alias/.git/modules/frontend"
	alias.Branch = "feature"
	got := call(t, s, "touch_files", filesTouch(parent, "edit", alias))
	if got["action"] != "deny" || got["message"] != denied["message"] {
		t.Fatalf("alias retry: %v", got)
	}
	if _, err := s.db.Exec(`INSERT INTO WorktreeAlias(host_id,worktree_id,worktree,remote_url,root_commit) VALUES('host','dev:orphan','/orphan',?,?)`, frontendRemote, "frontend-root"); err != nil {
		t.Fatal(err)
	}
	orphan := group
	orphan.Worktree = "/orphan"
	orphan.WorktreeID = "dev:orphan"
	orphan.CommonDir = "/orphan/.git"
	expectError(t, s, "touch_files", filesTouch(parent, "edit", orphan), "key_reused")
}
