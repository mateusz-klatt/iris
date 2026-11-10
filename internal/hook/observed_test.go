package hook

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
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

// iris mcp records what a tool call's ids showed in the same log, as the client's event "mcp".
// The write is best effort and may finish after Observer returns, so the test waits for it.
func TestTheMCPServerObservesInTheHooksLog(t *testing.T) {
	env := failureEnv(t)
	Observer(env, "codex")("calls of two sessions")
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
		if seen, _ := ReadRecentObservations(env("IRIS_TOKEN_FILE"), 1); len(seen) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the write never finished")
		}
	}
	got := observedOnly(t, env("IRIS_TOKEN_FILE"))
	if !got.Observed() || got.Client != "codex" || got.Event != "mcp" || got.Error != "calls of two sessions" {
		t.Fatalf("%+v", got)
	}
}

// An observation never holds the MCP server's call: a write that cannot go ahead is left to finish
// later, past the budget, and one that panics is recovered.
func TestAnObservationNeverHoldsItsCaller(t *testing.T) {
	token := failureEnv(t)("IRIS_TOKEN_FILE")
	release := make(chan struct{})
	blocked := func(k string) string {
		<-release
		if k == "IRIS_TOKEN_FILE" {
			return token
		}
		return ""
	}
	done := make(chan struct{})
	go func() {
		Observer(blocked, "codex")("calls of two sessions")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the caller waited for a blocked write")
	}
	close(release)
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
		if seen, _ := ReadRecentObservations(token, 1); len(seen) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the write never finished")
		}
	}
	Observer(func(string) string { panic("a broken environment") }, "codex")("calls of two sessions")
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
