package store

import (
	"fmt"
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

func TestCheckOwnSessionInAnotherWorktreeExplainsRelease(t *testing.T) {
	s, _, _ := testStore(t)
	r := Request{Client: "codex", Host: "host", Cwd: "/repo/w1", Worktree: "/repo/w1", CommonDir: "/repo/w1/.git", SessionID: "holder", Source: "hook"}
	first := call(t, s, "sessions", r)
	holder := Request{ExecutionID: first["execution_id"].(string), Generation: first["generation"].(int64)}
	held := call(t, s, "claim", claimRequest(holder, "held", "src", "prefix"))
	// A hook in a second checkout keeps this active session's Execution and mailbox,
	// but the guard has only the committing checkout's identity.
	r.Cwd, r.Worktree = "/repo/w2", "/repo/w2"
	again := call(t, s, "sessions", r)
	if first["execution_id"] != again["execution_id"] || first["agent"].(map[string]any)["id"] != again["agent"].(map[string]any)["id"] {
		t.Fatalf("same session split across worktrees: %v and %v", first, again)
	}
	r.Paths = []string{"src/file.go"}
	// Supplied session and Execution fields do not prove who made a commit.
	r.ExecutionID, r.Execution, r.Generation = holder.ExecutionID, holder.ExecutionID, holder.Generation
	out := call(t, s, "check", r)
	message := out["message"].(string)
	for _, want := range []string{
		"worktree /repo/w1 on host",
		"The guard cannot identify the committing session.",
		fmt.Sprintf("If this is your session in another worktree, call the release tool with {\"fence\":%v} from that session, then retry the commit.", held["fence"]),
		"Otherwise message the holder with the send tool",
	} {
		if !strings.Contains(message, want) {
			t.Fatalf("refusal %q missing %q", message, want)
		}
	}
	conflicts := out["conflicts"].([]map[string]any)
	if len(conflicts) != 1 || conflicts[0]["fence"] != held["fence"] || conflicts[0]["claimed_path"] != "src" {
		t.Fatalf("wrong claim to release: %v", conflicts)
	}
	// A different session cannot release the named fence, and the guard still refuses it.
	r.SessionID = "peer"
	r.ExecutionID, r.Execution, r.Generation = "", "", 0
	peerInfo := call(t, s, "sessions", r)
	peer := Request{ExecutionID: peerInfo["execution_id"].(string), Generation: peerInfo["generation"].(int64), Fence: number(held["fence"])}
	expectError(t, s, "release", peer, "conflict")
	if got := call(t, s, "check", r)["message"]; got != message {
		t.Fatalf("another session changed the refusal: %v", got)
	}
	call(t, s, "claim", claimRequest(peer, "peer-claim", "peer.go", "exact"))
	holder.Fence = number(held["fence"])
	call(t, s, "release", holder)
	if out := call(t, s, "check", r); out["message"] != "" {
		t.Fatalf("holder's release did not permit retry: %v", out)
	}
	// Releasing that one fence preserves claims held by other sessions.
	r.Worktree, r.Paths = "/repo/w3", []string{"peer.go"}
	if out := call(t, s, "check", r); len(out["conflicts"].([]map[string]any)) != 1 {
		t.Fatalf("release removed another session's claim: %v", out)
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
