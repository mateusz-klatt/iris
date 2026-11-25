package hook

import (
	"context"
	"fmt"
	"github.com/mateusz-klatt/iris/internal/hub"
	"github.com/mateusz-klatt/iris/internal/store"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/gitinfo"
	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/proc"
)

type reconcileHub struct{ wait bool }

func (reconcileHub) RegisterSession(context.Context, hubclient.SessionRequest) (hubclient.SessionResponse, error) {
	return hubclient.SessionResponse{ExecutionID: "shell", Generation: 1}, nil
}
func (reconcileHub) Touch(context.Context, hubclient.TouchRequest) (hubclient.TouchResponse, error) {
	return hubclient.TouchResponse{Action: "allow"}, nil
}
func (reconcileHub) EndExecution(context.Context, string, int) error { return nil }
func (h reconcileHub) Reconcile(ctx context.Context, r hubclient.ReconcileRequest) (hubclient.CheckResponse, error) {
	if h.wait {
		<-ctx.Done()
		return hubclient.CheckResponse{}, ctx.Err()
	}
	return hubclient.CheckResponse{Message: "Shell edit detected. Iris detects these changes and cannot undo them."}, nil
}

func TestShellContextEveryClient(t *testing.T) {
	for _, client := range Clients {
		t.Run(string(client), func(t *testing.T) {
			r := Runner{Find: func(string) (gitinfo.Repo, error) { return gitinfo.Repo{Worktree: "/repo"}, nil }, Hub: reconcileHub{}, Status: func(context.Context, string, gitinfo.Scope, *gitinfo.Attempt) (map[string]string, error) {
				return map[string]string{"path": "??"}, nil
			}, ShellDeadline: time.Hour}
			ev := Event{Client: client, Kind: KindPostTool, ToolClass: ClassShell, SessionID: "shell", CWD: "/repo"}
			out := r.reconcile(context.Background(), ev, &runState{})
			if out.Exit != 0 || !strings.Contains(string(out.Stdout), "cannot undo") {
				t.Fatalf("missing context: %#v", out)
			}
		})
	}
}
func TestShellDeadlineAndNonShellCost(t *testing.T) {
	r := Runner{ClientName: "claude", EventName: "PostToolUse", Hub: reconcileHub{wait: true}, Find: func(string) (gitinfo.Repo, error) { return gitinfo.Repo{Worktree: "/repo"}, nil }, Status: func(context.Context, string, gitinfo.Scope, *gitinfo.Attempt) (map[string]string, error) {
		return map[string]string{}, nil
	}}
	r.Stdin = strings.NewReader(`{"session_id":"shell","cwd":"/repo","tool_name":"Bash","tool_input":{"command":"echo ok"}}`)
	// The hub never answers and the hook's own deadline never fires, so only the shell
	// budget can end this call. If it did not, the test would hang until the binary's timeout.
	r.after = never
	out := r.Run()
	assertAllow(t, ClientClaude, out)
	r.after = nil
	r.Status = func(context.Context, string, gitinfo.Scope, *gitinfo.Attempt) (map[string]string, error) {
		t.Error("non-shell scanned Git status")
		return nil, nil
	}
	r.Stdin = strings.NewReader(`{"session_id":"shell","cwd":"/repo","tool_name":"Edit","tool_input":{"file_path":"/repo/a"}}`)
	assertAllow(t, ClientClaude, r.Run())
}

func TestShellStatusAgainstRealHub(t *testing.T) {
	root := t.TempDir()
	cmd := exec.Command("git", "init", root)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git: %v: %s", err, out)
	}
	database, err := store.Open(filepath.Join(t.TempDir(), "hub.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	server := httptest.NewUnstartedServer(nil)
	server.Config.Handler = hub.New(database, "secret", []string{server.Listener.Addr().String()})
	server.Start()
	defer server.Close()
	client := &hubclient.Client{Config: hubURL(server.URL)}
	// Functional integration does not impose a machine-speed assertion. The separate
	// deadline test proves the Runner's 300 ms fail-open boundary under stalled I/O.
	// Setup against the real hub is not timed. The test binary's own timeout bounds a hang.
	ctx := context.Background()
	holder, err := client.RegisterSession(ctx, hubclient.SessionRequest{Client: "codex", SessionID: "holder", Host: "testhost", CWD: "/other", Worktree: "/other", RemoteURL: "https://example.com/team/shell.git"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = client.Touch(ctx, hubclient.TouchRequest{ExecutionID: holder.ExecutionID, Generation: holder.Generation, ToolCallID: "held", Paths: []string{"shared"}}); err != nil {
		t.Fatal(err)
	}
	shell, err := client.RegisterSession(ctx, hubclient.SessionRequest{Client: "claude", SessionID: "shell", Host: "testhost", CWD: root, Worktree: root, RemoteURL: "https://example.com/team/shell.git"})
	if err != nil {
		t.Fatal(err)
	}
	request := hubclient.ReconcileRequest{ExecutionID: shell.ExecutionID, Generation: shell.Generation, Kind: "before"}
	request.Status, err = gitinfo.Status(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = client.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(root, "shared"), []byte("shell edit"), 0600); err != nil {
		t.Fatal(err)
	}
	request.Kind = "after"
	request.Status, err = gitinfo.Status(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	out, err := client.Reconcile(ctx, request)
	if err != nil || !strings.Contains(out.Message, "Shell edit detected") {
		t.Fatalf("missing shell conflict: %#v %v", out, err)
	}
}

// Codex's Code Mode js tool can write any file, and Iris cannot tell what from the code, so a
// real capture of it must take the shell path: reconciled before and after, not passed as an
// MCP call that touches nothing.
func TestCodexJavaScriptToolIsReconciledLikeAShell(t *testing.T) {
	for _, name := range []string{"PreToolUse-node_repl_js.json", "PostToolUse-node_repl_js.json"} {
		raw, err := os.ReadFile(filepath.Join("testdata", "codex", name))
		if err != nil {
			t.Fatal(err)
		}
		ev, err := codexAdapter{}.Decode(strings.TrimSuffix(strings.SplitN(name, "-", 2)[0], ".json"), raw)
		if err != nil {
			t.Fatal(err)
		}
		if ev.ToolClass != ClassShell || !shellEvent(ev) || !strings.Contains(ev.Command, "fs.appendFile") {
			t.Fatalf("%s: class %q shell %v command %q", name, ev.ToolClass, shellEvent(ev), ev.Command)
		}
	}
}

type fullShellOnlyHub struct {
	*recoveryHub
	reconciles int
}

func (h *fullShellOnlyHub) Reconcile(context.Context, hubclient.ReconcileRequest) (hubclient.CheckResponse, error) {
	h.reconciles++
	return hubclient.CheckResponse{}, nil
}

func TestFullSplitShellRechecksEndMarkerAfterSnapshot(t *testing.T) {
	m := newFileMemory(t)
	hub := &fullShellOnlyHub{recoveryHub: &recoveryHub{generation: 2}}
	ev := Event{Client: ClientClaude, SessionID: "s", Kind: KindPreTool, CWD: "/repo", ToolClass: ClassShell}
	r := Runner{Hub: hub, Memory: m}
	r.Status = func(context.Context, string, gitinfo.Scope, *gitinfo.Attempt) (map[string]string, error) {
		m.Forget("claude", "s", 2)
		return map[string]string{}, nil
	}
	state := &runState{}
	_, fromGit, err := r.shellFull(context.Background(), hub, ev, gitinfo.Repo{Worktree: "/repo"}, state, &shellRun{}, "before")
	marker := m.name("claude", "s") + ".ended"
	observations := state.observations()
	if err != errEndedGeneration || fromGit || hub.reconciles != 0 || len(observations) != 1 || observations[0] != endedProof(marker).Error() { // gitleaks:allow (a test observation token and path, not a credential)
		t.Fatalf("full split shell used ended G2: err %v, fromGit %t, reconciles %d, observations %v", err, fromGit, hub.reconciles, observations)
	}
}

func TestParseReconcileSkipped(t *testing.T) {
	for _, tc := range []struct {
		text     string
		wantKind string
		wantN    int
		wantOK   bool
	}{
		{"shell reconcile skipped (claim_reserve): 120 path(s)", ReconcileSkippedClaimReserve, 120, true},
		{"shell reconcile skipped (unknown): 3 path(s)", ReconcileSkippedUnknown, 3, true},
		{"shell reconcile skipped (secret): 3 path(s)", "", 0, false},
		{"shell reconcile skipped (claim_reserve): 3", "", 0, false},
		{"shell reconcile skipped (claim_reserve): abc path(s)", "", 0, false},
		{"shell reconcile skipped (claim_reserve): 0 path(s)", "", 0, false},
		{"unknown tool view", "", 0, false},
	} {
		kind, n, ok := ParseReconcileSkipped(tc.text)
		if kind != tc.wantKind || n != tc.wantN || ok != tc.wantOK {
			t.Errorf("ParseReconcileSkipped(%q) = (%q, %d, %t), want (%q, %d, %t)", tc.text, kind, n, ok, tc.wantKind, tc.wantN, tc.wantOK)
		}
	}
}

type skippedReconcileHub struct{ reconcileHub }

func (skippedReconcileHub) Reconcile(context.Context, hubclient.ReconcileRequest) (hubclient.CheckResponse, error) {
	return hubclient.CheckResponse{
		Message:    "Shell edit detected. Iris detects these changes and cannot undo them.",
		Skipped:    5,
		SkipReason: "SECRET_HUB_TEXT",
	}, nil
}

func TestReconcileSkippedObservationSanitizesReason(t *testing.T) {
	root := gitRepo(t)
	getenv := failureEnv(t)
	token := getenv("IRIS_TOKEN_FILE")
	payload := fmt.Sprintf(`{"session_id":"s-unrecognized","cwd":%q,"tool_name":"Bash","tool_input":{"command":"ls"}}`, root)
	out := Runner{
		ClientName: "claude",
		EventName:  "PostToolUse",
		Stdin:      strings.NewReader(payload),
		Getenv:     getenv,
		Hub:        skippedReconcileHub{},
		Ancestor:   func() (proc.Ancestor, error) { return proc.Ancestor{}, nil },
		Status: func(context.Context, string, gitinfo.Scope, *gitinfo.Attempt) (map[string]string, error) {
			return map[string]string{"a.go": "??"}, nil
		},
		ShellDeadline: time.Minute,
		after:         never,
		logAfter:      never,
	}.Run()
	if out.Exit != 0 || !strings.Contains(string(out.Stdout), "Shell edit detected.") {
		t.Fatalf("unexpected hook output: %#v", out)
	}
	got := observedOnly(t, token)
	if got.Error != "shell reconcile skipped (unknown): 5 path(s)" {
		t.Fatalf("got observation %+v", got)
	}
	raw, err := os.ReadFile(observedPath(token))
	if err != nil || strings.Contains(string(raw), "SECRET_HUB_TEXT") {
		t.Fatalf("raw observation log leaked or unreadable: err=%v raw=%s", err, raw)
	}
}

func TestShellReserveAllowsFollowUpEditsAndLogsObservation(t *testing.T) {
	root := gitRepo(t)
	database, err := store.Open(filepath.Join(t.TempDir(), "hub.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	server := httptest.NewUnstartedServer(nil)
	server.Config.Handler = hub.New(database, "secret", []string{server.Listener.Addr().String()})
	server.Start()
	defer server.Close()

	token := tokenFile(t)
	getenv := func(key string) string {
		switch key {
		case "IRIS_TOKEN_FILE":
			return token
		case "IRIS_HUB_URL":
			return server.URL
		default:
			return ""
		}
	}
	client := &hubclient.Client{Config: hubclient.EnvConfig{Getenv: getenv}}
	runHook := func(event, body string, status func(context.Context, string, gitinfo.Scope, *gitinfo.Attempt) (map[string]string, error)) Output {
		return Runner{
			ClientName:    "claude",
			EventName:     event,
			Stdin:         strings.NewReader(body),
			Getenv:        getenv,
			Hub:           client,
			Host:          func() string { return "testhost" },
			Ancestor:      func() (proc.Ancestor, error) { return proc.Ancestor{}, nil },
			Status:        status,
			ShellDeadline: time.Minute,
			after:         never,
			logAfter:      never,
		}.Run()
	}

	shellPayload := fmt.Sprintf(`{"session_id":"s-reserve","cwd":%q,"tool_name":"Bash","tool_input":{"command":"pip install pkg"}}`, root)
	beforeOut := runHook("PreToolUse", shellPayload, func(context.Context, string, gitinfo.Scope, *gitinfo.Attempt) (map[string]string, error) {
		return map[string]string{}, nil
	})
	assertAllow(t, ClientClaude, beforeOut)

	many := make(map[string]string, 420)
	for i := range 420 {
		many[fmt.Sprintf("pkg/f%03d.py", i)] = "??"
	}
	afterOut := runHook("PostToolUse", shellPayload, func(context.Context, string, gitinfo.Scope, *gitinfo.Attempt) (map[string]string, error) {
		return many, nil
	})
	if afterOut.Exit != 0 || !strings.Contains(string(afterOut.Stdout), "left 20 unclaimed") {
		t.Fatalf("post-shell output = %#v", afterOut)
	}
	obs := observedOnly(t, token)
	if obs.Error != "shell reconcile skipped (claim_reserve): 20 path(s)" {
		t.Fatalf("unexpected reserve observation: %+v", obs)
	}

	// Subsequent single-file pre-edit hook succeeds because 100 slots stayed reserved.
	editOne := fmt.Sprintf(`{"session_id":"s-reserve","cwd":%q,"tool_name":"Write","tool_input":{"file_path":%q}}`, root, filepath.Join(root, "edit-after.go"))
	assertAllow(t, ClientClaude, runHook("PreToolUse", editOne, nil))
	if failures, err := ReadRecentFailures(token, 10); err != nil || len(failures) != 0 {
		t.Fatalf("edit after reconcile reserve failed open: %+v %v", failures, err)
	}

	ctx := context.Background()
	var bound hubclient.SessionResponse
	if err := client.Do(ctx, "POST", "/v1/bind", map[string]any{"client": "claude", "host": "testhost", "session_id": "s-reserve"}, &bound); err != nil {
		t.Fatal(err)
	}
	for i := range 99 {
		if err := client.Do(ctx, "POST", "/v1/claims", map[string]any{
			"execution_id": bound.ExecutionID,
			"generation":   bound.Generation,
			"key":          fmt.Sprintf("exp-%02d", i),
			"kind":         "exact",
			"path":         fmt.Sprintf("explicit-%02d.go", i),
		}, nil); err != nil {
			t.Fatalf("explicit claim %d failed: %v", i, err)
		}
	}
	var listed struct {
		Claims []struct{ Path string }
	}
	if err := client.Do(ctx, "GET", "/v1/claims?repository_id="+url.QueryEscape(bound.RepositoryID), nil, &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Claims) != 500 {
		t.Fatalf("claims = %d, want 500", len(listed.Claims))
	}

	// Hard cap of 500 still refuses with too_large (claim_limit) and D3 fails open.
	editOver := fmt.Sprintf(`{"session_id":"s-reserve","cwd":%q,"tool_name":"Write","tool_input":{"file_path":%q}}`, root, filepath.Join(root, "edit-over.go"))
	assertAllow(t, ClientClaude, runHook("PreToolUse", editOver, nil))
	failures, err := ReadRecentFailures(token, 10)
	if err != nil || len(failures) != 1 || failures[0].Error != "hub request failed: too_large (claim_limit)" {
		t.Fatalf("failures at 500 cap = %+v, err = %v", failures, err)
	}
}
