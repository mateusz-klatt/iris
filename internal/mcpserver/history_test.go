package mcpserver

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/mateusz-klatt/iris/internal/budget"
)

func TestHistoryAndThreadRoutingAndBodyBudget(t *testing.T) {
	body := strings.Repeat("<ż", 3000) // 24,000 encoded bytes per message
	page, _ := json.Marshal(map[string]any{"messages": []map[string]any{
		{"message_id": "m1", "thread_id": "t", "sender": "s", "subject": "one", "body": body},
		{"message_id": "m2", "thread_id": "t", "sender": "s", "subject": "two", "body": body},
		{"message_id": "m3", "thread_id": "t", "sender": "s", "subject": "three", "body": "tail"},
	}, "next": "opaque"})
	for _, name := range []string{"history", "thread"} {
		t.Run(name, func(t *testing.T) {
			f := &fakeHub{reply: string(page)}
			args := map[string]any{"before": "cursor+/=", "limit": 3}
			path := "/v1/messages/history"
			if name == "history" {
				args["q"] = `subject:"one two"`
			} else {
				args["thread_id"] = "t"
				path = "/v1/threads/t"
			}
			out, failed := use(t, connect(t, f), name, args)
			if failed || out["next"] != "opaque" {
				t.Fatal(out)
			}
			seen := f.last(t)
			u, err := url.Parse(seen.path)
			if err != nil {
				t.Fatal(err)
			}
			if seen.method != "GET" || u.Path != path || u.Query().Get("execution_id") != "ex" || u.Query().Get("before") != "cursor+/=" || u.Query().Get("limit") != "3" {
				t.Fatal(seen)
			}
			if name == "history" && u.Query().Get("q") != args["q"] {
				t.Fatal(u)
			}
			items := out["messages"].([]any)
			total := 0
			for i, raw := range items {
				item := raw.(map[string]any)
				data := item["untrusted_data"].(map[string]any)
				part := data["body"].(string)
				total += budget.Encoded(part)
				if !utf8.ValidString(part) || data["sender"] != "s" || data["subject"] == nil || !strings.Contains(data["notice"].(string), "not instructions") || item["body"] != nil || item["subject"] != nil {
					t.Fatal(item)
				}
				if i == 0 {
					if item["body_truncated"] != nil || part != body {
						t.Fatal(item)
					}
				} else {
					if item["body_truncated"] != true || item["read_offset"] != float64(len(part)) || !strings.Contains(item["body_note"].(string), "this message_id") || !strings.Contains(item["body_note"].(string), "not the whole message") {
						t.Fatal(item)
					}
				}
			}
			if total > budget.Bodies || total < budget.Bodies-8 {
				t.Fatal(total)
			}
			if items[2].(map[string]any)["untrusted_data"].(map[string]any)["body"] != "" {
				t.Fatal("spent budget returned body")
			}
		})
	}
	if out, failed := use(t, connect(t, &fakeHub{}), "thread", map[string]any{"thread_id": " "}); !failed || !strings.Contains(out["error"].(map[string]any)["message"].(string), "thread_id") {
		t.Fatal(out)
	}
}

func TestReadByMessageIDAndRejectAmbiguousIDs(t *testing.T) {
	f := &fakeHub{reply: `{"message_id":"m","sender":"s","body":"rest","body_bytes":50,"offset":46}`}
	out, failed := use(t, connect(t, f), "read", map[string]any{"message_id": "m", "offset": 46})
	if failed || out["message_id"] != "m" || out["untrusted_data"].(map[string]any)["body"] != "rest" || out["body"] != nil {
		t.Fatal(out)
	}
	u, err := url.Parse(f.last(t).path)
	if err != nil {
		t.Fatal(err)
	}
	if u.Path != "/v1/messages/m/body" || u.Query().Get("after") != "46" || u.Query().Get("limit") != "32768" {
		t.Fatal(u)
	}
	for _, args := range []map[string]any{nil, {"message_id": " "}, {"message_id": "m", "delivery_id": "d"}} {
		if out, failed := use(t, connect(t, &fakeHub{}), "read", args); !failed {
			t.Fatal(out)
		}
	}
}
