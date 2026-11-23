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

	for _, name := range []string{"claude", "claude-code"} {
		if got := (Runner{ClientName: name}).ancestor(); got != withMarkers {
			t.Fatalf("%s hook walked without markers: %+v", name, got)
		}
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

// v0.1.13 treated Cursor's marker pair as Cursor for every client other than the requested one,
// whatever ancestor was recognized. Only a Claude hook now needs unknown ancestry for the pair.
func TestCursorMarkersKeepTheV0113RuleForOtherClients(t *testing.T) {
	env := map[string]string{"CURSOR_PROJECT_DIR": "/outer/cursor/project", "CURSOR_VERSION": "2026.11.22-e9042b2"}
	codex := func() (proc.Ancestor, error) { return proc.Ancestor{PID: 44, StartMS: 9, Name: "codex"}, nil }
	runner, cross := (Runner{Getenv: func(k string) string { return env[k] }, Ancestor: codex}).crossLoad("s1", ClientKimi)
	if !cross || runner != ClientCursor {
		t.Fatalf("kimi hook under codex with markers: runner %q, cross %v; want cursor, true", runner, cross)
	}
	runner, cross = (Runner{Getenv: func(k string) string { return env[k] }, Ancestor: codex}).crossLoad("s1", ClientClaude)
	if cross || runner != "" {
		t.Fatalf("claude hook under codex with markers: runner %q, cross %v; want native", runner, cross)
	}
}
