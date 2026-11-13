package integrate

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// codexInstalled integrates Codex and returns its hooks.json and config.toml.
func codexInstalled(t *testing.T) (Layout, string, string) {
	t.Helper()
	l := testLayout(t)
	if err := (codexClient{}).Install(l, nil); err != nil {
		t.Fatal(err)
	}
	files, err := codexClient{}.Files(l)
	if err != nil {
		t.Fatal(err)
	}
	return l, files[0], files[1]
}

// trustAs writes what Codex writes when a person trusts the hooks in /hooks.
func trustAs(t *testing.T, config, hooksFile string, group int, events ...string) {
	t.Helper()
	raw, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		key := fmt.Sprintf("%s:%s:%d:0", hooksFile, e, group)
		raw = append(raw, fmt.Sprintf("\n[hooks.state.%s]\ntrusted_hash = \"sha256:%s\"\n", tomlString(key), strings.Repeat("0", 64))...)
	}
	if err = os.WriteFile(config, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

var allCodexEvents = []string{"session_start", "user_prompt_submit", "pre_tool_use", "post_tool_use", "stop", "session_end"}

func TestCodexReviewNeedsEveryIrisHookTrusted(t *testing.T) {
	l, hooks, config := codexInstalled(t)
	if step, _ := (codexClient{}).Review(l); step != "Review hooks once in /hooks" {
		t.Fatalf("unreviewed: %q", step)
	}
	trustAs(t, config, hooks, 0, allCodexEvents[:5]...)
	if step, _ := (codexClient{}).Review(l); step != "Review hooks once in /hooks" {
		t.Fatalf("five of six: %q", step)
	}
	trustAs(t, config, hooks, 0, "session_end")
	step, note := codexClient{}.Review(l)
	if step != "" || !strings.Contains(note, "hash not verified") || !strings.Contains(note, "iris doctor --live --client codex") {
		t.Fatalf("%q %q", step, note)
	}
}

func TestCodexReviewIsForTheExactHooksFile(t *testing.T) {
	l, hooks, config := codexInstalled(t)
	// A review recorded under another home, or a header Codex never wrote a hash under, does not count.
	trustAs(t, config, "/elsewhere/.codex/hooks.json", 0, allCodexEvents...)
	raw, _ := os.ReadFile(config)
	raw = append(raw, fmt.Sprintf("\n[hooks.state.%s]\nenabled = true\n", tomlString(hooks+":stop:0:0"))...)
	if err := os.WriteFile(config, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	trustAs(t, config, hooks, 0, allCodexEvents[:4]...)
	trustAs(t, config, hooks, 0, "session_end")
	if step, _ := (codexClient{}).Review(l); step != "Review hooks once in /hooks" {
		t.Fatalf("%q", step)
	}
}

func TestCodexReviewFindsIrisAfterTheUsersOwnHook(t *testing.T) {
	l, hooks, config := codexInstalled(t)
	raw, _ := os.ReadFile(hooks)
	// The user's hook comes first under every event, so Iris's group is 1.
	doc := string(raw)
	for _, e := range codexEvents {
		doc = strings.Replace(doc, `"`+e.event+`": [`, `"`+e.event+`": [{"hooks":[{"type":"command","command":"mine"}]},`, 1)
	}
	if err := os.WriteFile(hooks, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	trustAs(t, config, hooks, 0, allCodexEvents...)
	if step, _ := (codexClient{}).Review(l); step == "" {
		t.Fatal("the user's hook review was taken for Iris's")
	}
	trustAs(t, config, hooks, 1, allCodexEvents...)
	if step, note := (codexClient{}).Review(l); step != "" {
		t.Fatalf("%q %q", step, note)
	}
}

func TestCodexReviewNamesAnIrisTableFromBeforeTheMCPKeys(t *testing.T) {
	l, hooks, config := codexInstalled(t)
	trustAs(t, config, hooks, 0, allCodexEvents...)
	raw, _ := os.ReadFile(config)
	old := strings.Replace(strings.Replace(string(raw), "env_vars =", "# was env_vars =", 1), "default_tools_approval_mode =", "# was =", 1)
	if err := os.WriteFile(config, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	step, _ := codexClient{}.Review(l)
	if step != "the Iris MCP server table lacks env_vars and default_tools_approval_mode: run `iris integrate codex`" {
		t.Fatalf("%q", step)
	}
	if err := os.Remove(hooks); err != nil {
		t.Fatal(err)
	}
	if step, _ = (codexClient{}).Review(l); !strings.HasSuffix(step, "; Review hooks once in /hooks") {
		t.Fatalf("a missing hooks.json is not a review: %q", step)
	}
}

func TestCodexReviewWithoutAHomeIsTheFixedStep(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("USERPROFILE", "")
	if step, note := (codexClient{}).Review(Layout{Getenv: func(string) string { return "" }}); step != "Review hooks once in /hooks" || note != "" {
		t.Fatalf("%q %q", step, note)
	}
	if got := staleMCP("no iris block"); got != nil {
		t.Fatal(got)
	}
}

// windowsHooksState is the fixture's home, which a test replaces with its own hooks.json.
const windowsHooksState = `C:\Users\someone\.codex\hooks.json`

func TestCodexReviewReadsTheLiteralKeysCodexWritesOnWindows(t *testing.T) {
	l, hooks, config := codexInstalled(t)
	fixture, err := os.ReadFile("testdata/codex/hooks-state-windows.toml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(fixture), "[hooks.state.'"+windowsHooksState+":user_prompt_submit:0:0']") {
		t.Fatal("the fixture no longer has the literal-key shape this test is about")
	}
	if step, _ := (codexClient{}).Review(l); step != "Review hooks once in /hooks" {
		t.Fatalf("unreviewed: %q", step)
	}
	raw, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw, "\n"+strings.ReplaceAll(string(fixture), windowsHooksState, hooks)...)
	if err = os.WriteFile(config, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if step, note := (codexClient{}).Review(l); step != "" || note != codexReviewed {
		t.Fatalf("a review Codex recorded under literal keys was missed: %q %q", step, note)
	}
}

func TestCodexReviewTrustsNothingInAConfigThatDoesNotDecode(t *testing.T) {
	l, hooks, config := codexInstalled(t)
	trustAs(t, config, hooks, 0, allCodexEvents...)
	raw, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	// A hooks.state entry that is not a table fails decoding like any other error.
	if err = os.WriteFile(config, append(raw, "\n[hooks.state]\nloose = 1\n"...), 0o600); err != nil {
		t.Fatal(err)
	}
	if step, _ := (codexClient{}).Review(l); step != "Review hooks once in /hooks" {
		t.Fatalf("%q", step)
	}
}
