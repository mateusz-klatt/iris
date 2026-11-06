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
