package store

import (
	"strings"
	"testing"
)

func TestCheckOtherWorktreeAndExpiry(t *testing.T) {
	s, clock, _ := testStore(t)
	holder, info := session(t, s, 1)
	call(t, s, "claim", claimRequest(holder, "claim", "src", "prefix"))
	r := Request{Host: "host", Worktree: "/repo/w2", RemoteURL: "https://example.com/team/project.git", RootCommit: "root", Paths: []string{"src/file\nname"}}
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

// A refusal says where the holder works: its branch, worktree and host, so the person refused
// knows which checkout to ask about. A worktree path is escaped onto one line, like the path.
func TestCheckNamesTheHoldersBranchAndWorktree(t *testing.T) {
	s, _, _ := testStore(t)
	holder := call(t, s, "sessions", Request{Client: "codex", Host: "home-wsl", Cwd: "/home/a/iris\nx", Worktree: "/home/a/iris\nx", Branch: "feat/claims", RemoteURL: "https://example.com/team/project.git", RootCommit: "root", SessionID: "h", Source: "hook"})
	call(t, s, "claim", claimRequest(Request{ExecutionID: holder["execution_id"].(string), Generation: holder["generation"].(int64)}, "c", "src/app.ts", "exact"))
	out := call(t, s, "check", Request{Host: "pc", Worktree: `D:\projects\iris`, RemoteURL: "https://example.com/team/project.git", RootCommit: "root", Paths: []string{"src/app.ts"}})
	msg := out["message"].(string)
	if !strings.Contains(msg, `(branch feat/claims, worktree /home/a/iris\u000ax on home-wsl)`) {
		t.Fatalf("message %q", msg)
	}
	c := out["conflicts"].([]map[string]any)[0]
	if c["branch"] != "feat/claims" || c["worktree"] != "/home/a/iris\nx" || c["host"] != "home-wsl" {
		t.Fatalf("conflict %v", c)
	}
	// A detached HEAD has no branch to name.
	detached := call(t, s, "sessions", Request{Client: "codex", Host: "mac", Cwd: "/m/iris", Worktree: "/m/iris", RemoteURL: "https://example.com/team/project.git", RootCommit: "root", SessionID: "d", Source: "hook"})
	call(t, s, "claim", claimRequest(Request{ExecutionID: detached["execution_id"].(string), Generation: detached["generation"].(int64)}, "d", "README", "exact"))
	out = call(t, s, "check", Request{Host: "pc", Worktree: `D:\projects\iris`, RemoteURL: "https://example.com/team/project.git", RootCommit: "root", Paths: []string{"README"}})
	if msg := out["message"].(string); !strings.Contains(msg, "(worktree /m/iris on mac)") {
		t.Fatalf("detached message %q", msg)
	}
}
