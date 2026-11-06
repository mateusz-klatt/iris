package integrate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

type failingPlugins struct {
	fakePlugins
	add, uninstall, remove error
}

func (f *failingPlugins) MarketplaceAdd(string) error    { return f.add }
func (f *failingPlugins) Uninstall(string) error         { return f.uninstall }
func (f *failingPlugins) MarketplaceRemove(string) error { return f.remove }

func TestClientsWithoutAHomeReportIt(t *testing.T) {
	for _, c := range All() {
		t.Run(c.Name(), func(t *testing.T) {
			l := homeless(t)
			if _, err := c.Files(l); err == nil {
				t.Error("Files")
			}
			if err := c.Install(l, &fakePlugins{}); err == nil {
				t.Error("Install")
			}
			if err := c.Remove(l, &fakePlugins{}); err == nil {
				t.Error("Remove")
			}
			if _, err := c.Canary(l); err == nil {
				t.Error("Canary")
			}
		})
	}
}

func TestCanaryBeforeInstall(t *testing.T) {
	for _, c := range All() {
		t.Run(c.Name(), func(t *testing.T) {
			if _, err := c.Canary(testLayout(t)); !errors.Is(err, ErrNotConfigured) {
				t.Fatal(err)
			}
		})
	}
}

// hookFile is the file Canary reads.
func hookFile(t *testing.T, c Client, l Layout) string {
	t.Helper()
	files, err := c.Files(l)
	if err != nil {
		t.Fatal(err)
	}
	if c.Name() == Claude {
		return files[2]
	}
	return files[0]
}

func TestCanaryOnFilesWithoutIris(t *testing.T) {
	bodies := map[string]string{
		"nothing":        "{}",
		"event no hook":  `{"hooks":{"preToolUse":[{}]},"iris":{"PreToolUse":[{}]}}`,
		"no event match": `{"iris":{"PreToolUse":[{"hooks":[{"command":"other"}]}]}}`,
	}
	for _, c := range All() {
		if c.Name() == Kimi {
			continue
		}
		for name, body := range bodies {
			t.Run(c.Name()+"/"+name, func(t *testing.T) {
				l := testLayout(t)
				seedFile(t, hookFile(t, c, l), body)
				if _, err := c.Canary(l); !errors.Is(err, ErrNotConfigured) {
					t.Fatal(err)
				}
			})
		}
		t.Run(c.Name()+"/broken", func(t *testing.T) {
			l := testLayout(t)
			seedFile(t, hookFile(t, c, l), "{")
			if _, err := c.Canary(l); !errors.Is(err, ErrBroken) {
				t.Fatal(err)
			}
		})
	}
}

func TestCanaryOnADirectory(t *testing.T) {
	for _, c := range All() {
		t.Run(c.Name(), func(t *testing.T) {
			l := testLayout(t)
			if err := os.MkdirAll(hookFile(t, c, l), 0o700); err != nil {
				t.Fatal(err)
			}
			if _, err := c.Canary(l); err == nil || errors.Is(err, ErrNotConfigured) {
				t.Fatal(err)
			}
		})
	}
}

func TestCopilotCanaryPicksTheShellOfThePlatform(t *testing.T) {
	l := testLayout(t)
	c, _ := ByName(Copilot)
	if err := c.Install(l, nil); err != nil {
		t.Fatal(err)
	}
	l.GOOS = "windows"
	in, err := c.Canary(l)
	if err != nil {
		t.Fatal(err)
	}
	if in.Kind != ShellPowerShell || !strings.Contains(in.Shell, "Get-Command") {
		t.Fatalf("%+v", in)
	}
}

func TestKimiCanaryReadsOnlyItsOwnBlock(t *testing.T) {
	cases := map[string]string{
		"no block":     "other = 1\n",
		"no event":     wrapBlock("[[hooks]]\nevent = \"Stop\"\n"),
		"no command":   wrapBlock("[[hooks]]\nevent = \"SessionStart\"\n"),
		"not a string": wrapBlock("[[hooks]]\nevent = \"SessionStart\"\ncommand = bare --event SessionStart\n"),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			l := testLayout(t)
			c, _ := ByName(Kimi)
			seedFile(t, hookFile(t, c, l), body)
			if _, err := c.Canary(l); !errors.Is(err, ErrNotConfigured) {
				t.Fatal(err)
			}
		})
	}
}

func TestTOMLStringsRoundTrip(t *testing.T) {
	want := "a\tb\nc\"d\\e"
	if got := unquoteTOML(tomlString(want)); got != want {
		t.Fatalf("%q", got)
	}
}

func TestInstallRefusesAShapeItCannotExtend(t *testing.T) {
	cases := []struct {
		client, body string
	}{
		{Codex, `{"hooks":{"Stop":1}}`},
		{Grok, `{"hooks":{"Stop":1}}`},
		{Copilot, `{"hooks":{"sessionStart":1}}`},
		{Copilot, `{"version":2}`},
		{Copilot, `{"version":"1"}`},
		{Cursor, `{"hooks":{"sessionStart":1}}`},
		{Cursor, `{"version":2}`},
		{Claude, `{"hooks":{"Stop":1}}`},
	}
	for _, tc := range cases {
		t.Run(tc.client+" "+tc.body, func(t *testing.T) {
			l := testLayout(t)
			c, _ := ByName(tc.client)
			target := hookFile(t, c, l)
			seedFile(t, target, tc.body)
			if err := c.Install(l, &fakePlugins{}); !errors.Is(err, ErrBroken) {
				t.Fatal(err)
			}
			if got := string(mustRead(t, target)); got != tc.body {
				t.Fatalf("%q", got)
			}
		})
	}
}

func TestInstallStopsAtABrokenSecondFile(t *testing.T) {
	for _, c := range All() {
		if c.Name() == Claude {
			continue
		}
		t.Run(c.Name(), func(t *testing.T) {
			l := testLayout(t)
			files, _ := c.Files(l)
			// Nothing but a marker or an open brace, whichever the format refuses.
			body := "{"
			if strings.HasSuffix(files[1], ".toml") {
				body = markerStart + "\n"
			}
			seedFile(t, files[1], body)
			if err := c.Install(l, nil); !errors.Is(err, ErrBroken) {
				t.Fatal(err)
			}
		})
	}
}

func TestInstallSitsBesideOtherHooks(t *testing.T) {
	cases := []struct {
		client, event, body string
	}{
		{Codex, "Stop", `{"hooks":{"Stop":[{"hooks":[{"command":"mine"}]}]}}`},
		{Cursor, "sessionStart", `{"version":1,"hooks":{"sessionStart":[{"command":"mine"}]}}`},
	}
	for _, tc := range cases {
		t.Run(tc.client, func(t *testing.T) {
			l := testLayout(t)
			c, _ := ByName(tc.client)
			target := hookFile(t, c, l)
			seedFile(t, target, tc.body)
			if err := c.Install(l, nil); err != nil {
				t.Fatal(err)
			}
			text := string(mustRead(t, target))
			if !strings.Contains(text, "mine") || !strings.Contains(text, "--client "+tc.client+" --event "+tc.event) {
				t.Fatalf("%s", text)
			}
		})
	}
}

func TestClaudeRefusesToRunWithoutAPluginInstaller(t *testing.T) {
	c, _ := ByName(Claude)
	l := testLayout(t)
	if err := c.Install(l, nil); err == nil {
		t.Fatal("Install")
	}
	if err := c.Remove(l, nil); err == nil {
		t.Fatal("Remove")
	}
}

func TestClaudeInstallReportsAPluginFailure(t *testing.T) {
	c, _ := ByName(Claude)
	failure := errors.New("marketplace")
	if err := c.Install(testLayout(t), &failingPlugins{add: failure}); !errors.Is(err, failure) {
		t.Fatal(err)
	}
}

func TestClaudeRemoveFinishesFilesAfterAPluginFailure(t *testing.T) {
	first := errors.New("uninstall")
	second := errors.New("marketplace")
	cases := []struct {
		name string
		p    Plugins
		want error
	}{
		{"uninstall wins", &failingPlugins{uninstall: first, remove: second}, first},
		{"marketplace", &failingPlugins{remove: second}, second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := testLayout(t)
			c, _ := ByName(Claude)
			if err := c.Install(l, &fakePlugins{}); err != nil {
				t.Fatal(err)
			}
			if err := c.Remove(l, tc.p); !errors.Is(err, tc.want) {
				t.Fatal(err)
			}
			files, _ := c.Files(l)
			if _, err := os.Stat(files[0]); !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
		})
	}
}

func TestClaudeRemoveReportsABrokenFileAndThePluginFailure(t *testing.T) {
	l := testLayout(t)
	c, _ := ByName(Claude)
	if err := c.Install(l, &fakePlugins{}); err != nil {
		t.Fatal(err)
	}
	files, _ := c.Files(l)
	if err := os.WriteFile(files[2], []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("uninstall")
	err := c.Remove(l, &failingPlugins{uninstall: failure})
	if !errors.Is(err, failure) || !errors.Is(err, ErrBroken) {
		t.Fatal(err)
	}
}

func TestMergeClaudeRefusesAnInvalidTemplate(t *testing.T) {
	if _, err := mergeClaude("x.json", nil, []byte("{")); err == nil {
		t.Fatal("no error")
	}
}

func TestMergeText(t *testing.T) {
	if got := string(mergeText(nil, []byte("a"))); got != "a\n" {
		t.Fatalf("%q", got)
	}
	if got := string(mergeText([]byte("mine"), []byte("a\n"))); got != "mine\na\n" {
		t.Fatalf("%q", got)
	}
}

func TestMergersRefuseBrokenInput(t *testing.T) {
	mergers := map[string]func(existing, rendered []byte) ([]byte, error){
		"objects": mergeObjects, "mcp": mergeMCPFile, "hooks": mergeClaudeHooks,
	}
	for name, merge := range mergers {
		t.Run(name, func(t *testing.T) {
			if _, err := merge([]byte("{"), []byte("{}")); !errors.Is(err, ErrBroken) {
				t.Fatal(err)
			}
			if _, err := merge(nil, []byte("[]")); err == nil {
				t.Fatal("a template that is not an object")
			}
		})
	}
	if _, err := mergeMonitors([]byte("{"), []byte("[]")); !errors.Is(err, ErrBroken) {
		t.Fatal(err)
	}
	if _, err := mergeMonitors([]byte("{}"), []byte("[]")); !errors.Is(err, ErrBroken) {
		t.Fatal(err)
	}
	if _, err := mergeMonitors(nil, []byte("{}")); err == nil {
		t.Fatal("a template that is not a list")
	}
}

func TestMergeMCPFileAddsOnlyWhatIsMissing(t *testing.T) {
	got, err := mergeMCPFile([]byte(`{"own":"mine","mcpServers":{"other":{}}}`),
		[]byte(`{"own":"theirs","extra":1,"mcpServers":{"iris":{"command":"i"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	text := string(got)
	for _, want := range []string{`"mine"`, `"extra"`, `"other"`, `"iris"`} {
		if !strings.Contains(text, want) {
			t.Fatalf("no %s in\n%s", want, text)
		}
	}
	if strings.Contains(text, "theirs") {
		t.Fatalf("overwrote\n%s", text)
	}
}

func TestMergeClaudeHooksSkipsEmptyTemplateEntries(t *testing.T) {
	got, err := mergeClaudeHooks(nil, []byte(`{"hooks":{"A":[],"B":[{"hooks":[]}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), "hooks") {
		t.Fatalf("%s", got)
	}
}

func TestMergeMonitorsKeepsOthersAndReplacesItsOwn(t *testing.T) {
	got, err := mergeMonitors([]byte(`[{"name":"iris","command":"old"},{"name":"mine"}]`), []byte(`[{"name":"iris","command":"new"}]`))
	if err != nil {
		t.Fatal(err)
	}
	text := string(got)
	if strings.Contains(text, "old") || !strings.Contains(text, "new") || !strings.Contains(text, "mine") {
		t.Fatalf("%s", text)
	}
	got, err = mergeMonitors([]byte(`[{"name":"iris"}]`), []byte(`[]`))
	if err != nil || !strings.Contains(string(got), "iris") {
		t.Fatalf("%s %v", got, err)
	}
}

func TestExecPluginsRunsTheClaudeCommand(t *testing.T) {
	var got []string
	p := ExecPlugins{Run: func(name string, args ...string) error {
		got = append(got, name+" "+strings.Join(args, " "))
		return nil
	}}
	if err := p.Uninstall("iris@iris"); err != nil {
		t.Fatal(err)
	}
	if err := p.MarketplaceRemove("iris"); err != nil {
		t.Fatal(err)
	}
	want := "claude plugin uninstall iris@iris\nclaude plugin marketplace remove iris"
	if strings.Join(got, "\n") != want {
		t.Fatal(got)
	}
}

func TestExecPluginsReportsWhatTheCommandSaid(t *testing.T) {
	dir := t.TempDir()
	if _, err := PlaceFake(dir, "claude"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("IRIS_FAKE_STDOUT", "")
	t.Setenv("IRIS_FAKE_EXIT", "")
	if err := (ExecPlugins{}).Install("iris@iris"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("IRIS_FAKE_EXIT", "3")
	if err := (ExecPlugins{}).Install("iris@iris"); err == nil || strings.Contains(err.Error(), ": ") {
		t.Fatalf("a silent failure is just the exit: %v", err)
	}
	t.Setenv("IRIS_FAKE_STDOUT", "plugin not found")
	if err := (ExecPlugins{}).Install("iris@iris"); err == nil || !strings.Contains(err.Error(), "plugin not found") {
		t.Fatal(err)
	}
}

func TestPutFlatHookAppendsBesideOtherHooks(t *testing.T) {
	doc := map[string]any{"hooks": map[string]any{"E": []any{map[string]any{"command": "mine"}}}}
	if err := putFlatHook(doc, "E", map[string]any{"command": "x --client c"}, "--client c"); err != nil {
		t.Fatal(err)
	}
	if got := doc["hooks"].(map[string]any)["E"].([]any); len(got) != 2 {
		t.Fatal(got)
	}
}

func TestFindNamedStringSkipsAKeyWithoutTheNeedle(t *testing.T) {
	v := map[string]any{"cmd": "other", "list": []any{map[string]any{"cmd": "wanted --x"}}}
	if got, ok := findNamedString(v, "cmd", "--x"); !ok || got != "wanted --x" {
		t.Fatal(got, ok)
	}
}

func TestGuardFailsOpenWithTheClientsOwnAnswer(t *testing.T) {
	if got := Guard("linux", Cursor, "iris", "preToolUse"); !strings.Contains(got, `{"permission":"allow"}`) {
		t.Fatal(got)
	}
	got := Guard("windows", Antigravity, `C:\Program Files\iris.exe`, "Stop")
	if !strings.Contains(got, `"C:\Program Files\iris.exe" hook`) {
		t.Fatal(got)
	}
}

func TestReadJSONSeparatesAbsentFromBroken(t *testing.T) {
	dir := t.TempDir()
	if _, err := readJSON(dir); err == nil || errors.Is(err, ErrNotConfigured) {
		t.Fatal(err)
	}
	if _, err := readJSON(filepath.Join(dir, "absent")); !errors.Is(err, ErrNotConfigured) {
		t.Fatal(err)
	}
}

func TestUpsertRefusesWhatKimiWouldDrop(t *testing.T) {
	if _, err := upsertHooks(nil, wrapBlock("[[hooks]]\npermission = 1\n")); !errors.Is(err, ErrBroken) {
		t.Fatal(err)
	}
	if _, err := upsertBlock("a\x00", "b"); !errors.Is(err, ErrBroken) {
		t.Fatal(err)
	}
}

func TestExeName(t *testing.T) {
	for _, tc := range []struct{ goos, name, want string }{
		{"windows", "iris", "iris.exe"},
		{"windows", "IRIS.EXE", "IRIS.EXE"},
		{"linux", "iris", "iris"},
	} {
		if got := exeName(tc.goos, tc.name); got != tc.want {
			t.Errorf("%+v: %q", tc, got)
		}
	}
}

func freshFake(t *testing.T) {
	t.Helper()
	if _, err := fakeBinary(); err != nil {
		t.Fatal(err)
	}
	exe := fakeExe
	fakeOnce, fakeExe, fakeErr = sync.Once{}, "", nil
	t.Cleanup(func() {
		fakeOnce, fakeExe, fakeErr = sync.Once{}, exe, nil
		fakeOnce.Do(func() {})
	})
}

func TestPlaceFakeReportsABuildFailure(t *testing.T) {
	freshFake(t)
	t.Setenv("PATH", t.TempDir())
	if _, err := PlaceFake(t.TempDir(), "x"); err == nil || !strings.Contains(err.Error(), "build fakebin") {
		t.Fatal(err)
	}
}

func TestPlaceFakeReportsATempFailure(t *testing.T) {
	freshFake(t)
	missing := filepath.Join(t.TempDir(), "absent")
	for _, name := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(name, missing)
	}
	if _, err := PlaceFake(t.TempDir(), "x"); err == nil {
		t.Fatal("no error")
	}
}

func TestPlaceFakeReportsADestinationFailure(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "file")
	seedFile(t, file, "x")
	if _, err := PlaceFake(filepath.Join(file, "below"), "x"); err == nil {
		t.Fatal("placed beneath a file")
	}
	taken := exeName(runtime.GOOS, "taken")
	if err := os.Mkdir(filepath.Join(dir, taken), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := PlaceFake(dir, taken); err == nil {
		t.Fatal("replaced a directory")
	}
}

func TestCopyFileFailures(t *testing.T) {
	dir := t.TempDir()
	if err := copyFile(filepath.Join(dir, "absent"), filepath.Join(dir, "out")); err == nil {
		t.Fatal("copied nothing")
	}
	if err := copyFile(dir, filepath.Join(dir, "out")); err == nil {
		t.Fatal("copied a directory")
	}
}

func TestInvocationRun(t *testing.T) {
	dir := t.TempDir()
	bin, err := PlaceFake(dir, "fake")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	t.Setenv("IRIS_FAKE_STDIN", "payload")
	t.Setenv("IRIS_FAKE_STDOUT", "answer")
	t.Setenv("IRIS_FAKE_EXIT", "7")
	out, code, err := Invocation{Argv: []string{bin}}.Run(ctx, []byte("payload"))
	if err != nil || code != 7 || strings.TrimSpace(string(out)) != "answer" {
		t.Fatalf("%q %d %v", out, code, err)
	}
	t.Setenv("IRIS_FAKE_EXIT", "0")
	out, code, err = Invocation{Argv: []string{bin}}.Run(ctx, []byte("payload"))
	if err != nil || code != 0 || strings.TrimSpace(string(out)) != "answer" {
		t.Fatalf("%q %d %v", out, code, err)
	}
}

func TestInvocationRunWithoutAProgram(t *testing.T) {
	ctx := context.Background()
	if _, code, err := (Invocation{}).Run(ctx, nil); err == nil || code != -1 {
		t.Fatal(code, err)
	}
	missing := filepath.Join(t.TempDir(), "absent")
	if _, code, err := (Invocation{Argv: []string{missing}}).Run(ctx, nil); err == nil || code != -1 {
		t.Fatal(code, err)
	}
}
