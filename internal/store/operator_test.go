package store

import (
	"context"
	"testing"
	"time"
)

func TestOperatorMailThreadsAndOverview(t *testing.T) {
	s, clock, _ := testStore(t)
	a, registered := session(t, s, 1)
	repo := registered["repository_id"].(string)
	name := registered["agent"].(map[string]any)["name"].(string)
	empty := call(t, s, "ui_overview", Request{})
	if empty["human_unread"] != 0 {
		t.Fatal(empty)
	}
	initial := call(t, s, "ui_doctor_get", Request{})
	if initial["checked_at"] != nil {
		t.Fatal(initial)
	}
	r := a
	r.Key = "to-human"
	r.To = []string{"human"}
	r.Subject = "Review required"
	r.Body = "Check this patch"
	sent := call(t, s, "messages", r)
	threadID := sent["thread_id"].(string)
	overview := call(t, s, "ui_overview", Request{})
	if overview["human_unread"] != 1 || len(overview["repositories"].([]map[string]any)) != 1 || len(overview["agents"].([]map[string]any)) != 1 {
		t.Fatal(overview)
	}
	clock.advance(time.Second)
	reply := call(t, s, "ui_messages", Request{Key: "reply", ReplyTo: sent["message_id"].(string), To: []string{name}, Subject: "Re: Review required", Body: "Approved"})
	if reply["thread_id"] != threadID {
		t.Fatal(reply)
	}
	detail := call(t, s, "ui_thread", Request{Key: threadID})
	messages := detail["messages"].([]map[string]any)
	if len(messages) != 2 || messages[1]["sender"] != "human" || messages[1]["body"] != "Approved" || len(messages[0]["recipients"].([]map[string]any)) != 1 {
		t.Fatal(detail)
	}
	for _, r := range []Request{{}, {RepositoryID: repo}, {Agent: name}, {Q: "patch"}, {RepositoryID: repo, Agent: name, Q: "patch", Before: "9999999999999", Limit: 1}} {
		threads := call(t, s, "ui_threads", r)["threads"].([]map[string]any)
		if len(threads) != 1 || threads[0]["count"] != int64(2) || len(threads[0]["participants"].([]string)) != 2 {
			t.Fatal(r, threads)
		}
	}
	if len(call(t, s, "ui_threads", Request{Before: threadID})["threads"].([]map[string]any)) != 0 {
		t.Fatal("pagination repeated thread")
	}
	expectError(t, s, "ui_thread", Request{Key: "missing"}, "not_found")
	for _, r := range []Request{{Key: "empty", Body: "x"}, {Key: "unknown-reply", ReplyTo: "missing", To: []string{name}, Body: "x"}, {Key: "unknown-recipient", To: []string{"missing"}, Body: "x"}} {
		code := "not_found"
		if r.Key == "empty" {
			code = "bad_request"
		}
		expectError(t, s, "ui_messages", r, code)
	}
	call(t, s, "ui_messages", Request{Key: "new-thread", RepositoryID: repo, To: []string{name}, Subject: "Hello", Body: "From operator"})
	call(t, s, "ui_messages", Request{Key: "infer-repo", To: []string{name}, Subject: "Hello", Body: "From inferred repository"})
	claim := call(t, s, "claim", claimRequest(a, "file", "file", "exact"))
	if len(call(t, s, "ui_overview", Request{})["claims"].([]map[string]any)) != 1 {
		t.Fatal("missing overview claim")
	}
	revoke := Request{Fence: number(claim["fence"])}
	call(t, s, "ui_revoke", revoke)
	call(t, s, "ui_revoke", revoke)
	if len(call(t, s, "claims", Request{RepositoryID: repo})["claims"].([]map[string]any)) != 0 {
		t.Fatal("operator revoke failed")
	}
	expectError(t, s, "ui_revoke", Request{Fence: 999999}, "not_found")
	report := Request{CheckedAt: "2026-11-06T00:00:00Z", Clients: []map[string]any{{"status": "ok"}, {"status": "not_configured"}, {"status": "needs_human"}, {"status": "silent"}}}
	call(t, s, "ui_doctor_put", report)
	got := call(t, s, "ui_doctor_get", Request{})
	if got["checked_at"] != report.CheckedAt || len(got["clients"].([]any)) != 4 {
		t.Fatal(got)
	}
	expectError(t, s, "ui_doctor_put", Request{Clients: []map[string]any{{"status": "invalid"}}}, "bad_request")
	if call(t, s, "ui_events", Request{})["cursor"].(int64) == 0 {
		t.Fatal("no UI events")
	}
	err := s.transact(context.Background(), func(tx *transaction) error { _, err := tx.operator("unknown", Request{}); return err })
	if err == nil || err.Error() != "unknown operator operation" {
		t.Fatal(err)
	}
}

func TestTokenStorageFailures(t *testing.T) {
	s, _, _ := testStore(t)
	ctx := context.Background()
	secret, err := s.AddToken(ctx, "host")
	if err != nil {
		t.Fatal(err)
	}
	// A real SQLite trigger rejection must roll back usage and revocation.
	for _, operation := range []string{"INSERT", "UPDATE"} {
		if _, err = s.db.Exec("CREATE TRIGGER deny_token BEFORE " + operation + " ON HostToken BEGIN SELECT RAISE(ABORT,'read only credentials'); END"); err != nil {
			t.Fatal(err)
		}
		if operation == "INSERT" {
			if _, err = s.AddToken(ctx, "new"); err == nil {
				t.Fatal("failed insert succeeded")
			}
		} else {
			if _, err = s.AuthenticateToken(ctx, secret); err == nil {
				t.Fatal("failed usage write authenticated")
			}
			if _, err = s.RevokeToken(ctx, "host"); err == nil {
				t.Fatal("failed revoke succeeded")
			}
		}
		if _, err = s.db.Exec("DROP TRIGGER deny_token"); err != nil {
			t.Fatal(err)
		}
	}
	list, err := s.ListTokens(ctx)
	if err != nil || len(list) != 1 || list[0]["requests"] != int64(0) || list[0]["revoked_ms"] != nil {
		t.Fatal(list, err)
	}
	if _, err = s.db.Exec("DROP TABLE HostToken"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ListTokens(ctx); err == nil {
		t.Fatal("missing token table listed")
	}
	if _, err = s.AuthenticateToken(ctx, secret); err == nil {
		t.Fatal("missing token table authenticated")
	}
	if _, err = s.RevokeToken(ctx, "host"); err == nil {
		t.Fatal("missing token table revoked")
	}
}
