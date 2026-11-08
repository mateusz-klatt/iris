package store

import (
	"testing"
	"time"
)

// The delivery-problems list names the human's own mail whose delivery is in trouble, and
// nothing else: a recipient with no active Execution at once, a reachable one only after the
// wait, never a delivery that was acked or said handled, never mail older than the window,
// never mail between agents, and never a body.
func TestDeliveryProblemsNameOnlyTheHumansMailInTrouble(t *testing.T) {
	s, clock, _ := testStore(t)
	old, out := session(t, s, 4)
	call(t, s, "ui_messages", Request{Key: "too-old", RepositoryID: out["repository_id"].(string), To: []string{out["agent"].(map[string]any)["name"].(string)}, Subject: "too-old", Body: "b"})
	call(t, s, "end", Request{ExecutionID: old.ExecutionID, Generation: old.Generation})
	clock.advance(deliveryWindow + time.Minute)
	busy, out := session(t, s, 1)
	repo := out["repository_id"].(string)
	busyName := out["agent"].(map[string]any)["name"].(string)
	gone, out := session(t, s, 2)
	goneName := out["agent"].(map[string]any)["name"].(string)
	reader, out := session(t, s, 3)
	readerName := out["agent"].(map[string]any)["name"].(string)
	other := call(t, s, "sessions", Request{Client: "codex", Host: "host", Cwd: "/other", Worktree: "/other", RemoteURL: "https://example.com/team/other.git", RootCommit: "other", SessionID: "other", Source: "hook"})
	otherName := other["agent"].(map[string]any)["name"].(string)
	call(t, s, "end", Request{ExecutionID: gone.ExecutionID, Generation: gone.Generation})

	human := func(key string, from string, to ...string) {
		call(t, s, "ui_messages", Request{Key: key, RepositoryID: from, To: to, Subject: key, Body: "secret body " + key})
	}
	human("to-all", repo, busyName, goneName, readerName)
	human("elsewhere", other["repository_id"].(string), otherName)
	mail := busy
	mail.Key, mail.To, mail.Subject, mail.Body = "agents", []string{goneName}, "agents", "b"
	call(t, s, "messages", mail)
	for _, d := range call(t, s, "inbox", reader)["deliveries"].([]map[string]any) {
		call(t, s, "ack", Request{ExecutionID: reader.ExecutionID, Generation: reader.Generation, Attempt: d["attempt"].(int64), Key: d["id"].(string)})
	}

	list := func(repository string) []map[string]any {
		got := call(t, s, "ui_deliveries", Request{RepositoryID: repository})
		if got["window_s"] != int64(deliveryWindow/time.Second) || got["wait_s"] != int64(deliveryWait/time.Second) {
			t.Fatalf("thresholds %v", got)
		}
		return got["messages"].([]map[string]any)
	}
	recipients := func(m map[string]any) map[string]string {
		by := map[string]string{}
		for _, r := range m["recipients"].([]map[string]any) {
			by[r["name"].(string)] = r["reason"].(string) + "/" + r["state"].(string) + "/" + r["execution"].(string)
		}
		return by
	}
	// At once, only the recipient with no live session is named.
	got := list(repo)
	if len(got) != 1 || got[0]["subject"] != "to-all" || got[0]["body"] != nil || got[0]["thread_id"] == "" {
		t.Fatalf("listed %v", got)
	}
	if r := recipients(got[0]); len(r) != 1 || r[goneName] != "unreachable/pending/ended" {
		t.Fatalf("recipients %v", r)
	}
	// After the wait, the busy recipient is named too, and the acked one never is.
	// The busy recipient keeps its lease through the wait.
	for waited := time.Duration(0); waited <= deliveryWait; waited += 30 * time.Second {
		clock.advance(30 * time.Second)
		call(t, s, "heartbeat", Request{ExecutionID: busy.ExecutionID, Generation: busy.Generation, Source: "shim"})
	}
	got = list(repo)
	if r := recipients(got[0]); len(got) != 1 || len(r) != 2 || r[busyName] != "waiting/pending/active" || r[readerName] != "" {
		t.Fatalf("after the wait %v", r)
	}
	// The whole-hub view adds the other repository's mail; its own view shows only that.
	if all := list(""); len(all) != 2 {
		t.Fatalf("whole hub %v", all)
	}
	if mine := list(other["repository_id"].(string)); len(mine) != 1 || mine[0]["subject"] != "elsewhere" {
		t.Fatalf("other repository %v", mine)
	}
}
