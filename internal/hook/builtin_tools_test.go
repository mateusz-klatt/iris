package hook

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// The built-in tools covered by this test were once logged as unknown on a real host and are
// known now. Codex's names were logged on home-wsl on 2026-11-08 and are exactly as logged. Codex
// joins a tool's namespace and name: its rollouts record
// {"namespace":"collaboration","name":"spawn_agent"}, which the hook sees as
// "collaborationspawn_agent". Claude Code's names are backed by captures in testdata/claude from
// 2.1.319 on Windows and 2.1.331 on Linux/WSL, which the golden test decodes. Antigravity 1.5.16
// and Kimi Code 2.7.3 names are backed by Linux/WSL captures from 2026-11-25. No Codex payload
// captures exist yet: Codex runs a new hook only after a person has trusted it.
func TestBuiltinToolsAreKnown(t *testing.T) {
	for name, want := range map[string]ToolClass{
		// Codex
		"clockcurr_time":             ClassOther,
		"clocksleep":                 ClassOther,
		"webrun":                     ClassOther,
		"collaborationspawn_agent":   ClassOther,
		"collaborationsend_message":  ClassOther,
		"collaborationfollowup_task": ClassOther,
		"collaborationlist_agents":   ClassOther,
		"collaborationwait_agent":    ClassOther,
		// Claude Code
		"Agent":            ClassOther,
		"PushNotification": ClassOther,
		"SendUserFile":     ClassOther,
		"TaskStop":         ClassOther,
		"ScheduleWakeup":   ClassOther,
		"Monitor":          ClassShell,
		// Antigravity
		"read_file":     ClassRead,
		"update_steps":  ClassOther,
		"manage_task":   ClassOther,
		"search_web":    ClassOther,
		"call_mcp_tool": ClassMCP,
		// Kimi Code
		"TodoList": ClassOther,
		"FetchURL": ClassOther,
	} {
		class, unknown := classifyTool(name, false)
		if class != want || unknown {
			t.Errorf("%s: %s, unknown %v; want %s and known", name, class, unknown, want)
		}
	}
}

// The captured non-writing built-ins allow the event, touch no hub and add nothing to the
// observation log doctor reads. The golden expectations also require no extracted paths, including
// SendUserFile's files array.
func TestClaudeBuiltinsAllowWithoutObservation(t *testing.T) {
	for _, tool := range []string{"agent", "pushnotification", "senduserfile"} {
		for _, event := range []string{"PreToolUse", "PostToolUse"} {
			name := event + "-" + tool
			t.Run(name, func(t *testing.T) {
				payload, err := os.ReadFile("testdata/claude/" + name + ".json")
				if err != nil {
					t.Fatal(err)
				}
				env := failureEnv(t)
				calls := 0
				out := Runner{
					ClientName: "claude",
					EventName:  event,
					Stdin:      strings.NewReader(string(payload)),
					Getenv:     env,
					Hub:        &countingHub{fn: func() { calls++ }},
					after:      never,
					logAfter:   never,
				}.Run()
				assertAllow(t, ClientClaude, out)
				if len(out.Stderr) != 0 || calls != 0 {
					t.Fatalf("stderr %q, hub calls %d", out.Stderr, calls)
				}
				observations, err := ReadRecentObservations(env("IRIS_TOKEN_FILE"), 10)
				if err != nil || len(observations) != 0 {
					t.Fatalf("observations %v: %v", observations, err)
				}
			})
		}
	}
}

func assertToolEventAllow(t *testing.T, client Client, eventName string, out Output) {
	t.Helper()
	want := Encode(Event{Client: client, Kind: kindFrom(eventName, "")}, Decision{Action: ActionAllow})
	if out.Exit != want.Exit || !bytes.Equal(out.Stdout, want.Stdout) {
		t.Fatalf("got stdout %q stderr %q exit %d; want stdout %q exit %d", out.Stdout, out.Stderr, out.Exit, want.Stdout, want.Exit)
	}
}

// Every captured Antigravity and Kimi built-in, including generic MCP dispatch, returns the exact
// allow output without a hub call or an observation. Golden expectations cover path extraction.
func TestAntigravityAndKimiBuiltinsAllowWithoutObservation(t *testing.T) {
	for _, tc := range []struct {
		client    Client
		pre, post string
	}{
		{ClientAntigravity, "pre_tool_use_read_file", "post_tool_use_read_file"},
		{ClientAntigravity, "pre_tool_use_update_steps", "post_tool_use_update_steps"},
		{ClientAntigravity, "pre_tool_use_update_steps_done", "post_tool_use_update_steps_done"},
		{ClientAntigravity, "pre_tool_use_search_web", "post_tool_use_search_web"},
		{ClientAntigravity, "pre_tool_use_call_mcp_tool", "post_tool_use_call_mcp_tool"},
		{ClientAntigravity, "pre_tool_use_manage_task", "post_tool_use_manage_task"},
		{ClientKimi, "pre_tool_use_todolist_reading", "posttooluse_todolist_reading"},
		{ClientKimi, "pre_tool_use_todolist_fetching", "posttooluse_todolist_fetching"},
		{ClientKimi, "pre_tool_use_todolist_done", "posttooluse_todolist_done"},
		{ClientKimi, "pre_tool_use_fetchurl", "posttooluse_fetchurl"},
	} {
		for _, name := range []string{tc.pre, tc.post} {
			t.Run(string(tc.client)+"/"+name, func(t *testing.T) {
				client := string(tc.client)
				payload, err := os.ReadFile("testdata/" + client + "/" + name + ".json")
				if err != nil {
					t.Fatal(err)
				}
				env := failureEnv(t)
				calls := 0
				out := Runner{
					ClientName: client,
					EventName:  argvEvent(client, name+".json"),
					Stdin:      bytes.NewReader(payload),
					Getenv:     env,
					Hub:        &countingHub{fn: func() { calls++ }},
					after:      never,
					logAfter:   never,
				}.Run()
				assertToolEventAllow(t, tc.client, argvEvent(client, name+".json"), out)
				if len(out.Stderr) != 0 || calls != 0 {
					t.Fatalf("stderr %q, hub calls %d", out.Stderr, calls)
				}
				observations, err := ReadRecentObservations(env("IRIS_TOKEN_FILE"), 10)
				if err != nil || len(observations) != 0 {
					t.Fatalf("observations %v: %v", observations, err)
				}
			})
		}
	}
}

func TestAntigravityCallMCPToolMatchesOrdinaryMCP(t *testing.T) {
	for _, name := range []string{"pre_tool_use_call_mcp_tool", "post_tool_use_call_mcp_tool"} {
		t.Run(name, func(t *testing.T) {
			payload, err := os.ReadFile("testdata/antigravity/" + name + ".json")
			if err != nil {
				t.Fatal(err)
			}
			ordinary := bytes.Replace(payload, []byte(`"name":"call_mcp_tool"`), []byte(`"name":"mcp__iris__whoami"`), 1)
			if bytes.Equal(ordinary, payload) {
				t.Fatal("MCP tool name was not replaced")
			}
			run := func(input []byte) (Output, int) {
				calls := 0
				out := Runner{
					ClientName: "antigravity",
					EventName:  argvEvent("antigravity", name+".json"),
					Stdin:      bytes.NewReader(input),
					Getenv:     failureEnv(t),
					Hub:        &countingHub{fn: func() { calls++ }},
					after:      never,
					logAfter:   never,
				}.Run()
				return out, calls
			}
			got, gotCalls := run(payload)
			want, wantCalls := run(ordinary)
			if got.Exit != want.Exit || !bytes.Equal(got.Stdout, want.Stdout) ||
				!bytes.Equal(got.Stderr, want.Stderr) || gotCalls != wantCalls {
				t.Fatalf("dispatcher %+v (%d calls); ordinary MCP %+v (%d calls)", got, gotCalls, want, wantCalls)
			}
			assertToolEventAllow(t, ClientAntigravity, argvEvent("antigravity", name+".json"), got)
			if gotCalls != 0 {
				t.Fatalf("hub calls %d", gotCalls)
			}
		})
	}
}

// The cron captures cover session-only jobs. Durable jobs write .claude/scheduled_tasks.json, so
// both tools stay unknown and observed while the hook still allows.
func TestClaudeCronToolsStayUnknownAndAllow(t *testing.T) {
	for _, tc := range []struct {
		file string
		tool string
	}{
		{"croncreate", "CronCreate"},
		{"crondelete", "CronDelete"},
	} {
		for _, event := range []string{"PreToolUse", "PostToolUse"} {
			name := event + "-" + tc.file
			t.Run(name, func(t *testing.T) {
				payload, err := os.ReadFile("testdata/claude/" + name + ".json")
				if err != nil {
					t.Fatal(err)
				}
				env := failureEnv(t)
				calls := 0
				out := Runner{
					ClientName: "claude",
					EventName:  event,
					Stdin:      strings.NewReader(string(payload)),
					Getenv:     env,
					Hub:        &countingHub{fn: func() { calls++ }},
					after:      never,
					logAfter:   never,
				}.Run()
				assertAllow(t, ClientClaude, out)
				wantStderr := "iris: unknown tool " + tc.tool + "\n"
				if string(out.Stderr) != wantStderr || calls != 0 {
					t.Fatalf("stderr %q, hub calls %d; want stderr %q", out.Stderr, calls, wantStderr)
				}
				got := observedOnly(t, env("IRIS_TOKEN_FILE"))
				if !got.Observed() || got.Degraded() || got.Error != "unknown tool "+tc.tool || got.Client != "claude" {
					t.Fatalf("%+v", got)
				}
			})
		}
	}
}

// Monitor runs a shell command in the background, so its command is read and reconciled as a
// shell's, never taken as paths. That reconciliation sees only what is written before
// PostToolUse, which for a background command is usually nothing.
func TestMonitorIsReconciledLikeAShell(t *testing.T) {
	var ev Event
	applyTool(&ev, "Monitor", json.RawMessage(`{"command":"tail -f build.log > copy.log","description":"build","timeout_ms":60000}`), false)
	if ev.ToolClass != ClassShell || ev.Command != "tail -f build.log > copy.log" || len(ev.Paths) != 0 || ev.UnknownTool {
		t.Fatalf("%+v", ev)
	}
}

// A name that only resembles one of them stays unknown, so it is still reported.
func TestANearNameStaysUnknown(t *testing.T) {
	for _, name := range []string{"clock_curr_time", "collaboration.spawn_agent", "webrunner"} {
		if class, unknown := classifyTool(name, false); class != ClassOther || !unknown {
			t.Errorf("%s: %s, unknown %v", name, class, unknown)
		}
	}
}
