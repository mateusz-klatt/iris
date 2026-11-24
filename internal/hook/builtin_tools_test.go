package hook

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// The built-in tools iris hook logged as unknown on a real host are known now. Codex's names were
// logged on home-wsl on 2026-11-08 and are exactly as logged. Codex joins a tool's namespace and
// name: its rollouts record {"namespace":"collaboration","name":"spawn_agent"}, which the hook sees
// as "collaborationspawn_agent". Claude Code's names are backed by captures in testdata/claude from
// 2.1.319 on Windows and 2.1.331 on Linux/WSL, which the golden test decodes. No Codex payload
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
		"CronCreate":       ClassOther,
		"CronDelete":       ClassOther,
		"TaskStop":         ClassOther,
		"ScheduleWakeup":   ClassOther,
		"Monitor":          ClassShell,
	} {
		class, unknown := classifyTool(name, false)
		if class != want || unknown {
			t.Errorf("%s: %s, unknown %v; want %s and known", name, class, unknown, want)
		}
	}
}

// Every captured non-writing built-in allows the event, touches no hub and adds nothing to the
// observation log doctor reads. The golden expectations also require no extracted paths, including
// SendUserFile's files array.
func TestClaudeBuiltinsAllowWithoutObservation(t *testing.T) {
	for _, tool := range []string{"agent", "pushnotification", "senduserfile", "croncreate", "crondelete"} {
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
