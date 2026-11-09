package hub

import (
	"fmt"
	"testing"
)

// Over HTTP: a fetch with receipt=1 keeps its mail waiting until POST /v1/inbox/receipt says the
// response arrived, and the receipt does not acknowledge it (ADR 010).
func TestTheFetchReceiptRoutes(t *testing.T) {
	_, s := setup(t)
	a, b := register(t, s, 1), register(t, s, 2)
	send(t, s, a, b, "receipt")
	ex := b["execution_id"].(string)
	pending := func() float64 {
		return request(t, s, "GET", "/v1/events?execution_id="+ex, nil, 200)["pending"].(float64)
	}
	got := request(t, s, "GET", "/v1/inbox?receipt=1&execution_id="+ex, nil, 200)["deliveries"].([]any)
	if len(got) != 1 || pending() != 1 {
		t.Fatalf("fetched %v, pending %v", got, pending())
	}
	d := got[0].(map[string]any)
	request(t, s, "POST", "/v1/inbox/receipt", map[string]any{"execution_id": ex, "generation": b["generation"], "receipts": []any{map[string]any{"id": d["id"], "attempt": d["attempt"]}}}, 200)
	if pending() != 0 {
		t.Fatalf("a received fetch is still pending: %v", pending())
	}
	if again := request(t, s, "GET", fmt.Sprintf("/v1/inbox?execution_id=%s", ex), nil, 200)["deliveries"].([]any); len(again) != 1 {
		t.Fatalf("the receipt settled the delivery: %v", again)
	}
}

// The sender's HTTP receipt observes reachability without settling queued mail.
func TestMessageStatusReportsAnUnreachableRecipient(t *testing.T) {
	_, server := setup(t)
	sender, recipient := register(t, server, 1), register(t, server, 2)
	sent := send(t, server, sender, recipient, "waiting for consent")
	status := "/v1/messages/" + sent["message_id"].(string) + "/status?execution_id=" + sender["execution_id"].(string)
	request(t, server, "POST", "/v1/executions/"+recipient["execution_id"].(string)+"/end", map[string]any{"generation": recipient["generation"]}, 200)
	got := request(t, server, "GET", status, nil, 200)["recipients"].([]any)
	if len(got) != 1 {
		t.Fatal(got)
	}
	receipt := got[0].(map[string]any)
	if receipt["state"] != "pending" || receipt["at"] != nil || receipt["unreachable"] != true || receipt["body"] != nil {
		t.Fatal(receipt)
	}
	returned := register(t, server, 2)
	receipt = request(t, server, "GET", status, nil, 200)["recipients"].([]any)[0].(map[string]any)
	if _, flagged := receipt["unreachable"]; flagged || receipt["state"] != "pending" {
		t.Fatal("a returning recipient stayed unreachable", receipt)
	}
	deliveries := request(t, server, "GET", "/v1/inbox?execution_id="+returned["execution_id"].(string), nil, 200)["deliveries"].([]any)
	if len(deliveries) != 1 || deliveries[0].(map[string]any)["id"] != sent["deliveries"].([]any)[0].(map[string]any)["id"] {
		t.Fatal("observing reachability settled queued mail", deliveries)
	}
}
