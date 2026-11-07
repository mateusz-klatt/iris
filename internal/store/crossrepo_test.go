package store

import (
	"context"
	"errors"
	"testing"
)

// crossRepo registers one session in each of two repositories, iris and snapper.
func crossRepo(t *testing.T) (*Store, Request, map[string]any, Request, map[string]any) {
	t.Helper()
	s, _, _ := testStore(t)
	irisOut := call(t, s, "sessions", Request{Client: "claude", Host: "home", SessionID: "i", Cwd: "/src/iris", Worktree: "/src/iris", RemoteURL: "https://github.com/team/iris.git", RootCommit: "iris-root", Source: "hook"})
	snapOut := call(t, s, "sessions", Request{Client: "claude", Host: "home", SessionID: "s", Cwd: "/src/snapper", Worktree: "/src/snapper", RemoteURL: "https://github.com/team/snapper.git", RootCommit: "snap-root", Source: "hook"})
	bound := func(out map[string]any) Request {
		return Request{ExecutionID: out["execution_id"].(string), Generation: out["generation"].(int64)}
	}
	return s, bound(irisOut), irisOut, bound(snapOut), snapOut
}

// firstDelivery reads a send result, which a retry returns decoded from JSON.
func firstDelivery(out map[string]any) map[string]any {
	switch ds := out["deliveries"].(type) {
	case []map[string]any:
		return ds[0]
	case []any:
		return ds[0].(map[string]any)
	}
	return nil
}

func agentName(out map[string]any) string { return out["agent"].(map[string]any)["name"].(string) }

func sendTo(r Request, key, body string, to ...string) Request {
	r.Key, r.Body, r.To = key, body, to
	return r
}

func refusedWith(t *testing.T, s *Store, op string, r Request, code string) {
	t.Helper()
	_, err := s.Call(context.Background(), op, r)
	var e *Error
	if !errors.As(err, &e) || e.Code != code {
		t.Fatalf("%s: %v, want %s", op, err, code)
	}
}

// A snapper agent reports an Iris bug to an Iris agent by name; the recipient sees
// which repository it came from.
func TestAnAgentInAnotherRepositoryCanBeMessagedByName(t *testing.T) {
	s, irisAgent, irisOut, snap, snapOut := crossRepo(t)
	out := call(t, s, "messages", sendTo(snap, "bug", "iris watch wakes too often", agentName(irisOut)))
	d := out["deliveries"].([]map[string]any)[0]
	if d["repository_id"] != irisOut["repository_id"] || d["queued"] != false {
		t.Fatalf("%v", d)
	}
	inbox := call(t, s, "inbox", irisAgent)["deliveries"].([]map[string]any)
	if len(inbox) != 1 || inbox[0]["from_repository"] != "snapper" || inbox[0]["cross_repository"] != true {
		t.Fatalf("%v", inbox)
	}
	// Mail inside one repository is not marked.
	call(t, s, "messages", sendTo(snap, "self", "note", agentName(snapOut)))
	own := call(t, s, "inbox", snap)["deliveries"].([]map[string]any)
	if own[0]["cross_repository"] != false {
		t.Fatal(own)
	}
	refusedWith(t, s, "messages", sendTo(snap, "nobody", "x", "no-such-agent"), "not_found")
}

// repo: reaches the contact the owner set, and the human inbox without one.
func TestARepositoryAddressReachesItsContactOrItsHuman(t *testing.T) {
	s, irisAgent, irisOut, snap, _ := crossRepo(t)
	repo := irisOut["repository_id"].(string)
	first := call(t, s, "messages", sendTo(snap, "r1", "first", "repo:"+repo))["deliveries"].([]map[string]any)[0]
	if first["recipient"] != "human" || first["repository_id"] != repo {
		t.Fatalf("no contact: %v", first)
	}
	call(t, s, "contact", Request{RepositoryID: repo, Agent: agentName(irisOut)})
	second := call(t, s, "messages", sendTo(snap, "r2", "second", "repo:iris"))["deliveries"].([]map[string]any)[0]
	if second["recipient"] != agentName(irisOut) {
		t.Fatalf("contact by name: %v", second)
	}
	// A retry after the contact changed returns its original delivery.
	call(t, s, "contact", Request{RepositoryID: repo})
	again := firstDelivery(call(t, s, "messages", sendTo(snap, "r2", "second", "repo:iris")))
	if again["id"] != second["id"] || again["recipient"] != agentName(irisOut) {
		t.Fatalf("retry was resolved again: %v", again)
	}
	if n := len(call(t, s, "inbox", irisAgent)["deliveries"].([]map[string]any)); n != 1 {
		t.Fatal(n)
	}
	refusedWith(t, s, "messages", sendTo(snap, "r3", "x", "repo:nonexistent"), "not_found")
	refusedWith(t, s, "contact", Request{RepositoryID: "rp_nope", Agent: "x"}, "not_found")
	refusedWith(t, s, "contact", Request{RepositoryID: repo, Agent: "human_" + repo}, "bad_request")
	refusedWith(t, s, "contact", Request{RepositoryID: repo, Agent: agentName(map[string]any{"agent": map[string]any{"name": "nobody"}})}, "bad_request")
}

// A contact in another repository is refused: it must belong to the repository it answers for.
func TestAContactMustBelongToItsRepository(t *testing.T) {
	s, _, irisOut, _, snapOut := crossRepo(t)
	refusedWith(t, s, "contact", Request{RepositoryID: irisOut["repository_id"].(string), Agent: agentName(snapOut)}, "bad_request")
}

// Two repositories with one name make repo:<name> ambiguous; the id still works.
func TestARepositoryNameSharedByTwoIsAmbiguous(t *testing.T) {
	s, _, irisOut, snap, _ := crossRepo(t)
	other := call(t, s, "sessions", Request{Client: "claude", Host: "work", SessionID: "o", Cwd: "/w/iris", Worktree: "/w/iris", RemoteURL: "https://gitlab.example.com/fork/iris.git", RootCommit: "other-root", Source: "hook"})
	if other["repository_id"] == irisOut["repository_id"] {
		t.Fatal("distinct repositories joined")
	}
	refusedWith(t, s, "messages", sendTo(snap, "amb", "x", "repo:iris"), "ambiguous")
	call(t, s, "messages", sendTo(snap, "id", "x", "repo:"+irisOut["repository_id"].(string)))
	call(t, s, "messages", sendTo(snap, "human", "x", "human:"+irisOut["repository_id"].(string)))
	refusedWith(t, s, "messages", sendTo(snap, "human-bad", "x", "human:nope"), "not_found")
}

// Mail for an agent with no session is queued, and the sender is told.
func TestMailForAnAbsentAgentIsMarkedQueued(t *testing.T) {
	s, irisAgent, irisOut, snap, _ := crossRepo(t)
	call(t, s, "end", irisAgent)
	d := call(t, s, "messages", sendTo(snap, "q", "later", agentName(irisOut)))["deliveries"].([]map[string]any)[0]
	if d["queued"] != true {
		t.Fatal(d)
	}
}

// A reply follows the thread across repositories, but only from someone who wrote
// or received the parent.
func TestOnlyAPartyToAMessageCanReplyToIt(t *testing.T) {
	s, irisAgent, irisOut, snap, snapOut := crossRepo(t)
	sent := call(t, s, "messages", sendTo(snap, "q", "question", agentName(irisOut)))
	reply := sendTo(irisAgent, "a", "answer", agentName(snapOut))
	reply.ReplyTo = sent["message_id"].(string)
	answered := call(t, s, "messages", reply)
	if answered["thread_id"] != sent["thread_id"] {
		t.Fatal(answered)
	}
	third := call(t, s, "sessions", Request{Client: "codex", Host: "home", SessionID: "x", Cwd: "/src/iris", Worktree: "/src/iris", RemoteURL: "https://github.com/team/iris.git", RootCommit: "iris-root", Source: "hook"})
	outsider := Request{ExecutionID: third["execution_id"].(string), Generation: third["generation"].(int64)}
	sneak := sendTo(outsider, "s", "me too", agentName(snapOut))
	sneak.ReplyTo = sent["message_id"].(string)
	refusedWith(t, s, "messages", sneak, "not_found")
}

// The console shows a message in a repository when its sender or a recipient is there.
func TestTheConsoleShowsAMessageWhereItsSenderOrARecipientIs(t *testing.T) {
	s, irisAgent, irisOut, snap, snapOut := crossRepo(t)
	work := call(t, s, "sessions", Request{Client: "claude", Host: "work", SessionID: "w", Cwd: "/w/shop", Worktree: "/w/shop", RemoteURL: "https://github.com/team/shop.git", RootCommit: "shop-root", Source: "hook"})
	// snapper → iris, then iris → shop, in one thread.
	first := call(t, s, "messages", sendTo(snap, "1", "to iris", agentName(irisOut)))
	next := sendTo(irisAgent, "2", "to shop", agentName(work))
	next.ReplyTo = first["message_id"].(string)
	call(t, s, "messages", next)
	count := func(repo string) (int, []string) {
		threads := call(t, s, "ui_threads", Request{RepositoryID: repo})["threads"].([]map[string]any)
		if len(threads) != 1 {
			return 0, nil
		}
		th := call(t, s, "ui_thread", Request{Key: threads[0]["id"].(string), RepositoryID: repo})
		return len(th["messages"].([]map[string]any)), threads[0]["participants"].([]string)
	}
	if n, _ := count(snapOut["repository_id"].(string)); n != 1 {
		t.Fatalf("snapper sees %d", n)
	}
	if n, _ := count(irisOut["repository_id"].(string)); n != 2 {
		t.Fatalf("iris sees %d", n)
	}
	n, who := count(work["repository_id"].(string))
	if n != 1 || len(who) != 2 {
		t.Fatalf("shop sees %d messages, participants %v", n, who)
	}
	if n, _ := count(""); n != 2 {
		t.Fatalf("the whole hub sees %d", n)
	}
}

// The operator replies from the view it reads, and the reply belongs to that repository.
func TestTheOperatorRepliesFromTheRepositoryItReads(t *testing.T) {
	s, _, irisOut, snap, snapOut := crossRepo(t)
	sent := call(t, s, "messages", sendTo(snap, "q", "question", agentName(irisOut)))
	irisRepo := irisOut["repository_id"].(string)
	reply := Request{RepositoryID: irisRepo, ReplyTo: sent["message_id"].(string), Key: "op", Body: "seen", To: []string{agentName(snapOut)}}
	out := call(t, s, "ui_messages", reply)
	var repo string
	if err := s.db.QueryRow("SELECT repository_id FROM Message WHERE id=?", out["message_id"]).Scan(&repo); err != nil || repo != irisRepo {
		t.Fatalf("the reply belongs to %q: %v", repo, err)
	}
	// A message the repository cannot see cannot be replied to from it.
	work := call(t, s, "sessions", Request{Client: "claude", Host: "work", SessionID: "w", Cwd: "/w/shop", Worktree: "/w/shop", RemoteURL: "https://github.com/team/shop.git", RootCommit: "shop-root", Source: "hook"})
	private := call(t, s, "messages", sendTo(snap, "p", "private", agentName(work)))
	blind := Request{RepositoryID: irisRepo, ReplyTo: private["message_id"].(string), Key: "op2", Body: "x", To: []string{agentName(snapOut)}}
	refusedWith(t, s, "ui_messages", blind, "not_found")
}

// A forgotten contact leaves repo: mail with the repository's human.
func TestAForgottenContactFallsBackToTheHuman(t *testing.T) {
	s, irisAgent, irisOut, snap, _ := crossRepo(t)
	repo := irisOut["repository_id"].(string)
	call(t, s, "contact", Request{RepositoryID: repo, Agent: agentName(irisOut)})
	call(t, s, "end", irisAgent)
	call(t, s, "forget", Request{Agent: agentName(irisOut)})
	d := call(t, s, "messages", sendTo(snap, "after", "x", "repo:"+repo))["deliveries"].([]map[string]any)[0]
	if d["recipient"] != "human" {
		t.Fatal(d)
	}
}
