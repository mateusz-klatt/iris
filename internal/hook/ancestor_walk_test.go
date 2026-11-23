package hook

import (
	"testing"

	"github.com/mateusz-klatt/iris/internal/proc"
)

// Only a Claude hook walks with Cursor's marker pair. Any other client walks as v0.1.13 did,
// so a native Kimi or Grok hook under a bare `agent` with inherited markers is not skipped as
// a cross-loaded Cursor hook.
func TestOnlyAClaudeHookWalksWithCursorMarkers(t *testing.T) {
	withMarkers := proc.Ancestor{PID: 9, StartMS: 1, Name: "agent"}
	withoutMarkers := proc.Ancestor{PID: 2, StartMS: 1, Name: "kimi"}
	origNearest, origPlain := nearestAncestor, plainAncestor
	t.Cleanup(func() { nearestAncestor, plainAncestor = origNearest, origPlain })
	nearestAncestor = func() (proc.Ancestor, error) { return withMarkers, nil }
	plainAncestor = func() (proc.Ancestor, error) { return withoutMarkers, nil }

	if got := (Runner{ClientName: string(ClientClaude)}).ancestor(); got != withMarkers {
		t.Fatalf("claude hook walked without markers: %+v", got)
	}
	for _, client := range []Client{ClientKimi, ClientGrok, ClientCursor, ClientCodex, ClientAntigravity, ClientCopilot} {
		if got := (Runner{ClientName: string(client)}).ancestor(); got != withoutMarkers {
			t.Fatalf("%s hook walked with Cursor markers: %+v", client, got)
		}
	}
	if a, ok := (Runner{ClientName: string(ClientAntigravity)}).antigravityAncestor(); ok || a != (proc.Ancestor{}) {
		t.Fatalf("antigravity wake used a non-antigravity ancestor: %+v, %v", a, ok)
	}
}
