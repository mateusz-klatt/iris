package hook

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mateusz-klatt/iris/internal/hubclient"
)

func TestCursorPrintModeGetsNoFollowUp(t *testing.T) {
	root := gitRepo(t)
	env := failureEnv(t)
	now := hubclient.Attention{Unread: 1, HighWater: 1, Identity: hubclient.Identity{Known: true, Pending: true, Stream: "s", Cursor: 1}}
	calls := 0
	prev := cursorCommandLine
	t.Cleanup(func() { cursorCommandLine = prev })
	for _, args := range [][]string{{"cursor-agent", "-p", "PING"}, {"cursor-agent", "--print", "PING"}, nil} {
		cursorCommandLine = func(int, int64) ([]string, error) {
			if args == nil {
				return nil, os.ErrNotExist
			}
			return args, nil
		}
		before := calls
		out := Runner{
			ClientName: "cursor", EventName: "stop",
			Stdin: strings.NewReader(cursorStopPayload(root, 0)),
			Hub:   attentionHub{now: &now, calls: &calls}, Getenv: env, after: never, logAfter: never,
		}.Run()
		if out.Exit != 0 || strings.Contains(string(out.Stdout), "followup_message") || calls != before {
			t.Fatalf("%v: calls %d->%d %s", args, before, calls, out.Stdout)
		}
	}
}

func TestCursorInteractiveArgs(t *testing.T) {
	if cursorInteractiveArgs(nil) || cursorInteractiveArgs([]string{"cursor-agent", "-p"}) || cursorInteractiveArgs([]string{"cursor-agent", "--print=hi"}) {
		t.Fatal("print mode looked interactive")
	}
	if !cursorInteractiveArgs([]string{"cursor-agent", "--trust", "say PING"}) || !cursorInteractiveArgs([]string{"cursor-agent", "--model", "composer-2.5-fast"}) {
		t.Fatal("an interactive launch was refused")
	}
	if cursorInteractiveArgs([]string{"cursor-agent", "--model"}) || cursorInteractiveArgs([]string{"cursor-agent", "--not-a-flag"}) || cursorInteractiveArgs([]string{"cursor-agent", "mcp"}) {
		t.Fatal("an incomplete, unknown or management command looked interactive")
	}
	if !cursorInteractiveArgs([]string{"cursor-agent", "agent", "--trust"}) || !cursorInteractiveArgs([]string{"cursor-agent", "--worktree", "wt"}) || !cursorInteractiveArgs([]string{"cursor-agent", "--worktree"}) {
		t.Fatal("agent and optional worktree should stay interactive")
	}
	if cursorInteractiveArgs([]string{"cursor-agent", "-H"}) || cursorInteractiveArgs([]string{"cursor-agent", "-q"}) || !cursorInteractiveArgs([]string{"cursor-agent", "-f", "-H", "N: V"}) || !cursorInteractiveArgs([]string{"cursor-agent", "-HName: V"}) || !cursorInteractiveArgs([]string{"cursor-agent", "-w", "wt"}) {
		t.Fatal("short options were classified wrong")
	}
	if cursorInteractiveArgs([]string{"cursor-agent", "-fp"}) {
		t.Fatal("a grouped -p was interactive")
	}
	if !cursorInteractiveArgs([]string{"cursor-agent", "-w"}) || !cursorInteractiveArgs([]string{"cursor-agent", "-w", "-f"}) {
		t.Fatal("a short worktree flag consumed the next option")
	}
	if !cursorInteractiveArgs([]string{"cursor-agent", "--", "prompt"}) {
		t.Fatal("arguments after -- are the prompt")
	}
}

func TestCapturedCursorStopIsTheFirstTurn(t *testing.T) {
	payload, err := os.ReadFile(filepath.Join("testdata", "cursor", "stop.json"))
	if err != nil {
		t.Fatal(err)
	}
	ev, err := Decode(ClientCursor, "stop", payload)
	if err != nil || ev.Kind != KindStop || ev.LoopCount != 0 || ev.SessionID != "c07cffe8-203b-4517-8ece-6dd9f9c8f162" {
		t.Fatalf("%+v %v", ev, err)
	}
	out := Encode(ev, Decision{Action: ActionAllow, Context: cursorFollowup})
	if !strings.Contains(string(out.Stdout), `"followup_message":"`+cursorFollowup+`"`) {
		t.Fatalf("follow-up: %s", out.Stdout)
	}
	quiet := Encode(ev, Decision{Action: ActionAllow})
	if string(quiet.Stdout) != "{}\n" {
		t.Fatalf("quiet stop: %s", quiet.Stdout)
	}
	denied := Encode(ev, Decision{Action: ActionDeny, Reason: "no"})
	if string(denied.Stdout) != "{}\n" {
		t.Fatalf("a stop deny is not a follow-up: %s", denied.Stdout)
	}
}

func cursorStopPayload(root string, loop int) string {
	raw, err := json.Marshal(map[string]any{
		"hook_event_name": "stop",
		"conversation_id": "s",
		"session_id":      "s",
		"status":          "completed",
		"loop_count":      loop,
		"workspace_roots": []string{root},
	})
	if err != nil {
		panic(err)
	}
	return string(raw)
}

func interactiveCursor(t *testing.T) {
	t.Helper()
	prev := cursorCommandLine
	cursorCommandLine = func(int, int64) ([]string, error) {
		return []string{"cursor-agent", "--trust"}, nil
	}
	t.Cleanup(func() { cursorCommandLine = prev })
}

func TestCursorStopFollowsANewerIdentityOnceAndNeverAFollowUp(t *testing.T) {
	interactiveCursor(t)
	root := gitRepo(t)
	env := failureEnv(t)
	now := &hubclient.Attention{}
	calls := 0
	hub := attentionHub{now: now, calls: &calls}
	run := func(loop int) string {
		t.Helper()
		out := Runner{
			ClientName: "cursor", EventName: "stop",
			Stdin: strings.NewReader(cursorStopPayload(root, loop)),
			Hub:   hub, Getenv: env, after: never, logAfter: never,
		}.Run()
		if out.Exit != 0 {
			t.Fatalf("exit %d: %s", out.Exit, out.Stderr)
		}
		return string(out.Stdout)
	}
	if got := run(0); strings.Contains(got, "followup_message") {
		t.Fatalf("nothing pending, yet %s", got)
	}
	*now = hubclient.Attention{
		Unread: 1, HighWater: 4,
		Identity: hubclient.Identity{Known: true, Pending: true, Stream: "stream", Cursor: 4},
	}
	if got := run(0); !strings.Contains(got, `"followup_message":"`+cursorFollowup+`"`) {
		t.Fatalf("newer attention was quiet: %s", got)
	}
	if got := run(0); strings.Contains(got, "followup_message") {
		t.Fatalf("the same identity was said again: %s", got)
	}
	*now = hubclient.Attention{
		Unread: 2, HighWater: 5,
		Identity: hubclient.Identity{Known: true, Pending: true, Stream: "stream", Cursor: 5},
	}
	before := calls
	if got := run(1); strings.Contains(got, "followup_message") || calls != before {
		t.Fatalf("loop_count 1 called the hub or followed up: calls %d->%d %s", before, calls, got)
	}
	*now = hubclient.Attention{
		Unread: 1, HighWater: 1,
		Identity: hubclient.Identity{Known: true, Pending: true, Stream: "stream", Cursor: 2},
	}
	if got := run(0); !strings.Contains(got, "followup_message") {
		t.Fatalf("a restored hub stayed quiet: %s", got)
	}
	*now = hubclient.Attention{Identity: hubclient.Identity{Known: true}}
	if got := run(0); strings.Contains(got, "followup_message") {
		t.Fatalf("a hub with no pending event followed up: %s", got)
	}
}

func TestCursorStopStaysQuietWhenWakeIsOffOrTheHubFails(t *testing.T) {
	root := gitRepo(t)
	home := t.TempDir()
	base := failureEnv(t)
	env := func(k string) string {
		if k == "HOME" {
			return home
		}
		return base(k)
	}
	if err := os.MkdirAll(filepath.Join(home, ".cursor"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".cursor", "iris-wake"), []byte("off\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	now := &hubclient.Attention{Unread: 1, HighWater: 1, Identity: hubclient.Identity{Known: true, Pending: true, Stream: "s", Cursor: 1}}
	calls := 0
	out := Runner{
		ClientName: "cursor", EventName: "stop",
		Stdin: strings.NewReader(cursorStopPayload(root, 0)),
		Hub:   attentionHub{now: now, calls: &calls}, Getenv: env, after: never, logAfter: never,
	}.Run()
	if out.Exit != 0 || strings.Contains(string(out.Stdout), "followup_message") || calls != 0 {
		t.Fatalf("off: exit %d calls %d %s", out.Exit, calls, out.Stdout)
	}
	interactiveCursor(t)
	broken := Runner{
		ClientName: "cursor", EventName: "stop",
		Stdin:  strings.NewReader(cursorStopPayload(root, 0)),
		Hub:    failureHub{stage: "session"},
		Getenv: failureEnv(t), after: never, logAfter: never,
	}.Run()
	if !cursorWakeOn(nil) || !cursorWakeOn(func(string) string { return "" }) {
		t.Fatal("an empty home turned wake off")
	}
	if cursorWakeOn(func(k string) string {
		if k == "USERPROFILE" {
			return home
		}
		return ""
	}) {
		t.Fatal("USERPROFILE off marker was ignored")
	}
	if err := os.Remove(filepath.Join(home, ".cursor", "iris-wake")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(home, ".cursor", "iris-wake"), 0o700); err != nil {
		t.Fatal(err)
	}
	if cursorWakeOn(func(k string) string {
		if k == "HOME" {
			return home
		}
		return ""
	}) {
		t.Fatal("an unreadable marker stayed on")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if (Runner{Getenv: failureEnv(t)}).cursorFollow(ctx, Event{Client: ClientCursor, SessionID: "s"}, *now, 0) {
		t.Fatal("a cancelled hook followed up")
	}
	if (Runner{}).cursorWakeEnabled() != cursorWakeOn(os.Getenv) {
		t.Fatal("a runner without getenv did not use the process environment")
	}
	pending := hubclient.Attention{HighWater: 1, Identity: hubclient.Identity{Known: true, Pending: true, Stream: "s", Cursor: 1}}
	if (Runner{}).cursorFollow(context.Background(), Event{Client: ClientCursor, SessionID: "s"}, pending, 0) {
		t.Fatal("a runner with no sessions directory followed up")
	}
	locked := failureEnv(t)
	release, err := sessionsFor(locked).lock(hookLockBudget)
	if err != nil {
		t.Fatal(err)
	}
	if (Runner{Getenv: locked, logAfter: never}).cursorFollow(context.Background(), Event{Client: ClientCursor, SessionID: "s"}, pending, 0) {
		release()
		t.Fatal("a held sessions lock still followed up")
	}
	release()
	if broken.Exit != 0 || string(broken.Stdout) != "{}\n" {
		t.Fatalf("hub failure: exit %d %s %s", broken.Exit, broken.Stdout, broken.Stderr)
	}
}
