package store

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/mateusz-klatt/iris/internal/budget"
)

func historySend(t *testing.T, s *Store, from Request, to, key, reply, subject, body string) map[string]any {
	t.Helper()
	from.To, from.Key, from.ReplyTo, from.Subject, from.Body = []string{to}, key, reply, subject, body
	return call(t, s, "messages", from)
}

func TestHistoryVisibilitySearchAndObservation(t *testing.T) {
	s, clock, _ := testStore(t)
	a, ar := session(t, s, 1)
	b, br := session(t, s, 2)
	cReg := call(t, s, "sessions", Request{Client: "codex", Host: "other", SessionID: "c", Worktree: "/other", Cwd: "/other", RemoteURL: "https://elsewhere.example/other.git"})
	c := Request{ExecutionID: cReg["execution_id"].(string), Generation: cReg["generation"].(int64)}
	name := func(r map[string]any) string { return r["agent"].(map[string]any)["name"].(string) }
	first := historySend(t, s, a, name(br), "ab", "", "alpha", "first body")
	clock.advance(time.Millisecond)
	second := historySend(t, s, b, name(cReg), "bc", first["message_id"].(string), "beta", "needle")
	clock.advance(time.Millisecond)
	third := historySend(t, s, c, name(ar), "ca", second["message_id"].(string), "gamma", "needle")
	// Same repository does not grant c's B→C message to a, nor A→B to c.
	before := databaseState(t, s)
	for _, tc := range []struct {
		r   Request
		ids []any
	}{
		{a, []any{third["message_id"], first["message_id"]}},
		{b, []any{second["message_id"], first["message_id"]}},
		{c, []any{third["message_id"], second["message_id"]}},
	} {
		for _, op := range []string{"history", "thread"} {
			r := tc.r
			if op == "thread" {
				r.Key = first["thread_id"].(string)
			}
			items := call(t, s, op, r)["messages"].([]map[string]any)
			ids := []any{}
			for _, m := range items {
				ids = append(ids, m["message_id"])
			}
			if !reflect.DeepEqual(ids, tc.ids) {
				t.Fatalf("%s: %v, want %v", op, ids, tc.ids)
			}
		}
	}
	r := a
	r.Q = "needle"
	items := call(t, s, "history", r)["messages"].([]map[string]any)
	if len(items) != 1 || items[0]["message_id"] != third["message_id"] || items[0]["cross_repository"] != true || items[0]["sent"] != false {
		t.Fatal(items)
	}
	r.Q = "alpha"
	items = call(t, s, "history", r)["messages"].([]map[string]any)
	if len(items) != 1 || items[0]["sent"] != true {
		t.Fatal(items)
	}
	r.Q = "absent"
	if len(call(t, s, "history", r)["messages"].([]map[string]any)) != 0 {
		t.Fatal("nonmatching messages returned")
	}
	for _, who := range []Request{a, b} {
		who.Key = first["message_id"].(string)
		if out := call(t, s, "read_message_body", who); out["body"] != "first body" || out["message_id"] != first["message_id"] {
			t.Fatal(out)
		}
	}
	c.Key = first["message_id"].(string)
	expectError(t, s, "read_message_body", c, "not_found")
	if after := databaseState(t, s); after != before {
		t.Fatal("history changed delivery, attention, acknowledgements or events")
	}
	// An unrelated thread and an unknown thread are indistinguishable.
	private := historySend(t, s, a, name(br), "private", "", "private", "private")
	c.Key = private["thread_id"].(string)
	expectError(t, s, "thread", c, "not_found")
	c.Key = "missing"
	expectError(t, s, "thread", c, "not_found")
	expectError(t, s, "read_message_body", c, "not_found")
	c.Key = ""
	expectError(t, s, "thread", c, "bad_request")
	// Visibility follows the durable mailbox after the original session ends.
	call(t, s, "end", a)
	resumed := call(t, s, "sessions", Request{Client: "codex", Host: "host", SessionID: "resumed", Cwd: "/repo/w1", Worktree: "/repo/w1"})
	if resumed["agent"].(map[string]any)["id"] != ar["agent"].(map[string]any)["id"] {
		t.Fatal(resumed)
	}
	r = Request{ExecutionID: resumed["execution_id"].(string)}
	if len(call(t, s, "history", r)["messages"].([]map[string]any)) != 3 {
		t.Fatal("later execution lost history")
	}
	a.Key = first["thread_id"].(string)
	for _, op := range []string{"history", "thread", "read_message_body"} {
		expectError(t, s, op, a, "lost")
	}
}

func TestHistoryCursorOrderingScopeAndDeletion(t *testing.T) {
	s, clock, _ := testStore(t)
	a, _ := session(t, s, 1)
	b, br := session(t, s, 2)
	_, cr := session(t, s, 3)
	name := br["agent"].(map[string]any)["name"].(string)
	ids := []string{}
	thread := ""
	for i := range 5 {
		out := historySend(t, s, a, name, fmt.Sprint(i), thread, "find", "body")
		ids = append(ids, out["message_id"].(string))
		thread = out["thread_id"].(string)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(ids))) // all five share one millisecond
	r := a
	r.Limit, r.Q = 2, "find"
	first := call(t, s, "history", r)
	items := first["messages"].([]map[string]any)
	if len(items) != 2 || items[0]["message_id"] != ids[0] || items[1]["message_id"] != ids[1] {
		t.Fatal(items)
	}
	r.Before = first["next"].(string)
	// Remove the anchor row; its continuation must still reach every older message.
	if _, err := s.db.Exec("DELETE FROM Delivery WHERE message_id=?", ids[1]); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("DELETE FROM Message WHERE id=?", ids[1]); err != nil {
		t.Fatal(err)
	}
	clock.advance(time.Millisecond)
	historySend(t, s, a, name, "newer", "", "find", "new")
	second := call(t, s, "history", r)
	items = second["messages"].([]map[string]any)
	if len(items) != 2 || items[0]["message_id"] != ids[2] || items[1]["message_id"] != ids[3] {
		t.Fatal(items)
	}
	r.Before = second["next"].(string)
	last := call(t, s, "history", r)
	if last["next"] != "" || last["messages"].([]map[string]any)[0]["message_id"] != ids[4] {
		t.Fatal(last)
	}
	for _, change := range []func(*Request){
		func(r *Request) { r.Q = "other" },
		func(r *Request) { r.ExecutionID = b.ExecutionID },
		func(r *Request) { r.Before = "!" },
		func(r *Request) { r.Before = base64.RawURLEncoding.EncodeToString([]byte("bad json")) },
		func(r *Request) { r.Before = base64.RawURLEncoding.EncodeToString([]byte(`{}`)) },
	} {
		bad := r
		change(&bad)
		expectError(t, s, "history", bad, "bad_request")
	}
	changedThread := r
	changedThread.Key = thread
	expectError(t, s, "thread", changedThread, "bad_request")
	r.Before, r.Q, r.Key, r.Limit = "", "", thread, 2
	count := 0
	for {
		page := call(t, s, "thread", r)
		count += len(page["messages"].([]map[string]any))
		r.Before = page["next"].(string)
		if r.Before == "" {
			break
		}
	}
	if count != 4 {
		t.Fatal(count)
	}
	// Repository/agent selectors are never authority for this view.
	r = a
	r.Agent, r.RepositoryID = cr["agent"].(map[string]any)["name"].(string), "elsewhere"
	if len(call(t, s, "history", r)["messages"].([]map[string]any)) != 5 {
		t.Fatal("selectors altered agent visibility")
	}
	// A quote is ordinary search text, with no matching tokens here.
	r.Q = `"`
	if len(call(t, s, "history", r)["messages"].([]map[string]any)) != 0 {
		t.Fatal("quote-only search returned messages")
	}
}

func TestHistoryPageBudgetAndLimits(t *testing.T) {
	s, _, _ := testStore(t)
	a, _ := session(t, s, 1)
	_, br := session(t, s, 2)
	name := br["agent"].(map[string]any)["name"].(string)
	for i := range 105 {
		historySend(t, s, a, name, fmt.Sprint(i), "", "subject", "body")
	}
	if len(call(t, s, "history", a)["messages"].([]map[string]any)) != 20 {
		t.Fatal("default limit")
	}
	a.Limit = 1000
	if len(call(t, s, "history", a)["messages"].([]map[string]any)) != 100 {
		t.Fatal("maximum limit")
	}
	// Maximal escaped bodies force an HTTP page smaller than the count limit.
	if _, err := s.db.Exec("UPDATE Message SET body=?", strings.Repeat("<", 16384)); err != nil {
		t.Fatal(err)
	}
	seen := 0
	for {
		page := call(t, s, "history", a)
		raw, _ := json.Marshal(page)
		if len(raw) > inboxBudget {
			t.Fatalf("HTTP page has %d bytes", len(raw))
		}
		items := page["messages"].([]map[string]any)
		if len(items) == 0 || len(items) >= 100 {
			t.Fatal(len(items))
		}
		seen += len(items)
		a.Before = page["next"].(string)
		if a.Before == "" {
			break
		}
	}
	if seen != 105 {
		t.Fatal(seen)
	}
}

func TestHistoryRetainsAcceptedMailAndReadsBodyInParts(t *testing.T) {
	s, _, _ := testStore(t)
	a, _ := session(t, s, 1)
	b, br := session(t, s, 2)
	body := "aż" + strings.Repeat("<ż", 3000)
	sent := historySend(t, s, a, br["agent"].(map[string]any)["name"].(string), "long", "", "long", body)
	delivery := call(t, s, "inbox", b)["deliveries"].([]map[string]any)[0]
	b.Key, b.Attempt = delivery["id"].(string), number(delivery["attempt"])
	call(t, s, "ack", b)
	for _, who := range []Request{a, b} {
		items := call(t, s, "history", who)["messages"].([]map[string]any)
		if len(items) != 1 || items[0]["message_id"] != sent["message_id"] {
			t.Fatal("accepted message missing", items)
		}
		who.Key, who.Limit = sent["message_id"].(string), 4001
		var got strings.Builder
		for {
			part := call(t, s, "read_message_body", who)
			chunk := part["body"].(string)
			if !utf8.ValidString(chunk) || budget.Encoded(chunk) > who.Limit || part["body_bytes"] != len(body) {
				t.Fatal(part)
			}
			got.WriteString(chunk)
			if part["next_offset"] == nil {
				break
			}
			who.After = int64(part["next_offset"].(int))
		}
		if got.String() != body {
			t.Fatal("chunks lost body content")
		}
		who.After = 2 // inside ż; the API returns that character from its start
		if part := call(t, s, "read_message_body", who); part["offset"] != 1 || !strings.HasPrefix(part["body"].(string), "ż") {
			t.Fatal(part)
		}
		who.After = int64(len(body))
		if part := call(t, s, "read_message_body", who); part["body"] != "" || part["next_offset"] != nil {
			t.Fatal(part)
		}
		who.After++
		expectError(t, s, "read_message_body", who, "bad_request")
		who.After = -1
		expectError(t, s, "read_message_body", who, "bad_request")
	}
}
