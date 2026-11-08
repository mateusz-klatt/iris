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
