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
	for client, tools := range map[Client]map[string]ToolClass{
		ClientCodex: {
			"clockcurr_time":             ClassOther,
			"clocksleep":                 ClassOther,
			"webrun":                     ClassOther,
			"collaborationspawn_agent":   ClassOther,
			"collaborationsend_message":  ClassOther,
			"collaborationfollowup_task": ClassOther,
			"collaborationlist_agents":   ClassOther,
			"collaborationwait_agent":    ClassOther,
		},
		ClientClaude: {
			"Agent":            ClassOther,
			"PushNotification": ClassOther,
			"SendUserFile":     ClassOther,
			"TaskStop":         ClassOther,
			"ScheduleWakeup":   ClassOther,
			"Monitor":          ClassShell,
		},
		ClientAntigravity: {
			"read_file":     ClassRead,
			"update_steps":  ClassOther,
			"search_web":    ClassOther,
			"call_mcp_tool": ClassMCP,
		},
		ClientKimi: {
			"TodoList": ClassOther,
			"FetchURL": ClassOther,
		},
	} {
		for name, want := range tools {
			t.Run(string(client)+"/"+name, func(t *testing.T) {
				class, unknown := classifyTool(client, name, false)
				if class != want || unknown {
					t.Errorf("%s: %s, unknown %v; want %s and known", name, class, unknown, want)
				}
			})
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

// The captured non-writing Antigravity and Kimi built-ins, including generic MCP dispatch, return
// the exact allow output without a hub call or an observation. Golden expectations cover paths.
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

// Passive list and write-capable send_input alike stay unknown and off shell reconciliation and
// the network. The real capture is changed in memory only for send_input.
func TestAntigravityManageTaskStaysUnknownAndAllows(t *testing.T) {
	for _, name := range []string{"pre_tool_use_manage_task", "post_tool_use_manage_task"} {
		for _, action := range []string{"list", "send_input"} {
			t.Run(name+"/"+action, func(t *testing.T) {
				payload, err := os.ReadFile("testdata/antigravity/" + name + ".json")
				if err != nil {
					t.Fatal(err)
				}
				if action == "send_input" {
					changed := bytes.Replace(payload, []byte(`"Action":"list"`), []byte(`"Action":"send_input"`), 1)
					if bytes.Equal(changed, payload) {
						t.Fatal("manage_task action was not replaced")
					}
					payload = changed
				}
				eventName := argvEvent("antigravity", name+".json")
				ev, err := Decode(ClientAntigravity, eventName, payload)
				if err != nil {
					t.Fatal(err)
				}
				if ev.ToolClass != ClassOther || !ev.UnknownTool || shellEvent(ev) {
					t.Fatalf("class %q, unknown %v, shell event %v", ev.ToolClass, ev.UnknownTool, shellEvent(ev))
				}
				env := failureEnv(t)
				calls := 0
				out := Runner{
					ClientName: "antigravity",
					EventName:  eventName,
					Stdin:      bytes.NewReader(payload),
					Getenv:     env,
					Hub:        &countingHub{fn: func() { calls++ }},
					after:      never,
					logAfter:   never,
				}.Run()
				assertToolEventAllow(t, ClientAntigravity, eventName, out)
				if string(out.Stderr) != "iris: unknown tool manage_task\n" || calls != 0 {
					t.Fatalf("stderr %q, hub calls %d", out.Stderr, calls)
				}
				got := observedOnly(t, env("IRIS_TOKEN_FILE"))
				if !got.Observed() || got.Degraded() || got.Error != "unknown tool manage_task" || got.Client != "antigravity" {
					t.Fatalf("%+v", got)
				}
			})
		}
	}
}

func TestAntigravityCallMCPToolMatchesOrdinaryMCP(t *testing.T) {
	for _, tc := range []struct {
		name, server, tool string
	}{
		{"whoami", "iris", "whoami"},
		{"filesystem_write_file", "filesystem", "write_file"},
	} {
		for _, name := range []string{"pre_tool_use_call_mcp_tool", "post_tool_use_call_mcp_tool"} {
			t.Run(tc.name+"/"+name, func(t *testing.T) {
				payload, err := os.ReadFile("testdata/antigravity/" + name + ".json")
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Contains(payload, []byte(`"ServerName":"iris"`)) ||
					!bytes.Contains(payload, []byte(`"ToolName":"whoami"`)) {
					t.Fatal("capture does not contain the original MCP server and tool")
				}
				dispatched := bytes.Replace(payload, []byte(`"ServerName":"iris"`), []byte(`"ServerName":"`+tc.server+`"`), 1)
				dispatched = bytes.Replace(dispatched, []byte(`"ToolName":"whoami"`), []byte(`"ToolName":"`+tc.tool+`"`), 1)
				ordinaryName := `"name":"mcp__` + tc.server + `__` + tc.tool + `"`
				ordinary := bytes.Replace(dispatched, []byte(`"name":"call_mcp_tool"`), []byte(ordinaryName), 1)
				if bytes.Equal(ordinary, dispatched) {
					t.Fatal("MCP tool name was not replaced")
				}
				eventName := argvEvent("antigravity", name+".json")
				gotEvent, err := Decode(ClientAntigravity, eventName, dispatched)
				if err != nil {
					t.Fatal(err)
				}
				wantEvent, err := Decode(ClientAntigravity, eventName, ordinary)
				if err != nil {
					t.Fatal(err)
				}
				if gotEvent.ToolClass != ClassMCP || gotEvent.UnknownTool ||
					gotEvent.ToolClass != wantEvent.ToolClass || gotEvent.UnknownTool != wantEvent.UnknownTool {
					t.Fatalf("dispatcher class %q unknown %v; ordinary MCP class %q unknown %v",
						gotEvent.ToolClass, gotEvent.UnknownTool, wantEvent.ToolClass, wantEvent.UnknownTool)
				}
				run := func(input []byte) (Output, int) {
					calls := 0
					env := failureEnv(t)
					out := Runner{
						ClientName: "antigravity",
						EventName:  eventName,
						Stdin:      bytes.NewReader(input),
						Getenv:     env,
						Hub:        &countingHub{fn: func() { calls++ }},
						after:      never,
						logAfter:   never,
					}.Run()
					observations, err := ReadRecentObservations(env("IRIS_TOKEN_FILE"), 10)
					if err != nil || len(observations) != 0 {
						t.Fatalf("observations %v: %v", observations, err)
					}
					return out, calls
				}
				got, gotCalls := run(dispatched)
				want, wantCalls := run(ordinary)
				if got.Exit != want.Exit || !bytes.Equal(got.Stdout, want.Stdout) ||
					!bytes.Equal(got.Stderr, want.Stderr) || gotCalls != wantCalls {
					t.Fatalf("dispatcher %+v (%d calls); ordinary MCP %+v (%d calls)", got, gotCalls, want, wantCalls)
				}
				assertToolEventAllow(t, ClientAntigravity, eventName, got)
				if gotCalls != 0 {
					t.Fatalf("hub calls %d", gotCalls)
				}
			})
		}
	}
}

func TestCapturedToolNamesStayUnknownForOtherClients(t *testing.T) {
	for _, tc := range []struct {
		owner Client
		tool  string
	}{
		{ClientAntigravity, "read_file"},
		{ClientAntigravity, "update_steps"},
		{ClientAntigravity, "search_web"},
		{ClientAntigravity, "call_mcp_tool"},
		{ClientKimi, "TodoList"},
		{ClientKimi, "FetchURL"},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			for _, client := range Clients {
				if client == tc.owner {
					continue
				}
				class, unknown := classifyTool(client, tc.tool, false)
				if class != ClassOther || !unknown {
					t.Errorf("%s: class %q, unknown %v; want other and unknown", client, class, unknown)
				}
			}

			payload := `{"session_id":"s1","cwd":"/workspace/project","hook_event_name":"PreToolUse","tool_name":"` + tc.tool + `","tool_input":{},"tool_use_id":"t1"}`
			env := failureEnv(t)
			calls := 0
			out := Runner{
				ClientName: "claude",
				EventName:  "PreToolUse",
				Stdin:      strings.NewReader(payload),
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
		if class, unknown := classifyTool(ClientCodex, name, false); class != ClassOther || !unknown {
			t.Errorf("%s: %s, unknown %v", name, class, unknown)
		}
	}
}
