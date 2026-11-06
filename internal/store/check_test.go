package store

import "testing"

func TestCheckOtherWorktreeAndExpiry(t *testing.T) {
	s, clock, _ := testStore(t)
	holder, info := session(t, s, 1)
	call(t, s, "claim", claimRequest(holder, "claim", "src", "prefix"))
	r := Request{Host: "host", Worktree: "/repo/w2", RemoteURL: "https://example.com/team/iris.git", RootCommit: "root", Paths: []string{"src/file\nname"}}
	if out := call(t, s, "check", r); out["message"] == "" {
		t.Fatal("missing conflict", out)
	}
	r.Worktree = "/repo/w1"
	if out := call(t, s, "check", r); out["message"] != "" {
		t.Fatal("same worktree blocked", out)
	}
	r.Host = "other-host"
	if out := call(t, s, "check", r); out["message"] == "" {
		t.Fatal("other host not checked", out)
	}
	_ = info
	clock.tick += 900001
	if out := call(t, s, "check", r); out["message"] != "" {
		t.Fatal("expired claim blocked", out)
	}
}

func TestCheckLinkedWorktreesWithoutRemote(t *testing.T) {
	s, _, _ := testStore(t)
	r := Request{Client: "codex", Host: "host", Cwd: "/repo/first", Worktree: "/repo/first", CommonDir: "/repo/first/.git", SessionID: "first"}
	first := call(t, s, "sessions", r)
	claim := Request{ExecutionID: first["execution_id"].(string), Generation: first["generation"].(int64)}
	call(t, s, "claim", claimRequest(claim, "held", "file", "exact"))
	r.Worktree = "/repo/linked"
	r.Cwd = r.Worktree
	r.SessionID = "linked"
	second := call(t, s, "sessions", r)
	if first["repository_id"] != second["repository_id"] {
		t.Fatal("linked worktree split repository identity")
	}
	r.Paths = []string{"file"}
	if out := call(t, s, "check", r); out["message"] == "" {
		t.Fatal("local linked worktree missed claim", out)
	}
	r.Host = "different-host"
	if out := call(t, s, "check", r); out["message"] != "" {
		t.Fatal("machine-relative path crossed hosts", out)
	}
}
