package hook

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/mateusz-klatt/iris/internal/hubclient"
)

type retryHub struct{ calls []hubclient.TouchRequest }

func (*retryHub) RegisterSession(context.Context, hubclient.SessionRequest) (hubclient.SessionResponse, error) {
	return hubclient.SessionResponse{ExecutionID: "ex", Generation: 1}, nil
}
func (h *retryHub) Touch(_ context.Context, r hubclient.TouchRequest) (hubclient.TouchResponse, error) {
	h.calls = append(h.calls, r)
	return hubclient.TouchResponse{Action: "allow"}, nil
}
func (*retryHub) EndExecution(context.Context, string, int) error { return nil }

func TestMissingToolCallIDIsStableAcrossRetries(t *testing.T) {
	raw, err := os.ReadFile("testdata/copilot/preToolUse-apply_patch.json")
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err = json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	payload["cwd"] = gitRepo(t)
	first, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	payload["timestamp"] = payload["timestamp"].(float64) + 1
	next, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	h := &retryHub{}
	for _, raw := range [][]byte{first, first, next} {
		out := (Runner{ClientName: "copilot", EventName: "preToolUse", Stdin: bytes.NewReader(raw), Hub: h, after: never}).Run()
		assertAllow(t, ClientCopilot, out)
	}
	if len(h.calls) != 3 {
		t.Fatal(h.calls)
	}
	a, b, c := h.calls[0].ToolCallID, h.calls[1].ToolCallID, h.calls[2].ToolCallID
	if a == "" || a != b || a == c {
		t.Fatalf("IDs %q %q %q", a, b, c)
	}
	digest := sha256.Sum256(first)
	rawKey := "payload:" + hex.EncodeToString(digest[:])
	want := namespaceToolCallID(Event{Client: ClientCopilot, ConversationID: payload["sessionId"].(string), ToolCallID: rawKey, NamespaceVerified: true})
	if a != want || a == rawKey || strings.Contains(a, payload["sessionId"].(string)) {
		t.Fatalf("payload fallback key %q, want namespaced %q", a, want)
	}
}

func TestClaimRetryKeyUsesVerifiedClientConversation(t *testing.T) {
	ev := Event{Client: ClientAntigravity, ConversationID: "conversation-one", ToolCallID: "3", NamespaceVerified: true}
	const want = "0ec558f92f0ad1f9e88b04948b3567d1748ecbff3a0c6eb04fe986b8d0e5a34a"
	if got := namespaceToolCallID(ev); got != want {
		t.Fatalf("namespaceToolCallID = %q, want %q", got, want)
	}
	if got := namespaceToolCallID(ev); got != want {
		t.Fatalf("retry key changed to %q", got)
	}
	second := ev
	second.ConversationID = "conversation-two"
	if got := namespaceToolCallID(second); got == want || len(got) != 64 {
		t.Fatalf("second conversation key = %q, first = %q", got, want)
	}
	otherClient := ev
	otherClient.Client = ClientKimi
	if got := namespaceToolCallID(otherClient); got == want {
		t.Fatal("client is absent from the namespace")
	}
	for _, unverified := range []Event{
		{Client: ClientAntigravity, ConversationID: "conversation-one", ToolCallID: "3"},
		{Client: ClientAntigravity, ToolCallID: "3", NamespaceVerified: true},
	} {
		if got := namespaceToolCallID(unverified); got != "3" {
			t.Fatalf("unverified namespace key = %q, want raw 3", got)
		}
	}
}

func TestConversationNamespaceVerification(t *testing.T) {
	captured, err := os.ReadFile("testdata/grok/SubagentStart.json")
	if err != nil {
		t.Fatal(err)
	}
	subagent, err := Decode(ClientGrok, "SubagentStart", captured)
	if err != nil {
		t.Fatal(err)
	}
	if !subagent.NamespaceVerified || subagent.ConversationID != grokParent || subagent.SessionID != grokChild {
		t.Fatalf("subagent identities = conversation %q session %q verified %v", subagent.ConversationID, subagent.SessionID, subagent.NamespaceVerified)
	}
	for _, tc := range []struct {
		name         string
		client       Client
		payload      string
		conversation string
		verified     bool
	}{
		{name: "matching common aliases", client: ClientGrok, payload: `{"session_id":"one","sessionId":"one"}`, conversation: "one", verified: true},
		{name: "mismatched common aliases", client: ClientGrok, payload: `{"session_id":"one","sessionId":"two"}`, conversation: "one"},
		{name: "unverified camel-only common ID", client: ClientClaude, payload: `{"sessionId":"one"}`, conversation: "one"},
		{name: "matching Cursor aliases", client: ClientCursor, payload: `{"conversation_id":"one","session_id":"one"}`, conversation: "one", verified: true},
		{name: "Cursor conversation alone", client: ClientCursor, payload: `{"conversation_id":"one"}`, conversation: "one", verified: true},
		{name: "mismatched Cursor aliases", client: ClientCursor, payload: `{"conversation_id":"one","session_id":"two"}`, conversation: "one"},
		{name: "missing Cursor conversation", client: ClientCursor, payload: `{"session_id":"one"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ev, err := Decode(tc.client, "PreToolUse", []byte(tc.payload))
			if err != nil {
				t.Fatal(err)
			}
			if ev.ConversationID != tc.conversation || ev.NamespaceVerified != tc.verified {
				t.Fatalf("conversation = %q verified = %v, want %q and %v", ev.ConversationID, ev.NamespaceVerified, tc.conversation, tc.verified)
			}
			ev.ToolCallID = "positional"
			got := namespaceToolCallID(ev)
			if tc.verified && got == "positional" || !tc.verified && got != "positional" {
				t.Fatalf("key = %q for verified=%v", got, tc.verified)
			}
		})
	}
}
