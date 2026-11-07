package store

import (
	"fmt"
	"testing"
)

// Two sessions in one worktree get slots 1 and 2. Mail is sent to the second.
// Both end, and they resume in either order: each must get its own mailbox
// back, because the mail is addressed to it (R4, D9).
func TestAResumedSessionGetsItsOwnMailboxBack(t *testing.T) {
	for _, order := range [][2]string{{"a", "b"}, {"b", "a"}} {
		t.Run(order[0]+" first", func(t *testing.T) {
			s, _, _ := testStore(t)
			register := func(id string) map[string]any {
				return call(t, s, "sessions", Request{Client: "codex", Host: "host", SessionID: id, Cwd: "/repo/w", Worktree: "/repo/w", RemoteURL: "https://example.com/team/project.git", RootCommit: "root", Source: "hook"})
			}
			bound := func(out map[string]any) Request {
				return Request{ExecutionID: out["execution_id"].(string), Generation: out["generation"].(int64)}
			}
			agents := map[string]string{}
			first := map[string]Request{}
			for _, id := range []string{"a", "b"} {
				out := register(id)
				agents[id] = out["agent"].(map[string]any)["name"].(string)
				first[id] = bound(out)
			}
			if agents["a"] == agents["b"] {
				t.Fatal("two live sessions share a mailbox")
			}
			mail := first["a"]
			mail.Key, mail.To, mail.Body = "m", []string{agents["b"]}, "for b"
			call(t, s, "messages", mail)
			call(t, s, "end", first["a"])
			call(t, s, "end", first["b"])
			for _, id := range order {
				out := register(id)
				if got := out["agent"].(map[string]any)["name"]; got != agents[id] {
					t.Fatalf("%s resumed as %v, want its own %s", id, got, agents[id])
				}
				inbox := call(t, s, "inbox", bound(out))["deliveries"].([]map[string]any)
				want := map[string]int{"a": 0, "b": 1}[id]
				if len(inbox) != want {
					t.Fatalf("%s sees %d deliveries, want %d", id, len(inbox), want)
				}
			}
		})
	}
}

// A session whose old mailbox another live session now holds gets a free slot instead.
func TestAResumedSessionDoesNotTakeAMailboxInUse(t *testing.T) {
	s, _, _ := testStore(t)
	register := func(id string) map[string]any {
		return call(t, s, "sessions", Request{Client: "codex", Host: "host", SessionID: id, Cwd: "/repo/w", Worktree: "/repo/w", RemoteURL: "https://example.com/team/project.git", RootCommit: "root", Source: "hook"})
	}
	a := register("a")
	call(t, s, "end", Request{ExecutionID: a["execution_id"].(string), Generation: a["generation"].(int64)})
	// c takes the free slot 1, which was a's.
	c := register("c")
	again := register("a")
	if fmt.Sprint(again["agent"]) == fmt.Sprint(c["agent"]) {
		t.Fatal("a took the mailbox c is using")
	}
}
