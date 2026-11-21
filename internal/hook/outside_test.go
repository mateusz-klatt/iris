package hook

import (
	"context"
	"strings"
	"testing"

	"github.com/mateusz-klatt/iris/internal/gitinfo"
	"github.com/mateusz-klatt/iris/internal/hubclient"
)

// contextHub is a hub that knows sessions outside git, or with older set one from before ADR 034.
type contextHub struct {
	routes     *[]string
	older      bool
	outsideGit bool
	ended      *int
	touched    *int
}

func (h contextHub) RegisterSession(_ context.Context, r hubclient.SessionRequest) (hubclient.SessionResponse, error) {
	*h.routes = append(*h.routes, "sessions")
	return hubclient.SessionResponse{ExecutionID: "ex", Generation: 1, OutsideGit: h.outsideGit}, nil
}

func (h contextHub) RegisterOutsideSession(_ context.Context, r hubclient.SessionRequest) (hubclient.SessionResponse, error) {
	*h.routes = append(*h.routes, "outside")
	if h.older {
		return hubclient.SessionResponse{}, &hubclient.Error{Code: "not_found", Message: "unknown API route"}
	}
	if r.Worktree != "" || r.CommonDir != "" || r.RemoteURL != "" || r.RootCommit != "" {
		return hubclient.SessionResponse{}, &hubclient.Error{Code: "bad_request", Message: "names a checkout"}
	}
	return hubclient.SessionResponse{ExecutionID: "ex", Generation: 1, OutsideGit: true, Agent: hubclient.Agent{Name: "claude-host-tmp"}}, nil
}

func (h contextHub) Touch(context.Context, hubclient.TouchRequest) (hubclient.TouchResponse, error) {
	*h.touched++
	return hubclient.TouchResponse{Action: "allow"}, nil
}

func (h contextHub) EndExecution(context.Context, string, int) error {
	*h.ended++
	return nil
}

func outsideRun(h Hub, getenv func(string) string, client, event, payload string, find func(string) (gitinfo.Repo, error)) Output {
	return Runner{ClientName: client, EventName: event, Stdin: strings.NewReader(payload), Hub: h, Getenv: getenv, Find: find, after: never, logAfter: never}.Run()
}

func notRepo(string) (gitinfo.Repo, error) { return gitinfo.Repo{}, gitinfo.ErrNotRepo }

// Outside git the hook registers at session start, at a prompt and at the end, on the outside
// route, and an edit there makes no request (ADR 034).
func TestAHookOutsideGitRegistersOnTheOutsideRoute(t *testing.T) {
	getenv := failureEnv(t)
	var routes []string
	var ended, touched int
	h := contextHub{routes: &routes, ended: &ended, touched: &touched}
	start := outsideRun(h, getenv, "claude", "SessionStart", `{"session_id":"s","cwd":"/home/u/tmp","hook_event_name":"SessionStart"}`, notRepo)
	if !strings.Contains(string(start.Stdout), "claude-host-tmp") || strings.Contains(string(start.Stdout), "pre-commit") {
		t.Fatalf("session start: %s", start.Stdout)
	}
	outsideRun(h, getenv, "claude", "UserPromptSubmit", `{"session_id":"s","cwd":"/home/u/tmp","hook_event_name":"UserPromptSubmit","prompt":"hello"}`, notRepo)
	edit := outsideRun(h, getenv, "claude", "PreToolUse", `{"session_id":"s","cwd":"/home/u/tmp","hook_event_name":"PreToolUse","tool_name":"Write","tool_use_id":"t","tool_input":{"file_path":"/home/u/tmp/x"}}`, notRepo)
	assertAllow(t, ClientClaude, edit)
	outsideRun(h, getenv, "claude", "SessionEnd", `{"session_id":"s","cwd":"/home/u/tmp","hook_event_name":"SessionEnd"}`, notRepo)
	if strings.Join(routes, ",") != "outside,outside,outside" || touched != 0 || ended != 1 {
		t.Fatalf("routes %v, touched %d, ended %d", routes, touched, ended)
	}
	if entries, err := ReadRecentFailures(getenv("IRIS_TOKEN_FILE"), 10); err != nil || len(entries) != 0 {
		t.Fatal(entries, err)
	}
}

// A hub without the outside route is no failure: the event is allowed, nothing is registered and
// nothing is logged, as a hook outside git did before (ADR 034). So is a hub that cannot say.
func TestAHookOutsideGitAllowsOnAnOlderHub(t *testing.T) {
	getenv := failureEnv(t)
	var routes []string
	var ended, touched int
	older := contextHub{routes: &routes, older: true, ended: &ended, touched: &touched}
	for _, event := range []string{"SessionStart", "UserPromptSubmit", "SessionEnd"} {
		out := outsideRun(older, getenv, "claude", event, `{"session_id":"s","cwd":"/home/u/tmp","hook_event_name":"`+event+`","prompt":"hello"}`, notRepo)
		assertAllow(t, ClientClaude, out)
	}
	out := outsideRun(scriptHub{}, getenv, "claude", "SessionStart", `{"session_id":"s","cwd":"/home/u/tmp","hook_event_name":"SessionStart"}`, notRepo)
	assertAllow(t, ClientClaude, out)
	// An interactive Cursor's stop registers too, and on an older hub stays quiet.
	interactiveCursor(t)
	stop := Runner{ClientName: "cursor", EventName: "stop", Stdin: strings.NewReader(cursorStopPayload("/home/u/tmp", 0)), Hub: older, Getenv: getenv, Find: notRepo, Ancestor: cursorAncestor, after: never, logAfter: never}.Run()
	if strings.Contains(string(stop.Stdout), "followup_message") || routes[len(routes)-1] != "outside" {
		t.Fatalf("cursor stop: %s, routes %v", stop.Stdout, routes)
	}
	if ended != 0 {
		t.Fatalf("ended %d", ended)
	}
	if entries, err := ReadRecentFailures(getenv("IRIS_TOKEN_FILE"), 10); err != nil || len(entries) != 0 {
		t.Fatal(entries, err)
	}
}

// iris doctor's canary runs in an empty directory so that it reaches no hub, and still does not.
func TestDoctorsCanaryOutsideGitReachesNoHub(t *testing.T) {
	var routes []string
	var ended, touched int
	out := outsideRun(contextHub{routes: &routes, ended: &ended, touched: &touched}, failureEnv(t), "claude", "SessionStart", `{"session_id":"`+CanarySession+`","cwd":"/tmp/iris-canary-1","hook_event_name":"SessionStart"}`, notRepo)
	assertAllow(t, ClientClaude, out)
	if len(routes) != 0 {
		t.Fatal(routes)
	}
}

// A session that began outside git stays attached there after a git init: its edit in the new
// checkout is allowed without a touch (ADR 034).
func TestAnEditOfASessionAttachedOutsideGitTakesNoClaim(t *testing.T) {
	getenv := failureEnv(t)
	root := gitRepo(t)
	var routes []string
	var ended, touched int
	h := contextHub{routes: &routes, outsideGit: true, ended: &ended, touched: &touched}
	out := outsideRun(h, getenv, "claude", "PreToolUse", writePayload(root, "s", "a.go"), gitinfo.Find)
	assertAllow(t, ClientClaude, out)
	if strings.Join(routes, ",") != "sessions" || touched != 0 {
		t.Fatalf("routes %v, touched %d", routes, touched)
	}
	if entries, err := ReadRecentFailures(getenv("IRIS_TOKEN_FILE"), 10); err != nil || len(entries) != 0 {
		t.Fatal(entries, err)
	}
}
