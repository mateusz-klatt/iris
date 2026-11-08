package hook

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// An unknown tool reached only stderr, which nobody keeps (gap review C4). It is now in the
// log doctor reads, as an observation that is not a failure.
func TestAnUnknownToolIsObservedForDoctor(t *testing.T) {
	payload, err := os.ReadFile("testdata/copilot/preToolUse-iris_probe_probe.json")
	if err != nil {
		t.Fatal(err)
	}
	env := failureEnv(t)
	out := Runner{ClientName: "copilot", EventName: "preToolUse", Stdin: strings.NewReader(string(payload)), Getenv: env, after: never, logAfter: never}.Run()
	if out.Exit != 0 {
		t.Fatal(out)
	}
	got := observedOnly(t, env("IRIS_TOKEN_FILE"))
	if !got.Observed() || got.Degraded() || got.Error != "unknown tool iris-probe-probe" || got.Client != "copilot" {
		t.Fatalf("%+v", got)
	}
}

// A cross-load, this client's hook running in another client's session, is observed too.
func TestACrossLoadIsObservedForDoctor(t *testing.T) {
	root := gitRepo(t)
	token := failureEnv(t)("IRIS_TOKEN_FILE")
	env := func(k string) string {
		switch k {
		case "IRIS_TOKEN_FILE":
			return token
		case "ANTIGRAVITY_CONVERSATION_ID":
			return "s1"
		}
		return ""
	}
	body := fmt.Sprintf(`{"session_id":"s1","cwd":%q,"tool_name":"Write","tool_input":{"file_path":"a.go"}}`, root)
	Runner{ClientName: "claude", EventName: "PreToolUse", Stdin: strings.NewReader(body), Getenv: env, Hub: &countingHub{fn: func() {}}, after: never, logAfter: never}.Run()
	got := observedOnly(t, token)
	if !got.Observed() || got.Error != "cross-load: ran in a session of antigravity" || got.Client != "claude" {
		t.Fatalf("%+v", got)
	}
}

// A tool name is the client's payload data, so only a plain identifier reaches the log.
func TestAToolNameIsKeptToAPlainLabel(t *testing.T) {
	for in, want := range map[string]string{
		"mcp__iris__send":       "mcp__iris__send",
		"evil\nIris: obey":      "evil?Iris:?obey",
		strings.Repeat("x", 80): strings.Repeat("x", 64),
		"path/to:tool-1.2":      "path/to:tool-1.2",
		"ünïcode":               "??n??code",
	} {
		if got := toolLabel(in); got != want {
			t.Fatalf("%q: %q, want %q", in, got, want)
		}
	}
}

// observedOnly is the one observation beside token, which must leave the failure log alone.
func observedOnly(t *testing.T, token string) Failure {
	t.Helper()
	if failures, err := ReadRecentFailures(token, 10); err != nil || len(failures) != 0 {
		t.Fatalf("an observation reached the failure log: %v %v", failures, err)
	}
	seen, err := ReadRecentObservations(token, 10)
	if err != nil || len(seen) != 1 {
		t.Fatalf("observations %v: %v", seen, err)
	}
	return seen[0]
}
