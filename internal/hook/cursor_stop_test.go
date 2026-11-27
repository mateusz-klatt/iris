package hook

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/proc"
)

const (
	cursorDarwinArgvFixture = "argv-interactive-2026.11.22-darwin.json"
	cursorTestScript        = "/home/user/.local/share/cursor-agent/versions/2026.11.22-e9042b2/index.js"
)

func TestCursorPrintModeGetsNoFollowUp(t *testing.T) {
	root := gitRepo(t)
	now := hubclient.Attention{Unread: 1, HighWater: 1, Identity: hubclient.Identity{Known: true, Pending: true, Stream: "s", Cursor: 1}}
	for _, tc := range []struct {
		args []string
		err  error
		why  string
	}{
		{[]string{"cursor-agent", "-p", "PING"}, nil, "print mode"},
		{[]string{"cursor-agent", "--print", "PING"}, nil, "print mode"},
		{nil, os.ErrNotExist, "arguments unreadable"},
	} {
		env := failureEnv(t)
		calls := 0
		readAs(t, tc.args, tc.err, true, nil)
		before := calls
		out := Runner{
			ClientName: "cursor", EventName: "stop",
			Stdin:    strings.NewReader(cursorStopPayload(root, 0)),
			Hub:      attentionHub{now: &now, calls: &calls},
			Getenv:   env,
			Ancestor: cursorAncestor,
			after:    never, logAfter: never,
		}.Run()
		if out.Exit != 0 || strings.Contains(string(out.Stdout), "followup_message") || calls != before {
			t.Fatalf("%v: calls %d->%d %s", tc.args, before, calls, out.Stdout)
		}
		notCursor := Runner{
			ClientName: "cursor", EventName: "stop",
			Stdin: strings.NewReader(cursorStopPayload(root, 0)),
			Hub:   attentionHub{now: &now, calls: &calls}, Getenv: env,
			Ancestor: func() (proc.Ancestor, error) {
				return proc.Ancestor{PID: 1, StartMS: 1, Name: "node"}, nil
			},
			after: never, logAfter: never,
		}.Run()
		if strings.Contains(string(notCursor.Stdout), "followup_message") {
			t.Fatal("a process that is not cursor-agent followed up")
		}
		seen, err := ReadRecentObservations(env("IRIS_TOKEN_FILE"), 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(seen) == 0 || seen[0].Error != wakeNotAsked+tc.why {
			t.Fatalf("observed %+v, want %s", seen, tc.why)
		}
	}
}

func TestCursorModeReadsTheMeasuredGrammar(t *testing.T) {
	if cursorMode([]string{"cursor-agent", "-p"}) != modePrint || cursorMode([]string{"cursor-agent", "--print"}) != modePrint || cursorMode([]string{"cursor-agent", "-pf"}) != modePrint {
		t.Fatal("print mode was not print")
	}
	if cursorMode([]string{"cursor-agent", "--print=hi"}) != modeUnknown || cursorMode([]string{"cursor-agent", "--trust=true"}) != modeUnknown || cursorMode([]string{"cursor-agent", "--not-a-flag"}) != modeUnknown || cursorMode([]string{"cursor-agent", "--model"}) != modeUnknown || cursorMode([]string{"cursor-agent", "-H"}) != modeUnknown || cursorMode([]string{"cursor-agent", "-q"}) != modeUnknown {
		t.Fatal("a rejected flag was accepted")
	}
	live := []string{"cursor-agent", "--use-system-ca", "/home/klattm/.local/share/cursor-agent/versions/2026.11.13-eacade1/index.js", "--resume"}
	if cursorMode(live) != modeInput || cursorMode([]string{"cursor-agent", "--trust", "say PING"}) != modeInput || cursorMode([]string{"cursor-agent", "--model", "composer-2.5-fast"}) != modeInput {
		t.Fatal("an interactive launch was not left to its terminal")
	}
	if cursorMode([]string{"cursor-agent", "mcp"}) != modeCommand || cursorMode([]string{"cursor-agent", "agent"}) != modeCommand {
		t.Fatal("a subcommand looked like a session")
	}
	if cursorMode([]string{"cursor-agent", "--worktree"}) != modeInput || cursorMode([]string{"cursor-agent", "--worktree", "wt"}) != modeInput || cursorMode([]string{"cursor-agent", "-w"}) != modeInput || cursorMode([]string{"cursor-agent", "-w", "wt"}) != modeInput || cursorMode([]string{"cursor-agent", "-w", "-f"}) != modeInput || cursorMode([]string{"cursor-agent", "-f", "-H", "N: V"}) != modeInput || cursorMode([]string{"cursor-agent", "-HName: V"}) != modeInput || cursorMode([]string{"cursor-agent", "--", "prompt"}) != modeInput {
		t.Fatal("optional and attached values were misread")
	}
}

func TestCursorModeReadsCapturedDarwinArgs(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "cursor", cursorDarwinArgvFixture))
	if err != nil {
		t.Fatal(err)
	}
	var args []string
	if err := json.Unmarshal(data, &args); err != nil {
		t.Fatal(err)
	}
	if got := cursorMode(args); got != modeInput {
		t.Fatalf("cursorMode(%q) = %v, want modeInput", args, got)
	}
}

func TestCursorModeReadsLauncherPrefixes(t *testing.T) {
	for _, tc := range []struct {
		name  string
		flags []string
	}{
		{"both", []string{"--disable-sigusr1", "--use-system-ca"}},
		{"system CA only", []string{"--use-system-ca"}},
		{"SIGUSR1 only", []string{"--disable-sigusr1"}},
		{"neither", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resume := slices.Concat([]string{"cursor-agent"}, tc.flags, []string{cursorTestScript, "--resume"})
			if got := cursorMode(resume); got != modeInput {
				t.Errorf("cursorMode(%q) = %v, want modeInput", resume, got)
			}
			print := slices.Concat([]string{"cursor-agent"}, tc.flags, []string{cursorTestScript, "-p", "PING"})
			if got := cursorMode(print); got != modePrint {
				t.Errorf("cursorMode(%q) = %v, want modePrint", print, got)
			}
		})
	}
	windows := []string{"node.exe", `C:\Users\user\cursor-agent\versions\2026.11.22-e9042b2\index.js`, "-p", "PING"}
	if got := cursorMode(windows); got != modePrint {
		t.Errorf("cursorMode(%q) = %v, want modePrint", windows, got)
	}
	windowsWithFlag := slices.Concat(windows[:1], []string{"--use-system-ca"}, windows[1:])
	if got := cursorMode(windowsWithFlag); got != modePrint {
		t.Errorf("cursorMode(%q) = %v, want modePrint", windowsWithFlag, got)
	}
}

func TestCursorModeRejectsMisplacedLauncherFlags(t *testing.T) {
	for _, flags := range [][]string{
		{"--disable-sigusr1"},
		{"--use-system-ca"},
		{cursorTestScript, "--disable-sigusr1"},
		{cursorTestScript, "--use-system-ca"},
		{"--trust", "--disable-sigusr1", cursorTestScript},
		{"--disable-sigusr1", "--unknown", cursorTestScript},
		{"--disable-sigusr1", "--unknown=" + cursorTestScript},
		{"--disable-sigusr1=true", cursorTestScript},
		{"--disable-sigusr1", "index.js"},
		{"--disable-sigusr1", "/tmp/cache/release/index.js"},
		{"--disable-sigusr1", "/tmp/versions//index.js"},
		{"--disable-sigusr1", "/tmp/versions/./index.js"},
		{"--disable-sigusr1", "/tmp/versions/../index.js"},
		{"--disable-sigusr1", "/tmp/versions/release/other.js"},
	} {
		args := append([]string{"cursor-agent"}, flags...)
		if got := cursorMode(args); got != modeUnknown {
			t.Errorf("cursorMode(%q) = %v, want modeUnknown", args, got)
		}
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

func cursorAncestor() (proc.Ancestor, error) {
	return proc.Ancestor{PID: 295482, StartMS: 1, Name: "cursor-agent"}, nil
}

func interactiveCursor(t *testing.T) {
	t.Helper()
	readAs(t, []string{"cursor-agent", "--disable-sigusr1", "--use-system-ca", cursorTestScript, "--resume"}, nil, true, nil)
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
			Hub:   hub, Getenv: env, Ancestor: cursorAncestor, after: never, logAfter: never,
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
		Stdin:    strings.NewReader(cursorStopPayload(root, 0)),
		Hub:      failureHub{stage: "session"},
		Getenv:   failureEnv(t),
		Ancestor: cursorAncestor,
		after:    never, logAfter: never,
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
