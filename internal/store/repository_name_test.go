package store

import "testing"

func TestRepositoryNameStable(t *testing.T) {
	for _, remote := range []string{"https://example.com/team/shop.git", "git@example.com:team/shop.git", ""} {
		t.Run(remote, func(t *testing.T) {
			s, _, _ := testStore(t)
			r := Request{Client: "codex", Host: "host", SessionID: "first", Cwd: "/work/shop-a", Worktree: "/work/shop-a", RemoteURL: remote}
			first := call(t, s, "sessions", r)
			// A local-only repository joins by the same alias; global remotes join across worktrees.
			if remote != "" {
				r.Worktree = "/work/shop-z"
			}
			r.SessionID = "second"
			call(t, s, "sessions", r)
			var name string
			if err := s.db.QueryRow("SELECT name FROM Repository WHERE id=?", first["repository_id"]).Scan(&name); err != nil {
				t.Fatal(err)
			}
			want := "shop"
			if remote == "" {
				want = "shop-a"
			}
			if name != want {
				t.Fatalf("name=%q want %q", name, want)
			}
		})
	}
}
