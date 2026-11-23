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
	withCheckout := modern
	withCheckout.Checkout = &ClaimGroup{Worktree: "/repo/w2", WorktreeID: "dev:editor", CommonDir: "/repo/w2/.git", Paths: []string{"src/api.go"}}
	if fmtHex(legacyOperationHash("touch_files", withCheckout)) == goldenV0113Digest {
		t.Fatal("the checkout was left out of the v0.1.13 digest")
	}
	otherDir := modern
	otherDir.Groups[0].CommonDir = "/somewhere/else/.git"
	if fmtHex(legacyOperationHash("touch_files", otherDir)) == goldenV0113Digest {
		t.Fatal("common dir was left out of the v0.1.13 digest")
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

// A grouped refusal is cached by the v0.1.13 digest. A later branch, and a
// change to the alias table, are not part of that digest. The checkout path,
// id and common dir are, so a different path on the same tool call is key_reused.
func TestAGroupedRetryIgnoresBranchAndAliasDisplay(t *testing.T) {
	s, _, _ := testStore(t)
	holder, _ := session(t, s, 1)
	editor, _ := session(t, s, 2)
	call(t, s, "touch", touchRequest(holder, "hold", "src/api.go"))
	remote := "https://example.com/team/project.git"
	group := ClaimGroup{
		Worktree: "/repo/w2", WorktreeID: "dev:editor", CommonDir: "/repo/w2/.git",
		RemoteURL: remote, RootCommit: "root", Branch: "main", Paths: []string{"src/api.go"},
	}
	first := filesTouch(editor, "edit", group)
	denied := call(t, s, "touch_files", first)
	if denied["action"] != "deny" || denied["message"] == "" {
		t.Fatalf("refusal: %v", denied)
	}
	res, err := s.db.Exec("UPDATE WorktreeAlias SET worktree=? WHERE host_id=? AND worktree_id=?", "/somewhere/else", "host", "dev:editor")
	if err != nil {
		t.Fatal(err)
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		t.Fatalf("alias rows %d %v", n, err)
	}
	again := filesTouch(editor, "edit", group)
	again.Groups[0].Branch = "release"
	again.LastMessageID = "later"
	again.ClaimAutoScope = "worktree"
	got := call(t, s, "touch_files", again)
	if got["action"] != "deny" || got["message"] != denied["message"] {
		t.Fatalf("cached refusal: action %v message %v", got["action"], got["message"])
	}
	otherPath := filesTouch(editor, "edit", group)
	otherPath.Groups[0].Paths = []string{"src/other.go"}
	otherPath.Groups[0].Branch = "release"
	expectError(t, s, "touch_files", otherPath, "key_reused")
	otherDir := filesTouch(editor, "edit", group)
	otherDir.Groups[0].CommonDir = "/somewhere/else/.git"
	otherDir.Groups[0].Branch = "release"
	expectError(t, s, "touch_files", otherDir, "key_reused")
}
