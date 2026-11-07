package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// These faults use SQLite's actual ABORT semantics. Every failed public operation
// must leave the entire durable state, including retry keys and events, unchanged.
func TestWriteFailuresAreAtomic(t *testing.T) {
	for _, tc := range []struct{ table, verb, op string }{
		{"Repository", "INSERT", "sessions"}, {"RepositoryAlias", "INSERT", "sessions"}, {"Agent", "INSERT", "sessions"}, {"Execution", "INSERT", "sessions"},
		{"Execution", "UPDATE", "heartbeat"}, {"Execution", "UPDATE", "end"}, {"Claim", "INSERT", "claim"}, {"Claim", "UPDATE", "claim-reuse"},
		{"Claim", "UPDATE", "touch"}, {"Claim", "UPDATE", "release"}, {"Message", "INSERT", "messages"}, {"Delivery", "INSERT", "messages"},
		{"Event", "INSERT", "messages"}, {"Operation", "INSERT", "messages"}, {"Cursor", "INSERT", "events"}, {"Delivery", "UPDATE", "inbox"},
		{"Ack", "INSERT", "ack"}, {"DoctorReport", "INSERT", "ui_doctor_put"}, {"ShellSnapshot", "INSERT", "reconcile"},
		{"Claim", "UPDATE", "ui_revoke"}, {"Agent", "INSERT", "ui_messages"}, {"Execution", "INSERT", "ui_messages"},
	} {
		t.Run(tc.table+"/"+tc.verb+"/"+tc.op, func(t *testing.T) {
			s, _, _ := testStore(t)
			a, ar := session(t, s, 1)
			b, br := session(t, s, 2)
			claim := call(t, s, "touch", touchRequest(a, "initial", "held"))
			if claim["action"] != "allow" {
				t.Fatal(claim)
			}
			fences := call(t, s, "claims", Request{RepositoryID: ar["repository_id"].(string)})["claims"].([]map[string]any)
			message := a
			message.Key = "initial-mail"
			message.To = []string{br["agent"].(map[string]any)["name"].(string)}
			message.Body = "persisted"
			call(t, s, "messages", message)
			inbox := call(t, s, "inbox", b)["deliveries"].([]map[string]any)
			if len(inbox) != 1 {
				t.Fatal(inbox)
			}
			op := tc.op
			r := a
			switch op {
			case "sessions":
				r = Request{Client: "codex", Host: "new", SessionID: "new", Cwd: "/new", Worktree: "/new", RemoteURL: "https://new.example/new.git"}
			case "heartbeat":
				r.Source = "shim"
			case "claim":
				r = claimRequest(a, "new-claim", "new", "exact")
			case "claim-reuse":
				op = "claim"
				r = claimRequest(a, "upgrade", "held", "exact")
			case "touch":
				r = touchRequest(a, "renew", "held")
			case "release":
				r.All = true
			case "messages":
				r = message
				r.Key = "new-mail"
			case "events":
				r.ConsumerID = "reader"
			case "inbox":
				r = b
			case "ack":
				r = b
				r.Key = inbox[0]["id"].(string)
				r.Attempt = number(inbox[0]["attempt"])
			case "reconcile":
				r.Kind = "before"
				r.Status = map[string]string{"held": " M"}
			case "ui_revoke":
				r.Fence = number(fences[0]["fence"])
			case "ui_messages":
				r = Request{RepositoryID: ar["repository_id"].(string), Key: "operator", To: message.To, Body: "human"}
			}
			before := databaseState(t, s)
			q := fmt.Sprintf("CREATE TRIGGER write_failure BEFORE %s ON %s BEGIN SELECT RAISE(ABORT,'injected write failure'); END", tc.verb, tc.table)
			if _, err := s.db.Exec(q); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Call(context.Background(), op, r); err == nil {
				t.Fatal("failed write reported success")
			}
			after := databaseState(t, s)
			if before != after {
				t.Fatalf("partial transaction visible\nbefore %s\nafter %s", before, after)
			}
			if _, err := s.db.Exec("DROP TRIGGER write_failure"); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Call(context.Background(), op, r); err != nil {
				t.Fatal("retry did not recover", err)
			}
		})
	}
}
func databaseState(t *testing.T, s *Store) string {
	t.Helper()
	out := map[string][][]any{}
	for _, table := range strings.Fields("Repository RepositoryAlias Agent Execution Claim Message Delivery Event Operation Cursor Ack DoctorReport ShellSnapshot") {
		rows, err := s.db.Query("SELECT * FROM " + table + " ORDER BY rowid")
		if err != nil {
			t.Fatal(err)
		}
		columns, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			values := make([]any, len(columns))
			targets := make([]any, len(columns))
			for i := range values {
				targets[i] = &values[i]
			}
			if err = rows.Scan(targets...); err != nil {
				t.Fatal(err)
			}
			out[table] = append(out[table], values)
		}
		if err = rows.Err(); err != nil {
			t.Fatal(err)
		}
		_ = rows.Close()
	}
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
