package store

import "testing"

// A non-default port can be a different service on the same host, so two remotes
// that differ only by it must not join. A default port is the same address.
func TestARemoteKeepsANonDefaultPort(t *testing.T) {
	plain := remoteAlias("https://git.example.com/team/shop.git")
	for _, same := range []string{"https://git.example.com:443/team/shop.git", "ssh://git@git.example.com:22/team/shop.git", "git://git.example.com:9418/team/shop.git"} {
		if got := remoteAlias(same); got != plain {
			t.Errorf("%s: %q, want %q", same, got, plain)
		}
	}
	for _, other := range []string{"https://git.example.com:8443/team/shop.git", "ssh://git@git.example.com:2222/team/shop.git", "http://git.example.com:3000/team/shop.git"} {
		if got := remoteAlias(other); got == plain || got == "" {
			t.Errorf("%s: %q must differ from %q", other, got, plain)
		}
	}
	if got := remoteAlias("https://git.example.com:8443/team/shop.git"); got != "remote:git.example.com:8443/team/shop" {
		t.Fatal(got)
	}
	s, _, _ := testStore(t)
	a := call(t, s, "sessions", Request{Client: "claude", Host: "one", SessionID: "a", Cwd: "/a", Worktree: "/a", RemoteURL: "https://git.example.com/team/shop.git"})
	b := call(t, s, "sessions", Request{Client: "claude", Host: "two", SessionID: "b", Cwd: "/b", Worktree: "/b", RemoteURL: "https://git.example.com:8443/team/shop.git"})
	if a["repository_id"] == b["repository_id"] {
		t.Fatal("two services on one host were joined")
	}
}

// A repository first seen without a root commit learns one. A later session that
// presents a different root must not join it by remote: that is another history.
func TestARootLearnedLaterRefusesAContradictingOne(t *testing.T) {
	s, _, _ := testStore(t)
	remote := "https://github.com/team/shop.git"
	first := call(t, s, "sessions", Request{Client: "claude", Host: "one", SessionID: "a", Cwd: "/a", Worktree: "/a", RemoteURL: remote})
	call(t, s, "sessions", Request{Client: "claude", Host: "one", SessionID: "a2", Cwd: "/a", Worktree: "/a", RemoteURL: remote, RootCommit: "root-a"})
	var root string
	if err := s.db.QueryRow("SELECT root_commit FROM Repository WHERE id=?", first["repository_id"]).Scan(&root); err != nil || root != "root-a" {
		t.Fatalf("the learned root was not kept: %q %v", root, err)
	}
	same := call(t, s, "sessions", Request{Client: "claude", Host: "two", SessionID: "b", Cwd: "/b", Worktree: "/b", RemoteURL: remote, RootCommit: "root-a"})
	other := call(t, s, "sessions", Request{Client: "claude", Host: "three", SessionID: "c", Cwd: "/c", Worktree: "/c", RemoteURL: remote, RootCommit: "root-b"})
	if same["repository_id"] != first["repository_id"] || other["repository_id"] == first["repository_id"] {
		t.Fatalf("first %v same %v other %v", first["repository_id"], same["repository_id"], other["repository_id"])
	}
}
