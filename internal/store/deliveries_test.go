package store

import (
	"fmt"
	"strings"
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
		if got["window_s"] != int64(deliveryWindow/time.Second) || got["wait_s"] != int64(deliveryWait/time.Second) || got["more"] != false {
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
	// Fetching now does not restart the wait: it is measured from message creation.
	call(t, s, "inbox", busy)
	if r := recipients(list(repo)[0]); r[busyName] != "waiting/inflight/active" {
		t.Fatalf("after a recent fetch %v", r)
	}
	// The whole-hub view adds the other repository's mail; its own view shows only that.
	if all := list(""); len(all) != 2 {
		t.Fatalf("whole hub %v", all)
	}
	if mine := list(other["repository_id"].(string)); len(mine) != 1 || mine[0]["subject"] != "elsewhere" {
		t.Fatalf("other repository %v", mine)
	}
}

func TestDeliveryProblemsCapCountsFilteredMessagesAndKeepsRecipients(t *testing.T) {
	for _, count := range []int{deliveryMessageLimit, deliveryMessageLimit + 1} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			s, clock, _ := testStore(t)
			gone, out := session(t, s, 1)
			repo := out["repository_id"].(string)
			goneName := out["agent"].(map[string]any)["name"].(string)
			alsoGone, out := session(t, s, 3)
			alsoGoneName := out["agent"].(map[string]any)["name"].(string)
			_, out = session(t, s, 2)
			busyName := out["agent"].(map[string]any)["name"].(string)
			call(t, s, "end", gone)
			call(t, s, "end", alsoGone)
			send := func(key string, to ...string) string {
				clock.advance(time.Millisecond)
				return call(t, s, "ui_messages", Request{Key: key, RepositoryID: repo, To: to, Subject: key, Body: "b"})["message_id"].(string)
			}
			// Fresh mail to a reachable recipient never consumes a slot, including
			// rows after the cap-th eligible message when more must remain false.
			send("oldest-busy", busyName)
			ids := make([]string, count)
			for i := range ids {
				ids[i] = send(fmt.Sprintf("trouble-%d", i), goneName, alsoGoneName, busyName)
				send(fmt.Sprintf("busy-%d", i), busyName)
			}
			got := call(t, s, "ui_deliveries", Request{RepositoryID: repo})
			messages := got["messages"].([]map[string]any)
			if len(messages) != deliveryMessageLimit || got["more"] != (count > deliveryMessageLimit) {
				t.Fatalf("count %d: listed %d, more %v", count, len(messages), got["more"])
			}
			for i, message := range messages {
				if message["id"] != ids[count-1-i] {
					t.Fatalf("message %d: got %v, wanted newest id %s", i, message["id"], ids[count-1-i])
				}
				recipients := message["recipients"].([]map[string]any)
				if len(recipients) != 2 {
					t.Fatalf("message %d recipients: %v", i, recipients)
				}
				byName := map[string]bool{}
				for _, recipient := range recipients {
					byName[recipient["name"].(string)] = recipient["reason"] == "unreachable" && recipient["execution"] == "ended"
				}
				if !byName[goneName] || !byName[alsoGoneName] {
					t.Fatalf("message %d did not keep both recipients in trouble: %v", i, recipients)
				}
			}
		})
	}
}

// Delivery problems must filter out settled deliveries through Ack's covering index before
// looking up Message rows; joining through DeliveryState scanned every historical message first.
func TestDeliveryProblemsFilterAckedBeforeMessageLookup(t *testing.T) {
	s, _, _ := testStore(t)
	rows, err := s.db.Query("EXPLAIN QUERY PLAN "+deliveriesSQL+"1=1 AND 1=1 AND 1=1 ORDER BY m.created_ms DESC,m.id,a.name", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var steps []string
	ackIdx, msgIdx := -1, -1
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		steps = append(steps, detail)
		if strings.Contains(detail, "sqlite_autoindex_Ack_1") && ackIdx < 0 {
			ackIdx = len(steps) - 1
		}
		if strings.Contains(detail, "SEARCH m ") && msgIdx < 0 {
			msgIdx = len(steps) - 1
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if ackIdx < 0 || msgIdx < 0 || ackIdx > msgIdx {
		t.Fatalf("expected Ack filter before Message lookup, plan=%v", steps)
	}
}

// A NULL Ack.delivery_id (permitted by SQLite on rowid PRIMARY KEY columns without NOT NULL)
// must never suppress unacked deliveries, and handled or redelivered deliveries must match
// DeliveryState's semantics identically.
func TestDeliveryProblemsSurvivesNullAckAndMatchesHandledAndRedelivered(t *testing.T) {
	s, clock, _ := testStore(t)
	unacked, out := session(t, s, 1)
	repo := out["repository_id"].(string)
	unackedName := out["agent"].(map[string]any)["name"].(string)
	acked, out := session(t, s, 2)
	ackedName := out["agent"].(map[string]any)["name"].(string)
	handled, out := session(t, s, 3)
	handledName := out["agent"].(map[string]any)["name"].(string)
	redelivered, out := session(t, s, 4)
	redeliveredName := out["agent"].(map[string]any)["name"].(string)

	call(t, s, "ui_messages", Request{
		Key: "eq-check", RepositoryID: repo,
		To: []string{unackedName, ackedName, handledName, redeliveredName}, Subject: "eq-check", Body: "b",
	})

	// Plain ack (handled=false) settles ackedName.
	for _, d := range call(t, s, "inbox", acked)["deliveries"].([]map[string]any) {
		call(t, s, "ack", Request{ExecutionID: acked.ExecutionID, Generation: acked.Generation, Attempt: d["attempt"].(int64), Key: d["id"].(string)})
	}
	// Handled ack (handled=true) settles handledName.
	for _, d := range call(t, s, "inbox", handled)["deliveries"].([]map[string]any) {
		call(t, s, "ack", Request{ExecutionID: handled.ExecutionID, Generation: handled.Generation, Attempt: d["attempt"].(int64), Key: d["id"].(string), Handled: true})
	}
	// Redelivered flow: fetch attempt 1, end Execution (requeues to pending), re-register gen 2, fetch attempt 2 (inflight).
	call(t, s, "inbox", redelivered)
	call(t, s, "end", Request{ExecutionID: redelivered.ExecutionID, Generation: redelivered.Generation})
	rejoined := call(t, s, "sessions", Request{
		Client: "codex", Host: "host", SessionID: "4", Cwd: "/repo/w4", Worktree: "/repo/w4",
		RemoteURL: "https://example.com/team/project.git", RootCommit: "root", Source: "hook",
	})
	redelivered2 := Request{ExecutionID: rejoined["execution_id"].(string), Generation: rejoined["generation"].(int64)}
	redeliveries := call(t, s, "inbox", redelivered2)["deliveries"].([]map[string]any)
	if len(redeliveries) != 1 || redeliveries[0]["attempt"].(int64) != 2 {
		t.Fatalf("expected redelivered attempt 2, got %+v", redeliveries)
	}

	// Insert a NULL delivery_id row into Ack (valid on SQLite rowid PRIMARY KEY without NOT NULL).
	if _, err := s.db.Exec("INSERT INTO Ack(delivery_id,execution_id,attempt,acked_ms) VALUES(NULL,?,1,?)", acked.ExecutionID, clock.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}

	// Advance past deliveryWait while keeping all active sessions alive.
	for waited := time.Duration(0); waited <= deliveryWait; waited += 30 * time.Second {
		clock.advance(30 * time.Second)
		for _, r := range []Request{unacked, acked, handled, redelivered2} {
			call(t, s, "heartbeat", Request{ExecutionID: r.ExecutionID, Generation: r.Generation, Source: "shim"})
		}
	}

	got := call(t, s, "ui_deliveries", Request{RepositoryID: repo})["messages"].([]map[string]any)
	if len(got) != 1 {
		t.Fatalf("expected 1 message with unacked/redelivered recipients despite NULL Ack row, got %+v", got)
	}
	byName := map[string]string{}
	for _, r := range got[0]["recipients"].([]map[string]any) {
		byName[r["name"].(string)] = r["reason"].(string) + "/" + r["state"].(string) + "/" + r["execution"].(string)
	}
	if len(byName) != 2 || byName[unackedName] != "waiting/pending/active" || byName[redeliveredName] != "waiting/inflight/active" {
		t.Fatalf("unexpected recipients with NULL Ack row: %+v", byName)
	}

	// Settling the redelivered attempt 2 removes redeliveredName while keeping unackedName.
	call(t, s, "ack", Request{
		ExecutionID: redelivered2.ExecutionID, Generation: redelivered2.Generation,
		Attempt: redeliveries[0]["attempt"].(int64), Key: redeliveries[0]["id"].(string), Handled: true,
	})
	afterAck := call(t, s, "ui_deliveries", Request{RepositoryID: repo})["messages"].([]map[string]any)
	if len(afterAck) != 1 {
		t.Fatalf("expected 1 message remaining for unackedName, got %+v", afterAck)
	}
	rem := afterAck[0]["recipients"].([]map[string]any)
	if len(rem) != 1 || rem[0]["name"] != unackedName || rem[0]["state"] != "pending" {
		t.Fatalf("expected only unackedName pending after settling redelivered attempt, got %+v", rem)
	}
}
