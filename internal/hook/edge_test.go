package hook

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/gitinfo"
	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/proc"
)

type scriptHub struct {
	regErr, endErr, touchErr error
	touch                    hubclient.TouchResponse
	sess                     *hubclient.SessionRequest
	ended                    *int
	message                  string
	phase                    *string
	reconcileErr             error
}

func (h scriptHub) RegisterSession(_ context.Context, r hubclient.SessionRequest) (hubclient.SessionResponse, error) {
	if h.sess != nil {
		*h.sess = r
	}
	return hubclient.SessionResponse{ExecutionID: "ex"}, h.regErr
}

func (h scriptHub) Touch(context.Context, hubclient.TouchRequest) (hubclient.TouchResponse, error) {
	return h.touch, h.touchErr
}

func (h scriptHub) EndExecution(context.Context, string, int) error {
	if h.ended != nil {
		*h.ended++
	}
	return h.endErr
}

func (h scriptHub) Reconcile(_ context.Context, r hubclient.ReconcileRequest) (hubclient.CheckResponse, error) {
	if h.phase != nil {
		*h.phase = r.Kind
	}
	return hubclient.CheckResponse{Message: h.message}, h.reconcileErr
}

// waitForFailure reads the entry a run has just written. With logAfter set to never, Run
// returns only after the write, so there is nothing to wait for.
func waitForFailure(t *testing.T, token string) Failure {
	t.Helper()
	entries, err := ReadRecentFailures(token, 10)
	if err != nil || len(entries) == 0 {
		t.Fatalf("no failure was recorded: %v", err)
	}
	return entries[len(entries)-1]
}

// never is a deadline that does not fire. A test that is not about a deadline uses it, so a
// slow machine cannot turn its decision into a fail-open. The test binary's own timeout
// bounds a real hang. A test about a deadline fires it explicitly instead.
func never(time.Duration) <-chan time.Time { return nil }

func run(hub Hub, event, payload string, env func(string) string) Output {
	return Runner{ClientName: "claude", EventName: event, Stdin: strings.NewReader(payload), Hub: hub, Getenv: env, after: never, logAfter: never}.Run()
}

func TestSessionStartWithoutTheHubFailsOpenAndIsRecorded(t *testing.T) {
	root := gitRepo(t)
	env := failureEnv(t)
	payload := `{"session_id":"s","cwd":` + quote(root) + `}`
	out := run(scriptHub{regErr: errors.New("down")}, "SessionStart", payload, env)
	assertAllow(t, ClientClaude, out)
	if got := waitForFailure(t, env("IRIS_TOKEN_FILE")); got.Stage != "session" {
		t.Fatal(got)
	}
}

func TestSessionEndCannotReachTheHub(t *testing.T) {
	root := gitRepo(t)
	env := failureEnv(t)
	ended := 0
	payload := `{"session_id":"s","cwd":` + quote(root) + `}`
	out := run(scriptHub{regErr: errors.New("down"), ended: &ended}, "SessionEnd", payload, env)
	assertAllow(t, ClientClaude, out)
	if ended != 0 {
		t.Fatal("ended an unknown execution")
	}
}

func TestSessionEndEndsTheExecution(t *testing.T) {
	root := gitRepo(t)
	ended := 0
	payload := `{"session_id":"s","cwd":` + quote(root) + `}`
	out := run(scriptHub{ended: &ended}, "SessionEnd", payload, failureEnv(t))
	assertAllow(t, ClientClaude, out)
	if ended != 1 {
		t.Fatalf("ended %d times", ended)
	}
}

func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func TestTouchAnswersReachTheClient(t *testing.T) {
	root := gitRepo(t)
	payload := `{"session_id":"s","cwd":` + quote(root) + `,"tool_name":"Write","tool_input":{"file_path":"a.go"}}`
	for action, want := range map[string]string{
		"warn":  "held by someone",
		"allow": "held by someone",
	} {
		t.Run(action, func(t *testing.T) {
			hub := scriptHub{touch: hubclient.TouchResponse{Action: action, Message: "held by someone"}}
			out := run(hub, "PreToolUse", payload, failureEnv(t))
			if !strings.Contains(string(out.Stdout), want) {
				t.Fatalf("%s", out.Stdout)
			}
		})
	}
}

func TestDecisionFromTouch(t *testing.T) {
	cases := []struct {
		in   hubclient.TouchResponse
		want Decision
	}{
		{hubclient.TouchResponse{Action: "DENY", Message: "m"}, Decision{Action: ActionDeny, Reason: "m"}},
		{hubclient.TouchResponse{Action: "ask", Message: "m"}, Decision{Action: ActionAsk, Reason: "m"}},
		{hubclient.TouchResponse{Action: "warn", Message: "m"}, Decision{Action: ActionWarn, Reason: "m"}},
		{hubclient.TouchResponse{Action: "allow", Message: "m"}, Decision{Action: ActionAllow, Context: "m"}},
		{hubclient.TouchResponse{Action: "allow"}, Decision{Action: ActionAllow}},
	}
	for _, tc := range cases {
		if got := decisionFromTouch(tc.in); got != tc.want {
			t.Errorf("%+v: %+v", tc.in, got)
		}
	}
}

func TestCrossLoadAppliesWhenThePayloadCannotBeDecoded(t *testing.T) {
	out := Runner{
		ClientName: "antigravity", EventName: "PreToolUse",
		Stdin: strings.NewReader(`{"conversationId":"s","stepIdx":"not a number"}`),
		Getenv: func(k string) string {
			if k == "GROK_SESSION_ID" {
				return "s"
			}
			return ""
		},
		after: never,
	}.Run()
	// Grok's allow, not Antigravity's, because Grok is the real runner.
	if string(out.Stdout) != "{}\n" {
		t.Fatalf("%q", out.Stdout)
	}
}

func TestRunWithoutStdinFailsOpen(t *testing.T) {
	out := Runner{ClientName: "cursor", Getenv: failureEnv(t), after: never, logAfter: never}.Run()
	assertAllow(t, ClientCursor, out)
}

func TestRepoForTriesEachDirectoryOnce(t *testing.T) {
	var asked []string
	r := Runner{Find: func(dir string) (gitinfo.Repo, error) {
		asked = append(asked, dir)
		return gitinfo.Repo{}, gitinfo.ErrNotRepo
	}}
	_, err := r.repoFor(Event{CWD: "/a", Workspaces: []string{"/a", "", "/b"}})
	if !errors.Is(err, gitinfo.ErrNotRepo) || strings.Join(asked, ",") != "/a,/b" {
		t.Fatal(err, asked)
	}
}

func TestAncestorFailureSendsNoProcess(t *testing.T) {
	r := Runner{Ancestor: func() (proc.Ancestor, error) { return proc.Ancestor{PID: 5}, errors.New("gone") }}
	if got := r.ancestor(); got != (proc.Ancestor{}) {
		t.Fatalf("%+v", got)
	}
}

func TestClaimPathsKeepsARenameOnlyWhenBothEndsAreInside(t *testing.T) {
	repo := gitinfo.Repo{Worktree: "/r"}
	r := Runner{}
	_, from, to := r.claimPaths(repo, Event{RenameFrom: "/r/a", RenameTo: "/r/b", Paths: []string{"/r/a"}})
	if from != "a" || to != "b" {
		t.Fatal(from, to)
	}
	_, from, to = r.claimPaths(repo, Event{RenameFrom: "/r/a", RenameTo: "/elsewhere/b"})
	if from != "" || to != "" {
		t.Fatal(from, to)
	}
	_, from, to = r.claimPaths(repo, Event{RenameFrom: "/elsewhere/a", RenameTo: "/r/b"})
	if from != "" || to != "" {
		t.Fatal(from, to)
	}
}

func shellRunner(hub Hub, root string) (Runner, Event) {
	// A context deadline cannot be one that never fires, so these tests give the shell budget
	// an hour, past the test binary's own timeout. The budget has its own test.
	return Runner{Hub: hub, Status: func(context.Context, string, gitinfo.Scope, *gitinfo.Attempt) (map[string]string, error) {
			return map[string]string{}, nil
		}, ShellDeadline: time.Hour},
		Event{Client: ClientClaude, Kind: KindPreTool, SessionID: "s", CWD: root, ToolClass: ClassShell}
}

func isAllow(out Output) bool { return string(out.Stdout) == "{}\n" }

func TestReconcileFailsOpen(t *testing.T) {
	root := gitRepo(t)
	ctx := context.Background()
	t.Run("repository", func(t *testing.T) {
		r, ev := shellRunner(scriptHub{}, root)
		r.Find = func(string) (gitinfo.Repo, error) { return gitinfo.Repo{}, errors.New("unreadable") }
		if !isAllow(r.reconcile(ctx, ev, &runState{})) {
			t.Fatal("not an allow")
		}
	})
	t.Run("panic", func(t *testing.T) {
		r, ev := shellRunner(scriptHub{}, root)
		r.Find = func(string) (gitinfo.Repo, error) { panic("dependency") }
		if !isAllow(r.reconcile(ctx, ev, &runState{})) {
			t.Fatal("not an allow")
		}
	})
	t.Run("session", func(t *testing.T) {
		r, ev := shellRunner(scriptHub{regErr: errors.New("down")}, root)
		if !isAllow(r.reconcile(ctx, ev, &runState{})) {
			t.Fatal("not an allow")
		}
	})
	t.Run("status", func(t *testing.T) {
		r, ev := shellRunner(scriptHub{}, root)
		r.Status = func(context.Context, string, gitinfo.Scope, *gitinfo.Attempt) (map[string]string, error) {
			return nil, errors.New("git")
		}
		if !isAllow(r.reconcile(ctx, ev, &runState{})) {
			t.Fatal("not an allow")
		}
	})
}

func TestReconcileReportsItsPhase(t *testing.T) {
	root := t.TempDir()
	init := exec.Command("git", "init")
	init.Dir = root
	if out, err := init.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	for kind, want := range map[Kind]string{KindPreTool: "before", KindPostTool: "after"} {
		phase := ""
		r, ev := shellRunner(scriptHub{phase: &phase, message: "changed"}, root)
		r.Status = nil // the real git status, within shellRunner's hour
		ev.Kind = kind
		out := r.reconcile(context.Background(), ev, &runState{})
		if phase != want || !strings.Contains(string(out.Stdout), "changed") {
			t.Fatalf("%s: %q %s", kind, phase, out.Stdout)
		}
	}
}

func TestEncodeForEveryAnswer(t *testing.T) {
	cases := []struct {
		name string
		ev   Event
		d    Decision
		want string
		exit int
	}{
		{"claude ask", Event{Client: ClientClaude, Kind: KindPreTool}, Decision{Action: ActionAsk, Reason: "r"}, `"permissionDecision":"ask"`, 0},
		{"copilot ask", Event{Client: ClientCopilot, Kind: KindPreTool}, Decision{Action: ActionAsk, Reason: "r"}, `"permissionDecision":"ask"`, 0},
		{"cursor warn", Event{Client: ClientCursor, Kind: KindPreTool}, Decision{Action: ActionWarn, Reason: "r"}, `"user_message":"r"`, 0},
		{"cursor allow text", Event{Client: ClientCursor, Kind: KindPreTool}, Decision{Action: ActionAllow, Context: "c"}, `"agent_message":"c"`, 0},
		{"antigravity ask", Event{Client: ClientAntigravity, Kind: KindPreTool}, Decision{Action: ActionAsk, Reason: "r"}, `"decision":"ask"`, 0},
		{"antigravity deny later", Event{Client: ClientAntigravity, Kind: KindStop}, Decision{Action: ActionDeny, Reason: "r"}, `"decision":"deny"`, 0},
		{"grok ask", Event{Client: ClientGrok, Kind: KindPreTool}, Decision{Action: ActionAsk, Reason: "r"}, `"decision":"ask"`, 0},
		{"kimi allow", Event{Client: ClientKimi, Kind: KindPreTool}, Decision{Action: ActionAllow, Context: "c\n"}, "c\n", 0},
		{"codex ask is refused", Event{Client: ClientCodex, Kind: KindPreTool}, Decision{Action: ActionAsk}, askHuman, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := Encode(tc.ev, tc.d)
			if !strings.Contains(string(out.Stdout), tc.want) || out.Exit != tc.exit {
				t.Fatalf("%q exit %d", out.Stdout, out.Exit)
			}
		})
	}
}

func TestKimiDenyHasNoMessageWithoutAReason(t *testing.T) {
	out := Encode(Event{Client: ClientKimi}, Decision{Action: ActionDeny})
	if out.Exit != 2 || !strings.Contains(string(out.Stderr), askHuman) && len(out.Stderr) != 0 {
		t.Fatalf("%+v", out)
	}
	if got := withNL(""); got != nil {
		t.Fatal(got)
	}
}

func TestUnknownClientIsNeitherDecodedNorEncoded(t *testing.T) {
	if _, err := Decode("emacs", "Stop", []byte("{}")); !errors.Is(err, errNoSession) {
		t.Fatal(err)
	}
	if out := Encode(Event{Client: "emacs"}, Decision{Action: ActionDeny}); string(out.Stdout) != "{}\n" {
		t.Fatalf("%q", out.Stdout)
	}
}

func TestJSONLineStaysValidWhenAValueCannotEncode(t *testing.T) {
	if got := string(jsonLine(make(chan int))); got != "{}\n" {
		t.Fatalf("%q", got)
	}
}

func TestClaudeEventNames(t *testing.T) {
	for kind, want := range map[Kind]string{
		KindSessionStart: "SessionStart", KindSessionEnd: "SessionEnd", KindPrompt: "UserPromptSubmit",
		KindPostTool: "PostToolUse", KindPostToolFailure: "PostToolUseFailure", KindStop: "Stop",
		KindSubagentStart: "SubagentStart", KindSubagentStop: "SubagentStop", KindPreTool: "PreToolUse",
	} {
		if got := claudeEventName(kind); got != want {
			t.Errorf("%s: %s", kind, got)
		}
	}
}

func TestDecodeRefusals(t *testing.T) {
	for _, client := range []Client{ClientAntigravity, ClientCopilot, ClientCursor} {
		if _, err := Decode(client, "Stop", []byte("{}")); !errors.Is(err, errNoSession) {
			t.Errorf("%s: %v", client, err)
		}
	}
	if _, err := Decode(ClientAntigravity, "Stop", []byte("{")); err == nil {
		t.Error("broken antigravity payload")
	}
	if _, err := Decode(ClientCursor, "Stop", []byte("{")); err == nil {
		t.Error("broken cursor payload")
	}
}

func TestCursorTakesItsEventFromThePayload(t *testing.T) {
	ev, err := Decode(ClientCursor, "", []byte(`{"session_id":"s","hook_event_name":"afterFileEdit","file_path":"a"}`))
	if err != nil {
		t.Fatal(err)
	}
	if ev.Kind != KindPostTool || ev.ToolClass != ClassEdit {
		t.Fatalf("%+v", ev)
	}
}

func TestKinds(t *testing.T) {
	if got := kindFrom("", "stop"); got != KindStop {
		t.Fatal(got)
	}
	if got := kindFrom("nonsense", "more"); got != KindOther {
		t.Fatal(got)
	}
}

func TestPeekSessionID(t *testing.T) {
	if got := peekSessionID([]byte(`{"conversation_id":"c"}`)); got != "c" {
		t.Fatal(got)
	}
	if got := peekSessionID([]byte(`[]`)); got != "" {
		t.Fatal(got)
	}
	if got := peekSessionID([]byte(`{}`)); got != "" {
		t.Fatal(got)
	}
}

func TestSessionTextDefaults(t *testing.T) {
	text := SessionText(hubclient.SessionResponse{
		ExecutionID: "ex_1",
		Peers:       []hubclient.Peer{{Name: "p"}},
		Claims:      []hubclient.Claim{{Path: "a"}},
	})
	for _, want := range []string{"Iris handle: ex_1", "branch -", "until no expiry"} {
		if !strings.Contains(text, want) {
			t.Fatalf("no %q in\n%s", want, text)
		}
	}
}

func TestSessionTextStaysWithinItsLimit(t *testing.T) {
	text := SessionText(hubclient.SessionResponse{Handle: strings.Repeat("h", contextLimit)})
	if len(text) != contextLimit {
		t.Fatal(len(text))
	}
}

func TestWriteLinesRespectsItsBudget(t *testing.T) {
	var b strings.Builder
	budget := 3
	writeLines(&b, &budget, "Peers", nil)
	writeLines(&b, &budget, "Peers", []string{"- a\n"})
	if b.Len() != 0 || budget != 3 {
		t.Fatalf("%q %d", b.String(), budget)
	}
	budget = 100
	writeLines(&b, &budget, "Peers", nil)
	if b.String() != "Peers: none.\n" || budget != 100-b.Len() {
		t.Fatalf("%q %d", b.String(), budget)
	}
}

func TestClassifyTool(t *testing.T) {
	if class, unknown := classifyTool("", false); class != "" || unknown {
		t.Fatal(class, unknown)
	}
}

func TestShellTextAndMalformedArguments(t *testing.T) {
	var ev Event
	applyTool(&ev, "Bash", json.RawMessage(`"ls -l"`), false)
	if ev.Command != "ls -l" {
		t.Fatal(ev.Command)
	}
	ev = Event{}
	applyTool(&ev, "Write", json.RawMessage(`"plain text"`), false)
	if ev.Command != "" {
		t.Fatal("an edit has no command")
	}
	if m, _, isStr := parseRaw(json.RawMessage(`[1]`)); m != nil || isStr {
		t.Fatal(m, isStr)
	}
}

func TestMoveToolsReportBothEnds(t *testing.T) {
	paths, from, to := pathsFromMap("move_file", map[string]any{"source": "a", "destination": "b", "source_path": "", "n": 1})
	if from != "a" || to != "b" || strings.Join(paths, ",") != "a,b" {
		t.Fatal(paths, from, to)
	}
	paths, from, to = pathsFromMap("Edit", map[string]any{"rename_from": "a"})
	if from != "a" || to != "" || strings.Join(paths, ",") != "a" {
		t.Fatal(paths, from, to)
	}
	if got := dedupe([]string{"a", "", "a"}); strings.Join(got, ",") != "a" {
		t.Fatal(got)
	}
}

func TestFailureTextNeverCarriesTheCause(t *testing.T) {
	var syntax error
	var mismatch error
	syntax = json.Unmarshal([]byte("{"), &struct{}{})
	mismatch = json.Unmarshal([]byte(`{"a":1}`), &struct {
		A string `json:"a"`
	}{})
	cases := []struct {
		err  error
		want string
	}{
		{context.Canceled, "hook operation canceled"},
		{mismatch, "invalid JSON field type at byte 6"},
		{syntax, "invalid JSON at byte 1"},
		{&hubclient.Error{Code: "unauthorized", Message: "SECRET"}, "hub request failed: unauthorized"},
		{&hubclient.Error{Code: "strange", Message: "SECRET"}, "hub request failed"},
		{&os.PathError{Op: "open", Path: "/SECRET", Err: syscall.ENOENT}, syscall.ENOENT.Error()},
		{&url.Error{Op: "Get", URL: "http://SECRET", Err: io.EOF}, "incomplete hook input or hub response"},
		{&net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}, syscall.ECONNREFUSED.Error()},
		{io.ErrUnexpectedEOF, "incomplete hook input or hub response"},
		{errors.New("SECRET"), "dependency failure (*errors.errorString)"},
	}
	for _, tc := range cases {
		if got := failureText(tc.err); got != tc.want {
			t.Errorf("%v: %q want %q", tc.err, got, tc.want)
		}
	}
}

func TestLongEventNamesAreCut(t *testing.T) {
	if got := boundedLabel(strings.Repeat("e", 200)); len(got) != 128 {
		t.Fatal(len(got))
	}
}

func TestRecordFailureWithoutATokenPathIsSilent(t *testing.T) {
	// No configuration directory, so there is nowhere to write. It must simply return.
	for _, key := range []string{"HOME", "XDG_CONFIG_HOME", "AppData"} {
		t.Setenv(key, "")
	}
	Runner{ClientName: "claude", Getenv: func(string) string { return "" }}.recordFailure("read", errors.New("x"), nil)
}

// faultyLog is a real file with one call that fails.
type faultyLog struct {
	*os.File
	fail string
	info os.FileInfo
}

var errLog = errors.New("log failure")

func (f faultyLog) Fd() uintptr {
	if f.fail == "lock" {
		return ^uintptr(0)
	}
	return f.File.Fd()
}

func (f faultyLog) Stat() (os.FileInfo, error) {
	if f.fail == "stat" {
		return nil, errLog
	}
	if f.info != nil {
		return f.info, nil
	}
	return f.File.Stat()
}

func (f faultyLog) ReadAt(p []byte, off int64) (int, error) {
	if f.fail == "readat" {
		return 0, errLog
	}
	return f.File.ReadAt(p, off)
}

func (f faultyLog) Read(p []byte) (int, error) {
	if f.fail == "read" {
		return 0, errLog
	}
	return f.File.Read(p)
}

func (f faultyLog) WriteAt(p []byte, off int64) (int, error) {
	if f.fail == "writeat" {
		return 0, errLog
	}
	return f.File.WriteAt(p, off)
}

func (f faultyLog) Truncate(n int64) error {
	if f.fail == "truncate" {
		return errLog
	}
	return f.File.Truncate(n)
}

func (f faultyLog) Seek(off int64, whence int) (int64, error) {
	if f.fail == "seek" {
		return 0, errLog
	}
	return f.File.Seek(off, whence)
}

func useFaultyLog(t *testing.T, fail string, info os.FileInfo) {
	t.Helper()
	real := openFailureLog
	t.Cleanup(func() { openFailureLog = real })
	openFailureLog = func(path string, flag int, perm os.FileMode) (logFile, error) {
		if fail == "open" {
			return nil, errLog
		}
		f, err := os.OpenFile(path, flag, perm)
		return faultyLog{File: f, fail: fail, info: info}, err
	}
}

func fullLog(t *testing.T) string {
	t.Helper()
	token := filepath.Join(t.TempDir(), "hub.token")
	line := `{"stage":"old"}` + "\n"
	body := strings.Repeat(line, failureLogLimit/len(line))
	if err := os.WriteFile(failurePath(token), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return token
}

func TestAppendFailureReportsEveryStep(t *testing.T) {
	entry := Failure{Stage: "new"}
	for _, step := range []string{"open", "lock", "stat", "readat", "truncate", "writeat", "seek"} {
		t.Run(step, func(t *testing.T) {
			token := fullLog(t)
			useFaultyLog(t, step, nil)
			if err := appendFailure(token, entry); err == nil {
				t.Fatal("no error")
			}
		})
	}
}

func TestAppendFailureRefusesWhatIsNotAFile(t *testing.T) {
	token := fullLog(t)
	dir, err := os.Stat(filepath.Dir(token))
	if err != nil {
		t.Fatal(err)
	}
	useFaultyLog(t, "", dir)
	if err := appendFailure(token, Failure{}); err == nil || !strings.Contains(err.Error(), "not regular") {
		t.Fatal(err)
	}
}

func TestAppendFailureRefusals(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := appendFailure(filepath.Join(file, "hub.token"), Failure{}); err == nil {
		t.Fatal("wrote beneath a file")
	}
	token := filepath.Join(t.TempDir(), "hub.token")
	if err := appendFailure(token, Failure{Error: strings.Repeat("x", failureLogLimit)}); err == nil {
		t.Fatal("an oversized entry was written")
	}
	if err := appendFailure(token, Failure{Time: time.Date(20000, 1, 1, 0, 0, 0, 0, time.UTC)}); err == nil {
		t.Fatal("a time that cannot be encoded was written")
	}
}

func TestAppendFailureCannotInspectTheLog(t *testing.T) {
	if runtime.GOOS == "windows" || os.Getuid() == 0 {
		t.Skip("directory modes are not enforced here")
	}
	dir := filepath.Join(t.TempDir(), "locked")
	if err := os.Mkdir(dir, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	token := filepath.Join(dir, "hub.token")
	if err := appendFailure(token, Failure{}); !errors.Is(err, os.ErrPermission) {
		t.Fatal(err)
	}
	if _, err := ReadRecentFailures(token, 1); !errors.Is(err, os.ErrPermission) {
		t.Fatal(err)
	}
}

func TestRotationDropsAPartialLine(t *testing.T) {
	token := filepath.Join(t.TempDir(), "hub.token")
	if err := os.WriteFile(failurePath(token), []byte(strings.Repeat("x", failureLogLimit)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := appendFailure(token, Failure{Stage: "new"}); err != nil {
		t.Fatal(err)
	}
	got, err := ReadRecentFailures(token, 10)
	if err != nil || len(got) != 1 || got[0].Stage != "new" {
		t.Fatal(got, err)
	}
}

func TestReadRecentFailuresReportsEveryStep(t *testing.T) {
	for _, step := range []string{"open", "lock", "stat", "seek", "read"} {
		t.Run(step, func(t *testing.T) {
			token := fullLog(t)
			useFaultyLog(t, step, nil)
			if _, err := ReadRecentFailures(token, 1); err == nil {
				t.Fatal("no error")
			}
		})
	}
}

func TestReadRecentFailuresWithNoLimitReadsNothing(t *testing.T) {
	got, err := ReadRecentFailures(fullLog(t), 0)
	if err != nil || len(got) != 0 {
		t.Fatal(got, err)
	}
}

func TestReadRecentFailuresRefusesWhatIsNotAFile(t *testing.T) {
	token := filepath.Join(t.TempDir(), "hub.token")
	if err := os.Mkdir(failurePath(token), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadRecentFailures(token, 1); err == nil {
		t.Fatal("no error")
	}
}

func TestReadRecentFailuresOfAnOversizedLog(t *testing.T) {
	token := filepath.Join(t.TempDir(), "hub.token")
	body := strings.Repeat("x", failureLogLimit) + "\n" + `{"stage":"kept"}` + "\n"
	if err := os.WriteFile(failurePath(token), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ReadRecentFailures(token, 10)
	if err != nil || len(got) != 1 || got[0].Stage != "kept" {
		t.Fatal(got, err)
	}
	if err := os.WriteFile(failurePath(token), []byte(strings.Repeat("x", failureLogLimit*2)), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err = ReadRecentFailures(token, 10)
	if err != nil || len(got) != 0 {
		t.Fatal(got, err)
	}
}
