package hook

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/hubclient"
)

// attentionHub answers registrations with the attention the test sets, and counts them.
type attentionHub struct {
	scriptHub
	now   *hubclient.Attention
	calls *int
}

func (h attentionHub) RegisterSession(context.Context, hubclient.SessionRequest) (hubclient.SessionResponse, error) {
	*h.calls++
	return hubclient.SessionResponse{ExecutionID: "ex", Generation: 1, Attention: *h.now}, nil
}

func promptPayload(c Client, root string) (event, payload string) {
	switch c {
	case ClientCopilot:
		return "userPromptSubmitted", `{"sessionId":"s","cwd":` + quote(root) + `,"prompt":"go"}`
	case ClientAntigravity:
		return "PreInvocation", `{"conversationId":"s","workspacePaths":[` + quote(root) + `]}`
	default:
		return "UserPromptSubmit", `{"session_id":"s","cwd":` + quote(root) + `,"hook_event_name":"UserPromptSubmit","prompt":"go"}`
	}
}

// The prompt hook says each new event once, as counts, and stays quiet where the session's
// monitor beats (gap review A4).
func TestThePromptHookSaysEachNewEventOnce(t *testing.T) {
	root := gitRepo(t)
	env := failureEnv(t)
	now := &hubclient.Attention{}
	calls := 0
	hub := attentionHub{now: now, calls: &calls}
	prompt := func() string {
		t.Helper()
		event, payload := promptPayload(ClientClaude, root)
		out := Runner{ClientName: "claude", EventName: event, Stdin: strings.NewReader(payload), Hub: hub, Getenv: env, after: never, logAfter: never}.Run()
		if out.Exit != 0 {
			t.Fatalf("exit %d: %s", out.Exit, out.Stderr)
		}
		return string(out.Stdout)
	}
	if got := prompt(); strings.Contains(got, "Iris:") {
		t.Fatalf("nothing new, yet %s", got)
	}
	*now = hubclient.Attention{Unread: 2}
	if got := prompt(); !strings.Contains(got, "Iris: 2 unread messages") || !strings.Contains(got, "not a user message") {
		t.Fatalf("new mail not said: %s", got)
	}
	if got := prompt(); strings.Contains(got, "Iris:") {
		t.Fatalf("said twice: %s", got)
	}
	// Reading mail lowers the count, so later mail is new again.
	*now = hubclient.Attention{Unread: 0}
	prompt()
	*now = hubclient.Attention{Unread: 1, Available: 1}
	if got := prompt(); !strings.Contains(got, "1 unread message;") || !strings.Contains(got, "1 path you were refused is free") {
		t.Fatalf("second round: %s", got)
	}
	// A beating monitor has said it already.
	*now = hubclient.Attention{Unread: 3, Available: 2, Monitored: true}
	if got := prompt(); strings.Contains(got, "Iris:") {
		t.Fatalf("repeated what the monitor said: %s", got)
	}
	// The monitor may have died within its freshness window, so once it is not seen, the
	// events it may not have printed are said (be60dbb0's review of #123).
	*now = hubclient.Attention{Unread: 3, Available: 2}
	if got := prompt(); !strings.Contains(got, "3 unread messages") || !strings.Contains(got, "1 path you were refused is free") {
		t.Fatalf("events seen only while monitored were hidden for good: %s", got)
	}
	if calls != 7 {
		t.Fatalf("%d registrations for 7 prompts", calls)
	}
}

// SessionStart already states the unread count, so the first prompt does not repeat it.
func TestSessionStartCountsAsSaid(t *testing.T) {
	root := gitRepo(t)
	env := failureEnv(t)
	now := &hubclient.Attention{Unread: 4}
	calls := 0
	hub := attentionHub{now: now, calls: &calls}
	start := `{"session_id":"s","cwd":` + quote(root) + `,"hook_event_name":"SessionStart"}`
	Runner{ClientName: "claude", EventName: "SessionStart", Stdin: strings.NewReader(start), Hub: hub, Getenv: env, after: never, logAfter: never}.Run()
	event, payload := promptPayload(ClientClaude, root)
	out := Runner{ClientName: "claude", EventName: event, Stdin: strings.NewReader(payload), Hub: hub, Getenv: env, after: never, logAfter: never}.Run()
	if strings.Contains(string(out.Stdout), "Iris:") {
		t.Fatalf("SessionStart's count repeated: %s", out.Stdout)
	}
}

// SessionStart counts as told for everything pending, freed paths included, so its context states
// them as it states the unread count. It used to leave them out while counting them as told, and a
// path freed for a resumed session was then never announced (ADR 017). With none freed, the context
// has no such line.
func TestSessionStartStatesTheFreedPathsItCountsAsSaid(t *testing.T) {
	root := gitRepo(t)
	env := failureEnv(t)
	now := &hubclient.Attention{Available: 2, HighWater: hubHighWater, Identity: hubclient.Identity{Known: true, Pending: true, Stream: testStream, Cursor: 7}}
	calls := 0
	hub := attentionHub{now: now, calls: &calls}
	run := func(event, payload string) string {
		t.Helper()
		out := Runner{ClientName: "claude", EventName: event, Stdin: strings.NewReader(payload), Hub: hub, Getenv: env, after: never, logAfter: never}.Run()
		if out.Exit != 0 {
			t.Fatalf("exit %d: %s", out.Exit, out.Stderr)
		}
		return string(out.Stdout)
	}
	start := `{"session_id":"s","cwd":` + quote(root) + `,"hook_event_name":"SessionStart","source":"resume"}`
	if got := run("SessionStart", start); !strings.Contains(got, "Paths you were refused that are free now: 2. Use the claims tool.") {
		t.Fatalf("the context leaves out freed paths that it counts as told: %s", got)
	}
	if got := run(promptPayload(ClientClaude, root)); strings.Contains(got, "Iris:") {
		t.Fatalf("the freed paths SessionStart stated were repeated: %s", got)
	}
	*now = hubclient.Attention{Unread: 1}
	if got := run("SessionStart", start); strings.Contains(got, "Paths you were refused") {
		t.Fatalf("a freed-paths line with none freed: %s", got)
	}
}

// Only clients whose prompt hook reaches the model make the call; the others make none.
func TestOnlyClientsWithPromptContextAsk(t *testing.T) {
	root := gitRepo(t)
	for _, c := range []Client{ClientClaude, ClientCodex, ClientCopilot, ClientKimi, ClientAntigravity, ClientGrok, ClientCursor} {
		calls := 0
		hub := attentionHub{now: &hubclient.Attention{Unread: 1}, calls: &calls}
		event, payload := promptPayload(c, root)
		out := Runner{ClientName: string(c), EventName: event, Stdin: strings.NewReader(payload), Hub: hub, Getenv: failureEnv(t), after: never, logAfter: never}.Run()
		want := carriesPromptContext(c)
		if (calls == 1) != want {
			t.Errorf("%s: %d calls, carries context %v", c, calls, want)
		}
		if want && !strings.Contains(string(out.Stdout), "1 unread message") {
			t.Errorf("%s: note missing from %q", c, out.Stdout)
		}
	}
}

// A failed registration on a prompt adds nothing and keeps the session usable (D3).
func TestAPromptHookThatCannotReachTheHubStaysQuiet(t *testing.T) {
	root := gitRepo(t)
	event, payload := promptPayload(ClientCodex, root)
	out := Runner{ClientName: "codex", EventName: event, Stdin: strings.NewReader(payload), Hub: scriptHub{regErr: context.DeadlineExceeded}, Getenv: failureEnv(t), after: never, logAfter: never}.Run()
	if out.Exit != 0 || strings.Contains(string(out.Stdout), "Iris:") {
		t.Fatalf("exit %d: %s", out.Exit, out.Stdout)
	}
	var v any
	if json.Unmarshal(out.Stdout, &v) != nil {
		t.Fatalf("invalid output %q", out.Stdout)
	}
}

// The told state lives next to the generation, in its own file, so neither write can roll the
// other back. Dropping a stale generation keeps it; the session's end removes it.
func TestMemoryKeepsTheToldStateBesideTheGeneration(t *testing.T) {
	m := fileMemory{dir: t.TempDir(), now: time.Now, restrict: func(string) error { return nil }}
	m.Remember("codex", "s", 5)
	if err := m.updateAttention("codex", "s", func(s *toldState) bool { s.Latest, s.Told = Counts{3, 1}, Counts{2, 1}; return true }); err != nil {
		t.Fatal(err)
	}
	m.Remember("codex", "s", 6)
	told := parseTold(m.read(m.counts("codex", "s")))
	if m.Recall("codex", "s") != 6 || told.Latest != (Counts{3, 1}) || told.Told != (Counts{2, 1}) {
		t.Fatal(m.Recall("codex", "s"), told)
	}
	if other := parseTold(m.read(m.counts("codex", "other"))); other != (toldState{}) {
		t.Fatal(other)
	}
	m.Forget("codex", "s")
	if _, err := os.Stat(m.counts("codex", "s")); err != nil {
		t.Fatalf("dropping the generation took the told state: %v", err)
	}
	if err := m.forgetAttention("codex", "s"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(m.counts("codex", "s")); !os.IsNotExist(err) {
		t.Fatalf("the session's end left the told state: %v", err)
	}
}

// attentionTouchHub answers a touch with the attention the test sets.
type attentionTouchHub struct {
	attentionHub
	touchAt *hubclient.Attention
}

func (h attentionTouchHub) Touch(context.Context, hubclient.TouchRequest) (hubclient.TouchResponse, error) {
	return hubclient.TouchResponse{Action: "allow", Attention: *h.touchAt}, nil
}

// An allowed edit carries the note on clients that show pre-edit context, at no extra request,
// and never on a refused edit or on clients that do not show it.
func TestAnAllowedEditSaysNewEventsOnClientsThatShowIt(t *testing.T) {
	for _, c := range []Client{ClientCodex, ClientClaude, ClientKimi} {
		root := gitRepo(t)
		calls := 0
		at := &hubclient.Attention{Unread: 1}
		hub := attentionTouchHub{attentionHub: attentionHub{now: &hubclient.Attention{}, calls: &calls}, touchAt: at}
		out := Runner{ClientName: string(c), EventName: "PreToolUse", Stdin: strings.NewReader(editPayload(c, root)), Hub: hub, Getenv: failureEnv(t), after: never, logAfter: never}.Run()
		said := strings.Contains(string(out.Stdout)+string(out.Stderr), "1 unread message")
		if said != carriesToolContext(c) {
			t.Fatalf("%s: said %v, carries %v: %s", c, said, carriesToolContext(c), out.Stdout)
		}
	}
}
