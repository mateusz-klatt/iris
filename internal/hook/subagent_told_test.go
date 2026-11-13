package hook

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// subagentStart is a captured SubagentStart payload with its paths moved under root, in memory,
// and the event it decodes to there.
func subagentStart(t *testing.T, client Client, root string) ([]byte, Event) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", string(client), "SubagentStart.json"))
	if err != nil {
		t.Fatal(err)
	}
	captured, err := Decode(client, "SubagentStart", raw)
	if err != nil {
		t.Fatal(err)
	}
	var payload any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	// A Windows temp path has backslashes, which JSON would escape; git accepts forward slashes.
	moved, err := json.Marshal(rewriteCapturedPaths(payload, captured.CWD, filepath.ToSlash(root)))
	if err != nil {
		t.Fatal(err)
	}
	ev, err := Decode(client, "SubagentStart", moved)
	if err != nil {
		t.Fatal(err)
	}
	return moved, ev
}

// A SubagentStart context reaches the sub-agent's model, not the root session's. Claude Code's
// sub-agent is its root session to Iris (ADR 017): its hooks carry the root's session id and
// name the sub-agent agent_id. So its context must not count as told for the root, whose model
// never saw it, and the root's next prompt still says what is pending. The payload is a real
// capture. A Grok sub-agent has a session of its own, which keeps its own told state.
func TestASubagentContextIsNeverToldForItsRootSession(t *testing.T) {
	root := gitRepo(t)
	env := failureEnv(t)
	now := pending(7, 1, 0)
	calls := 0
	hub := attentionHub{now: &now, calls: &calls}
	run := func(r Runner, payload []byte) string {
		t.Helper()
		r.Stdin, r.Hub, r.Getenv, r.after, r.logAfter = bytes.NewReader(payload), hub, env, never, never
		out := r.Run()
		if out.Exit != 0 {
			t.Fatalf("exit %d: %s", out.Exit, out.Stderr)
		}
		return string(out.Stdout)
	}

	start, ev := subagentStart(t, ClientClaude, root)
	if ev.Kind != KindSubagentStart || ev.ParentSessionID != "" {
		t.Fatalf("the capture no longer decodes to its root session: %+v", ev)
	}
	if got := run(Runner{ClientName: "claude", EventName: "SubagentStart"}, start); !strings.Contains(got, "Unread messages") {
		t.Fatalf("the sub-agent got no context: %s", got)
	}
	if told := toldOf(sessionsFor(env), ClientClaude, ev.SessionID); told.Stream != "" || told.Rev != 0 {
		t.Fatalf("the sub-agent's context was counted as told for the root session: %+v", told)
	}
	prompt := `{"session_id":` + quote(ev.SessionID) + `,"cwd":` + quote(root) + `,"hook_event_name":"UserPromptSubmit","prompt":"go"}`
	if got := run(Runner{ClientName: "claude", EventName: "UserPromptSubmit"}, []byte(prompt)); !strings.Contains(got, "Iris: 1 unread message.") {
		t.Fatalf("the root's prompt did not say what its model was never told: %s", got)
	}

	grok, child := subagentStart(t, ClientGrok, root)
	if child.ParentSessionID == "" {
		t.Fatalf("the Grok capture no longer names a sub-agent session: %+v", child)
	}
	run(Runner{ClientName: "grok", EventName: "SubagentStart", Ancestor: grokAncestor(4242, 1700000000000, "grok")}, grok)
	if told := toldOf(sessionsFor(env), ClientGrok, child.SessionID); told.Stream != testStream || told.Cursor != 7 {
		t.Fatalf("a sub-agent with a session of its own did not keep its told state: %+v", told)
	}
}
