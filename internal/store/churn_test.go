package store

import (
	"fmt"
	"testing"
)

// worktreeSession registers session id in the shared worktree /repo/shared, as a hook does.
func worktreeSession(t *testing.T, s *Store, id string) (Request, string) {
	t.Helper()
	out := call(t, s, "sessions", Request{Client: "codex", Host: "host", Cwd: "/repo/shared", Worktree: "/repo/shared", RemoteURL: "https://example.com/team/project.git", RootCommit: "root", SessionID: id, Source: "hook"})
	return Request{ExecutionID: out["execution_id"].(string), Generation: out["generation"].(int64)}, out["agent"].(map[string]any)["name"].(string)
}

// Gap review B3: three sessions share a worktree, so they take three slots, three mailboxes.
// Each gets its own mail, ends without reading it, and they come back in every order. A
// session gets its own mailbox back whatever the order, so it reads its own mail and no one
// else's, and a new session takes a slot of its own.
func TestMailboxesSurviveSlotChurnInEveryOrder(t *testing.T) {
	orders := [][]int{{0, 1, 2}, {0, 2, 1}, {1, 0, 2}, {1, 2, 0}, {2, 0, 1}, {2, 1, 0}}
	for _, order := range orders {
		t.Run(fmt.Sprint(order), func(t *testing.T) {
			s, _, _ := testStore(t)
			sender, _ := session(t, s, 9)
			ids := []string{"first", "second", "third"}
			mailbox := map[string]string{}
			var live []Request
			for i, id := range ids {
				r, name := worktreeSession(t, s, id)
				mailbox[id] = name
				live = append(live, r)
				mail := sender
				mail.Key, mail.To, mail.Subject, mail.Body = "to-"+id, []string{name}, id, fmt.Sprintf("for %s only (%d)", id, i)
				call(t, s, "messages", mail)
			}
			if mailbox["first"] == mailbox["second"] || mailbox["second"] == mailbox["third"] || mailbox["first"] == mailbox["third"] {
				t.Fatalf("concurrent sessions share a mailbox: %v", mailbox)
			}
			for _, r := range live {
				call(t, s, "end", r)
			}
			for _, i := range order {
				id := ids[i]
				r, name := worktreeSession(t, s, id)
				if name != mailbox[id] {
					t.Fatalf("%s came back as %s, not its own %s", id, name, mailbox[id])
				}
				got := call(t, s, "inbox", r)["deliveries"].([]map[string]any)
				if len(got) != 1 || got[0]["subject"] != id {
					t.Fatalf("%s reads %v", id, got)
				}
			}
			if _, name := worktreeSession(t, s, "newcomer"); name == mailbox["first"] || name == mailbox["second"] || name == mailbox["third"] {
				t.Fatalf("a new session took a held mailbox: %s", name)
			}
		})
	}
}
