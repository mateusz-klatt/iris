package mcpserver

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestReceiptDescriptionExplainsCurrentReachabilityAndItsLimits(t *testing.T) {
	session := connect(t, &fakeHub{})
	list, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range list.Tools {
		if tool.Name != "receipts" {
			continue
		}
		for _, meaning := range []string{
			"pending or inflight",
			"unreachable=true",
			"no active Execution now",
			"cannot predict whether a reply will come",
			"mail stays queued",
			"if the agent returns",
			"Acked or handled deliveries never show it",
		} {
			if !strings.Contains(tool.Description, meaning) {
				t.Fatalf("receipts description does not explain %q: %s", meaning, tool.Description)
			}
		}
		return
	}
	t.Fatal("receipts tool was not listed")
}

func TestReceiptsPreserveEachRecipientsReachabilityWithoutChangingStates(t *testing.T) {
	reply := `{"message_id":"ms_1","recipients":[{"recipient":"lost","repository_id":"rp","state":"pending","at":null,"unreachable":true},{"recipient":"gone","repository_id":"rp","state":"inflight","at":null,"unreachable":true},{"recipient":"live","repository_id":"rp","state":"pending","at":null},{"recipient":"accepted","repository_id":"rp","state":"acked","at":"2027-01-15T08:00:01Z"},{"recipient":"finished","repository_id":"rp","state":"handled","at":"2027-01-15T08:01:01Z"}]}`
	f := &fakeHub{reply: reply}
	out, failed := use(t, connect(t, f), "receipts", map[string]any{"message_id": "ms_1"})
	var want map[string]any
	if err := json.Unmarshal([]byte(reply), &want); err != nil {
		t.Fatal(err)
	}
	if failed || !reflect.DeepEqual(out, want) {
		t.Fatalf("receipt result changed: got %v (failed=%v), want %v", out, failed, want)
	}
	if got := f.last(t); got.method != "GET" || got.path != "/v1/messages/ms_1/status?execution_id=ex" {
		t.Fatalf("receipt request: %s %s", got.method, got.path)
	}
}

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
