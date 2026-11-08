package hook

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
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
}
