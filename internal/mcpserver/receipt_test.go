package mcpserver

import (
	"testing"
)

// Once the MCP server has the fetch's response it posts the receipt, naming each delivery at
// the attempt the fetch gave it, so the hub quiets the mail; a fetch that returned nothing
// needs none (ADR 010).
func TestAFetchIsFollowedByItsReceipt(t *testing.T) {
	f := &fakeHub{reply: `{"deliveries":[{"id":"dl_1","attempt":2,"sender":"a","subject":"s","body":"b"},{"id":"dl_2","attempt":1,"sender":"a","subject":"s","body":"b"}]}`}
	if out, failed := use(t, connect(t, f), "inbox", nil); failed {
		t.Fatal(out)
	}
	got := f.last(t)
	if got.method != "POST" || got.path != "/v1/inbox/receipt" || got.body["generation"] != float64(3) {
		t.Fatalf("%s %s %v", got.method, got.path, got.body)
	}
	receipts, _ := got.body["receipts"].([]any)
	if len(receipts) != 2 || receipts[0].(map[string]any)["id"] != "dl_1" || receipts[0].(map[string]any)["attempt"] != float64(2) || receipts[1].(map[string]any)["id"] != "dl_2" {
		t.Fatalf("receipts %v", receipts)
	}
	empty := &fakeHub{reply: `{"deliveries":[]}`}
	if out, failed := use(t, connect(t, empty), "inbox", nil); failed {
		t.Fatal(out)
	}
	if last := empty.last(t); last.path != "/v1/inbox?execution_id=ex&receipt=1" {
		t.Fatalf("an empty fetch was followed by %s %s", last.method, last.path)
	}
}
