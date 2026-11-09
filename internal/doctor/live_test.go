package doctor

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/gitinfo"
	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/integrate"
)

// TestMain lets the test binary stand in for a client. RunLive starts it as
// it would start claude, so the process, its directory and its environment are
// the real ones; only the client's behaviour is scripted.
func TestMain(m *testing.M) {
	if mode := os.Getenv("IRIS_FAKE_LIVE_CLIENT"); mode != "" {
		os.Exit(fakeLiveClient(mode, os.Args[1:]))
	}
	os.Exit(m.Run())
}

var nonceInPrompt = regexp.MustCompile(`containing exactly ([0-9a-f]+) to`)

// fakeLiveClient does what a hooked client does on the live prompt: its hook
// registers the session and claims the file, its MCP server returns the
// execution id, and the model writes the line and repeats the id.
func fakeLiveClient(mode string, args []string) int {
	cwd, _ := os.Getwd()
	if mode == "fail" {
		fmt.Print("the model is not available")
		return 3
	}
	if mode == "sleep" {
		time.Sleep(time.Minute)
		return 0
	}
	nonce := nonceInPrompt.FindStringSubmatch(strings.Join(args, " "))
	if nonce == nil {
		fmt.Print("no nonce in ", args)
		return 4
	}
	if mode != "nohook" {
		ctx := context.Background()
		hub := &hubclient.Client{Config: hubclient.NewConfig()}
		repo, err := gitinfo.Find(cwd)
		if err != nil {
			fmt.Print(err)
			return 5
		}
		if mode == "foreign" {
			repo.RemoteURL = "https://example.invalid/somebody-else.git"
		}
		// Grok also loads Claude's hooks, so the session is not Grok's own.
		client := "claude"
		if name := os.Getenv("IRIS_FAKE_LIVE_NAME"); name != "" {
			client = name
		}
		if mode == "otherclient" {
			client = "grok"
		}
		register := func(session string) hubclient.SessionResponse {
			resp, err := hub.RegisterSession(ctx, hubclient.SessionRequest{CommonDir: repo.CommonDir, Client: client, SessionID: session, Host: "fake", CWD: cwd, Worktree: repo.Worktree, RemoteURL: repo.RemoteURL, Source: "hook"})
			if err != nil {
				fmt.Print(err)
				os.Exit(6)
			}
			return resp
		}
		if mode == "held" {
			// Another session holds the file, so the pre-edit claim is refused.
			other := register("fake-other")
			if err := hub.Do(ctx, "POST", "/v1/claims", map[string]any{"execution_id": other.ExecutionID, "generation": other.Generation, "kind": "exact", "path": scratchFile, "key": "fake-held"}, nil); err != nil {
				fmt.Print(err)
				return 7
			}
		}
		// A refused request proves nothing and must not count.
		_, _ = hub.Touch(ctx, hubclient.TouchRequest{ExecutionID: "ex_unknown", Paths: []string{scratchFile}})
		own := register("fake-session")
		if _, err := hub.Touch(ctx, hubclient.TouchRequest{ExecutionID: own.ExecutionID, Generation: own.Generation, ToolCallID: "t1", Paths: []string{"other.txt", scratchFile}}); err != nil {
			fmt.Print(err)
			return 8
		}
		switch mode {
		case "nomcp":
		case "echo":
			// The handle from the hook's session context, echoed with no MCP call behind it.
			fmt.Println("execution_id:", own.ExecutionID)
		default:
			// What the Iris whoami tool does: bind with the handle and report the binding.
			var bound struct {
				RepositoryID string `json:"repository_id"`
			}
			if err := hub.Do(ctx, "POST", "/v1/bind", map[string]any{"client": client, "host": "fake", "cwd": cwd, "execution": own.ExecutionID}, &bound); err != nil {
				fmt.Print(err)
				return 11
			}
			fmt.Println("repository_id:", bound.RepositoryID)
		}
	}
	if config := os.Getenv("IRIS_FAKE_CODEX_CONFIG"); config != "" {
		// Codex trusts every directory it runs in, in the user's config.toml.
		raw, _ := os.ReadFile(config)
		raw = append(raw, "\n[projects."+integrate.TOMLString(cwd)+"]\ntrust_level = \"trusted\"\n"...)
		raw = append(raw, os.Getenv("IRIS_FAKE_CODEX_EXTRA")...)
		if err := os.WriteFile(config, raw, 0o600); err != nil {
			fmt.Print(err)
			return 10
		}
	}
	old, _ := os.ReadFile(filepath.Join(cwd, scratchFile))
	if err := os.WriteFile(filepath.Join(cwd, scratchFile), append(old, nonce[1]+"\n"...), 0o600); err != nil {
		fmt.Print(err)
		return 9
	}
	return 0
}

func fakePlan(t *testing.T, mode string) LivePlan {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("IRIS_FAKE_LIVE_CLIENT", mode)
	return LivePlan{Client: integrate.Claude, Binary: exe, Model: "haiku"}
}

func TestLiveRunProvesEveryPart(t *testing.T) {
	// The configured hub must never be used: the client inherits these and they are replaced.
	t.Setenv("IRIS_HUB_URL", "http://127.0.0.1:1")
	t.Setenv("IRIS_EXECUTION", "ex_from_the_parent")
	res := RunLive(context.Background(), LiveOptions{Timeout: time.Minute}, fakePlan(t, "full"))
	if !res.OK() {
		t.Fatalf("%+v", res)
	}
	if !strings.HasPrefix(res.Marker, "https://example.invalid/iris-doctor-live/") {
		t.Fatal(res.Marker)
	}
	text := FormatLive([]LiveResult{res})
	for _, want := range []string{"claude (", "model haiku): ok", "hook fired: yes", "claim reached the hub: yes", "MCP tools answered: yes", "scratch file edited: yes"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in\n%s", want, text)
		}
	}
}

func TestLiveRunSaysWhichPartFailed(t *testing.T) {
	for _, tc := range []struct {
		mode                     string
		hook, claim, tools, edit bool
		detail                   string
	}{
		{mode: "nohook", edit: true, detail: "the hook used the configured hub"},
		{mode: "foreign", edit: true, detail: "the hook used the configured hub"},
		{mode: "otherclient", edit: true, detail: "the hook used the configured hub"},
		{mode: "nomcp", hook: true, claim: true, edit: true},
		// The execution id is in the hook's session context, so echoing it proves no MCP call (gap B7).
		{mode: "echo", hook: true, claim: true, edit: true},
		{mode: "held", hook: true, tools: true, edit: true},
		{mode: "fail", detail: "client failed: "},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			res := RunLive(context.Background(), LiveOptions{Timeout: time.Minute}, fakePlan(t, tc.mode))
			if res.OK() || res.HookFired != tc.hook || res.ClaimReached != tc.claim || res.ToolsAnswered != tc.tools || res.FileEdited != tc.edit {
				t.Fatalf("%+v", res)
			}
			if !strings.Contains(res.Detail, tc.detail) {
				t.Fatalf("detail %q", res.Detail)
			}
			if tc.detail == "the hook used the configured hub" && (!strings.Contains(res.Detail, res.Marker) || !strings.Contains(res.Detail, "iris admin forget")) {
				t.Fatalf("no way to find stray records: %q", res.Detail)
			}
			// Whatever fact failed, a client that exited 0 shows what it did.
			if tc.mode != "fail" && !strings.Contains(res.Detail, "client output: ") {
				t.Fatalf("a client that exits 0 must show its output when a fact fails: %q", res.Detail)
			}
			if (tc.mode == "nomcp" || tc.mode == "echo") && !strings.HasPrefix(res.Detail, "client output: ") {
				t.Fatalf("MCP failed alone, so the output is the whole explanation: %q", res.Detail)
			}
			if tc.mode == "fail" && !strings.Contains(res.Detail, "the model is not available") {
				t.Fatalf("client output lost: %q", res.Detail)
			}
			if !strings.Contains(FormatLive([]LiveResult{res}), ": failed\n") {
				t.Fatal(FormatLive([]LiveResult{res}))
			}
		})
	}
}

func TestLiveRunKeepsToItsTimeout(t *testing.T) {
	start := time.Now()
	res := RunLive(context.Background(), LiveOptions{Timeout: 200 * time.Millisecond}, fakePlan(t, "sleep"))
	if !strings.Contains(res.Detail, "timed out after 200ms") || time.Since(start) > 30*time.Second {
		t.Fatalf("%+v", res)
	}
}

func TestLiveRunLeavesNothingBehind(t *testing.T) {
	tmp := t.TempDir()
	for _, name := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(name, tmp)
	}
	res := RunLive(context.Background(), LiveOptions{}, fakePlan(t, "full"))
	if !res.OK() {
		t.Fatalf("%+v", res)
	}
	left, err := os.ReadDir(tmp)
	if err != nil || len(left) != 0 {
		t.Fatalf("left %v err %v", left, err)
	}
}

func TestLiveRunReportsItsOwnSetupFailures(t *testing.T) {
	plan := LivePlan{Client: integrate.Claude, Binary: "claude"}
	never := func(context.Context, string, []string, string, []string) ([]byte, error) {
		t.Fatal("the client ran")
		return nil, nil
	}
	if res := RunLive(context.Background(), LiveOptions{Run: never}, LivePlan{Client: "unheard-of"}); !strings.Contains(res.Detail, "no live invocation") {
		t.Fatalf("%+v", res)
	}
	missing := filepath.Join(t.TempDir(), "missing")
	for _, name := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(name, missing)
	}
	if res := RunLive(context.Background(), LiveOptions{Run: never}, plan); res.Detail == "" {
		t.Fatalf("%+v", res)
	}
	for _, name := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(name, t.TempDir())
	}
	orig := listen
	defer func() { listen = orig }()
	listen = func(string, string) (net.Listener, error) { return nil, errors.New("refused") }
	if res := RunLive(context.Background(), LiveOptions{Run: never}, plan); res.Detail != "scratch setup: refused" {
		t.Fatalf("%+v", res)
	}
}

func TestScratchRepoAndHubReportWhatTheyCannotWrite(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := setupScratch(file, "x"); err == nil {
		t.Fatal("repository under a file")
	}
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git", "HEAD"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := scratchRepo(root, "x"); err == nil {
		t.Fatal("HEAD is a directory")
	}
	if _, err := startScratchHub(filepath.Join(file, "hub")); err == nil {
		t.Fatal("token under a file")
	}
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "hub.db"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := startScratchHub(dir); err == nil {
		t.Fatal("database is a directory")
	}
}

func TestScratchRepoIsARepositoryOfItsOwn(t *testing.T) {
	root := t.TempDir()
	marker := "https://example.invalid/iris-doctor-live/abc.git"
	if err := scratchRepo(root, marker); err != nil {
		t.Fatal(err)
	}
	repo, err := gitinfo.Find(root)
	if err != nil {
		t.Fatal(err)
	}
	if repo.RemoteURL != marker || repo.RootCommit != "" || repo.Branch != "main" {
		t.Fatalf("%+v", repo)
	}
}

func TestLiveEnvReplacesTheHubAndDropsTheParentSession(t *testing.T) {
	env := liveEnv([]string{"PATH=/bin", "IRIS_HUB_URL=http://configured", "iris_token_file=/real", "IRIS_EXECUTION=ex_parent", "CLAUDE_CODE_SESSION_ID=parent", "COPILOT_AGENT_SESSION_ID=p", "GROK_SESSION_ID=p"}, "http://127.0.0.1:9", "/tmp/t")
	want := []string{"PATH=/bin", "IRIS_HUB_URL=http://127.0.0.1:9", "IRIS_TOKEN_FILE=/tmp/t"}
	if strings.Join(env, "\n") != strings.Join(want, "\n") {
		t.Fatal(env)
	}
}

func TestLiveCodexPutsItsConfigBack(t *testing.T) {
	layout, config := codexLayout(t)
	writeConfig(t, config, userCodexConfig)
	t.Setenv("IRIS_FAKE_CODEX_CONFIG", config)
	t.Setenv("IRIS_FAKE_LIVE_NAME", integrate.Codex)
	plan := fakePlan(t, "nohook")
	plan.Client = integrate.Codex
	res := RunLive(context.Background(), LiveOptions{Layout: layout, Timeout: time.Minute}, plan)
	if res.HookFired || !strings.Contains(res.Detail, "the scratch hub saw no session from this client's hook") {
		t.Fatalf("%+v", res)
	}
	if got, _ := os.ReadFile(config); string(got) != userCodexConfig {
		t.Fatalf("config.toml not restored:\n%s", got)
	}
	// A file the user changed meanwhile is left alone, and the report says so.
	writeConfig(t, config, userCodexConfig)
	t.Setenv("IRIS_FAKE_CODEX_EXTRA", "# the user edited this meanwhile\n")
	plan = fakePlan(t, "full")
	plan.Client = integrate.Codex
	res = RunLive(context.Background(), LiveOptions{Layout: layout, Timeout: time.Minute}, plan)
	if res.OK() || !res.HookFired || !strings.HasPrefix(res.Detail, config+" changed during the run in other ways") {
		t.Fatalf("%+v", res)
	}
	if got, _ := os.ReadFile(config); !strings.HasSuffix(string(got), "# the user edited this meanwhile\n") {
		t.Fatalf("an edited config.toml was rewritten:\n%s", got)
	}
}

func TestLivePlansNeedAWorkingHookAndABinary(t *testing.T) {
	layout := integrate.Layout{LookPath: func(name string) (string, error) {
		if name == "kimi" {
			return "", errors.New("not found")
		}
		return "/bin/" + name, nil
	}}
	rows := []ClientReport{
		{Client: integrate.Claude, Status: StatusOK},
		{Client: integrate.Codex, Status: StatusNeedsHuman, Detail: "Review hooks once in /hooks"},
		{Client: integrate.Copilot, Status: StatusSilent},
		{Client: integrate.Cursor, Status: StatusNotConfigured, Detail: "configuration is absent"},
		// An unknown status cannot authorize a billable run.
		{Client: integrate.Antigravity, Status: "unknown"},
		{Client: integrate.Grok, Status: StatusOK},
		{Client: integrate.Kimi, Status: StatusOK},
	}
	plans, skipped := LivePlans(layout, rows, "")
	// A silent row may be an old failure, and live is how to prove the client works now.
	if len(plans) != 3 || plans[0] != (LivePlan{Client: integrate.Claude, Binary: "/bin/claude", Model: "haiku"}) || plans[1].Client != integrate.Copilot || plans[2] != (LivePlan{Client: integrate.Grok, Binary: "/bin/grok", Model: "grok-4.7-build-fast"}) {
		t.Fatalf("%+v", plans)
	}
	if strings.Join(skipped, "\n") != "codex: Review hooks once in /hooks\ncursor: configuration is absent\nkimi: client binary not found" {
		t.Fatalf("%q", skipped)
	}
	if only, _ := LivePlans(layout, rows, integrate.Grok); len(only) != 1 || only[0].Client != integrate.Grok {
		t.Fatalf("%+v", only)
	}
	// Naming a client never bypasses the step doctor knows is still needed.
	if named, skipped := LivePlans(layout, rows, integrate.Codex); len(named) != 0 || strings.Join(skipped, "\n") != "codex: Review hooks once in /hooks" {
		t.Fatalf("%+v %v", named, skipped)
	}
	if named, skipped := LivePlans(layout, rows, integrate.Cursor); len(named) != 0 || len(skipped) != 1 {
		t.Fatalf("an explicitly named unconfigured client must be skipped: %+v %v", named, skipped)
	}
	missing := []ClientReport{{Client: integrate.Codex, Status: StatusNeedsHuman, Detail: "client binary not found"}}
	if named, skipped := LivePlans(layout, missing, integrate.Codex); len(named) != 0 || len(skipped) != 1 {
		t.Fatalf("%+v %v", named, skipped)
	}
}

func TestLivePlansIncludeSlowClients(t *testing.T) {
	layout := integrate.Layout{LookPath: func(name string) (string, error) {
		if name == "kimi" {
			return "", errors.New("not found")
		}
		return "/bin/" + name, nil
	}}
	rows := []ClientReport{
		{Client: integrate.Codex, Status: StatusSlow, Detail: "47 shell reconciliations ran past their budget in 24 h"},
		{Client: integrate.Kimi, Status: StatusSlow},
	}
	want := LivePlan{Client: integrate.Codex, Binary: "/bin/codex", Model: "gpt-6.1-terra"}
	for _, only := range []string{"", integrate.Codex} {
		plans, skipped := LivePlans(layout, rows, only)
		if len(plans) != 1 || plans[0] != want {
			t.Fatalf("only %q: a slow client must run without a human prerequisite: %+v", only, plans)
		}
		wantSkipped := ""
		if only == "" {
			wantSkipped = "kimi: client binary not found"
		}
		if strings.Join(skipped, "\n") != wantSkipped {
			t.Fatalf("only %q: skipped %q, want %q", only, skipped, wantSkipped)
		}
	}
	if plans, skipped := LivePlans(layout, rows, integrate.Kimi); len(plans) != 0 || strings.Join(skipped, "\n") != "kimi: client binary not found" {
		t.Fatalf("a slow client still needs its binary: %+v %v", plans, skipped)
	}
}

func TestLiveInvocationsHaveNoTrustBypass(t *testing.T) {
	banned := []string{"--trust", "--always-approve", "--dangerously-skip-permissions", "--trust-folder", "--yolo", "bypassPermissions"}
	for _, name := range integrate.Names {
		spec, ok := liveClients[name]
		if !ok {
			t.Fatalf("%s has no live invocation", name)
		}
		// A client default may be an expensive model, so every client names one.
		if spec.model == "" {
			t.Fatalf("%s runs on its default model", name)
		}
		args := spec.args(spec.model, livePrompt("n"))
		if !strings.Contains(strings.Join(args, " "), livePrompt("n")) || !strings.Contains(strings.Join(args, " "), spec.model) {
			t.Fatalf("%s drops the prompt or the model: %v", name, args)
		}
		for _, a := range args {
			for _, bad := range banned {
				// Cursor's --trust trusts only the scratch repository doctor made.
				if name == integrate.Cursor && a == "--trust" {
					continue
				}
				if strings.Contains(a, bad) {
					t.Fatalf("%s live args contain %s: %v", name, bad, args)
				}
			}
		}
	}
}

func TestExecLiveClosesStdin(t *testing.T) {
	bin, err := integrate.PlaceFake(t.TempDir(), "codex")
	if err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(), "IRIS_FAKE_REQUIRE_CLOSED_STDIN=1", "IRIS_FAKE_EXIT=0", "IRIS_FAKE_STDOUT=done")
	// Generous: the first run of a fresh binary on Windows waits for the virus scanner.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := execLive(ctx, t.TempDir(), env, bin, []string{"exec", "x"})
	if err != nil || strings.TrimSpace(string(out)) != "done" {
		t.Fatalf("out %q err %v", out, err)
	}
}

func TestClipKeepsTheStartOfLongOutput(t *testing.T) {
	if got := clip([]byte(" short \n")); got != "short" {
		t.Fatal(got)
	}
	if got := clip([]byte(strings.Repeat("x", 500))); got != strings.Repeat("x", 400) {
		t.Fatal(len(got))
	}
}
