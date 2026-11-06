package store

import "testing"

func TestRemoteAliasSpellings(t *testing.T) {
	want := "remote:github.com/team/shop"
	for _, raw := range []string{
		"git@github.com:team/shop.git",
		"github.com:team/shop.git",
		"https://github.com/team/shop.git",
		"https://github.com/team/shop",
		"https://user@GitHub.com/team/shop/",
		"ssh://git@github.com/team/shop.git",
		"ssh://git@github.com:22/team/shop.git",
		"git://github.com/team/shop.git",
	} {
		if got := remoteAlias(raw); got != want {
			t.Errorf("%s: got %q, want %q", raw, got, want)
		}
	}
	// Each of these can mean something different on another machine.
	for _, raw := range []string{
		"",
		"git@work:team/shop.git",
		"origin:team/shop.git",
		"../shop.git",
		"/srv/git/shop.git",
		"C:\\repos\\shop.git",
		"file:///srv/git/shop.git",
		"git@10.0.0.1:team/shop.git",
		"git@host.local:team/shop.git",
		"https://github.com/",
	} {
		if got := remoteAlias(raw); got != "" {
			t.Errorf("%s: got %q, want none", raw, got)
		}
	}
}

// Found live: two machines with an ssh origin were split into two repositories, so
// their agents could not see each other.
func TestMachinesJoinOverSSHAndHTTPS(t *testing.T) {
	s, _, _ := testStore(t)
	first := call(t, s, "sessions", Request{Client: "claude", Host: "linux", SessionID: "a", Cwd: "/src/shop", Worktree: "/src/shop", RemoteURL: "git@github.com:team/shop.git", RootCommit: "abc"})
	second := call(t, s, "sessions", Request{Client: "claude", Host: "wsl", SessionID: "b", Cwd: "/home/me/shop", Worktree: "/home/me/shop", RemoteURL: "git@github.com:team/shop.git"})
	third := call(t, s, "sessions", Request{Client: "claude", Host: "win", SessionID: "c", Cwd: "D:/shop", Worktree: "D:/shop", RemoteURL: "https://github.com/team/shop.git"})
	if first["repository_id"] != second["repository_id"] || first["repository_id"] != third["repository_id"] {
		t.Fatalf("split: %v %v %v", first["repository_id"], second["repository_id"], third["repository_id"])
	}
}

func TestKnownWorktreeGainsRemote(t *testing.T) {
	s, _, _ := testStore(t)
	first := call(t, s, "sessions", Request{Client: "claude", Host: "linux", SessionID: "a", Cwd: "/src/shop", Worktree: "/src/shop"})
	call(t, s, "sessions", Request{Client: "claude", Host: "linux", SessionID: "b", Cwd: "/src/shop", Worktree: "/src/shop", RemoteURL: "git@github.com:team/shop.git"})
	other := call(t, s, "sessions", Request{Client: "claude", Host: "wsl", SessionID: "c", Cwd: "/home/me/shop", Worktree: "/home/me/shop", RemoteURL: "git@github.com:team/shop.git"})
	if first["repository_id"] != other["repository_id"] {
		t.Fatalf("split: %v %v", first["repository_id"], other["repository_id"])
	}
}
