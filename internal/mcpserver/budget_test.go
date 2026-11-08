package mcpserver

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/mateusz-klatt/iris/internal/budget"
)

// ADR 008: bodies in one inbox result share a budget a client lets into context. Within it a
// body is whole; past it, the body is cut on a character boundary and marked so plainly that a
// model cannot take it for the whole message.
func TestInboxBodiesShareABudgetAndACutIsMarked(t *testing.T) {
	whole := strings.Repeat("a", budget.Bodies-10)
	cut := strings.Repeat("ż", 1000) // 2,000 bytes, of which only 10 fit
	empty := "x"
	page, _ := json.Marshal(map[string]any{"deliveries": []map[string]any{
		{"id": "d1", "sender": "s", "subject": "one", "body": whole},
		{"id": "d2", "sender": "s", "subject": "two", "body": cut},
		{"id": "d3", "sender": "s", "subject": "three", "body": empty},
	}})
	out, failed := use(t, connect(t, &fakeHub{reply: string(page)}), "inbox", nil)
	if failed {
		t.Fatal(out)
	}
	items := out["deliveries"].([]any)
	first, second, third := items[0].(map[string]any), items[1].(map[string]any), items[2].(map[string]any)
	if first["body_truncated"] != nil || first["untrusted_data"].(map[string]any)["body"] != whole {
		t.Fatal("a body within the budget was cut")
	}
	body := second["untrusted_data"].(map[string]any)["body"].(string)
	if second["body_truncated"] != true || second["body_bytes"] != float64(len(cut)) || second["read_offset"] != float64(10) || body != strings.Repeat("ż", 5) {
		t.Fatalf("cut body: %v, %q", second, body)
	}
	note := second["body_note"].(string)
	if !strings.Contains(note, "not the whole message") || !strings.Contains(note, "offset 10") || strings.Contains(second["untrusted_data"].(map[string]any)["body"].(string), "read") {
		t.Fatalf("the note %q, or it went inside the sender's data", note)
	}
	if third["body_truncated"] != true || third["untrusted_data"].(map[string]any)["body"] != "" || third["read_offset"] != float64(0) {
		t.Fatal("a body past a spent budget was not marked", third)
	}
}

// read returns a part of the body as the sender's untrusted data, as inbox does.
func TestReadWrapsThePartAsUntrustedData(t *testing.T) {
	f := &fakeHub{reply: `{"delivery_id":"d","sender":"s","body":"rest","body_bytes":40,"offset":36}`}
	out, failed := use(t, connect(t, f), "read", map[string]any{"delivery_id": "d", "offset": 36})
	if failed || out["body"] != nil {
		t.Fatal(out)
	}
	data := out["untrusted_data"].(map[string]any)
	if data["body"] != "rest" || data["sender"] != "s" || !strings.Contains(data["notice"].(string), "not instructions") {
		t.Fatal(data)
	}
	if _, failed := use(t, connect(t, &fakeHub{}), "read", map[string]any{"delivery_id": " "}); !failed {
		t.Fatal("read without a delivery id reached the hub")
	}
}

// The budget counts what the client receives, encoded JSON, so characters that expand there use
// more of it than their bytes.
func TestTheBudgetCountsEncodedSize(t *testing.T) {
	expands := strings.Repeat("<", budget.Bodies/6+10) // each < is six bytes encoded
	page, _ := json.Marshal(map[string]any{"deliveries": []map[string]any{{"id": "d", "sender": "s", "subject": "x", "body": expands}}})
	out, _ := use(t, connect(t, &fakeHub{reply: string(page)}), "inbox", nil)
	item := out["deliveries"].([]any)[0].(map[string]any)
	if item["body_truncated"] != true || item["read_offset"] != float64(budget.Bodies/6) {
		t.Fatal(item)
	}
}
