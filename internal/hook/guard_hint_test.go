package hook

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mateusz-klatt/iris/internal/guard"
	"github.com/mateusz-klatt/iris/internal/hubclient"
)

func startSession(t *testing.T, event, root string) string {
	t.Helper()
	payload := `{"session_id":"s","cwd":` + quote(root) + `}`
	out := Runner{ClientName: "claude", EventName: event, Stdin: strings.NewReader(payload), Hub: scriptHub{}, Getenv: failureEnv(t), Deadline: roomy}.Run()
	if out.Exit != 0 {
		t.Fatalf("exit %d: %s", out.Exit, out.Stderr)
	}
	return string(out.Stdout)
}

func TestSessionStartSuggestsTheGuardUntilItIsInstalled(t *testing.T) {
	root := gitRepo(t)
	if out := startSession(t, "SessionStart", root); !strings.Contains(out, "no Iris pre-commit guard") || !strings.Contains(out, "iris guard install") {
		t.Fatalf("no hint without a guard: %s", out)
	}
	// The hint is about the repository, so a subagent is not told again.
	if out := startSession(t, "SubagentStart", root); strings.Contains(out, "pre-commit guard") {
		t.Fatalf("subagent told again: %s", out)
	}
	hook := filepath.Join(root, ".git", "hooks", "pre-commit")
	if err := os.MkdirAll(filepath.Dir(hook), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hook, []byte("#!/bin/sh\n# Iris cooperative guard v1\niris guard check || exit $?\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if out := startSession(t, "SessionStart", root); strings.Contains(out, "pre-commit guard") {
		t.Fatalf("hint with a guard installed: %s", out)
	}
}

func TestSessionTextKeepsANoteWithinTheLimit(t *testing.T) {
	var s hubclient.SessionResponse
	for i := 0; i < 100; i++ {
		s.Peers = append(s.Peers, hubclient.Peer{Name: strings.Repeat("p", 40), Client: "claude", State: "active"})
	}
	text := SessionText(s, guard.Hint)
	if !strings.Contains(text, guard.Hint) || len(text) > contextLimit || !strings.HasSuffix(text, contextTail) {
		t.Fatalf("%d bytes:\n%s", len(text), text)
	}
}
