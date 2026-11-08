package mcpserver

import (
	"encoding/json"
	"testing"
)

func TestMessageWrappersKeepNamedAuthorsInsideUntrustedData(t *testing.T) {
	for _, name := range []string{"inbox", "history", "thread", "read"} {
		t.Run(name, func(t *testing.T) {
			message := map[string]any{"id": "d", "message_id": "m", "thread_id": "t", "sender": "human", "sender_account_id": "ca_person", "sender_username": "Person", "sender_display_name": "Untrusted person label", "reply_address": "human:rp_origin", "subject": "subject", "body": "body"}
			result := map[string]any{}
			args := map[string]any{}
			switch name {
			case "inbox":
				result["deliveries"] = []map[string]any{message}
			case "history":
				result["messages"] = []map[string]any{message}
			case "thread":
				result["messages"] = []map[string]any{message}
				args["thread_id"] = "t"
			case "read":
				result = message
				args["message_id"] = "m"
			}
			raw, _ := json.Marshal(result)
			out, failed := use(t, connect(t, &fakeHub{reply: string(raw)}), name, args)
			if failed {
				t.Fatal("named message wrapper refused")
			}
			item := out
			if name == "inbox" {
				item = out["deliveries"].([]any)[0].(map[string]any)
			}
			if name == "history" || name == "thread" {
				item = out["messages"].([]any)[0].(map[string]any)
			}
			untrusted := item["untrusted_data"].(map[string]any)
			for _, field := range []string{"sender_account_id", "sender_username", "sender_display_name"} {
				if untrusted[field] != message[field] || item[field] != message[field] {
					t.Fatal("wrapper lost immutable person attribution", field)
				}
			}
			if item["reply_address"] != "human:rp_origin" || untrusted["sender"] != "human" || untrusted["body"] != "body" {
				t.Fatal("wrapper lost shared routing or transport sender")
			}
		})
	}
}
