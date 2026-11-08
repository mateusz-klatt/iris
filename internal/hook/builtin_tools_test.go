package hook

import (
	"encoding/json"
	"testing"
)

// The built-in tools iris hook logged as unknown on a real host (hook-observed.log on home-wsl,
// 2026-11-08: Codex 0.167.0 and Claude Code 2.1.319) are known now. The names are exactly as
// logged. Codex joins a tool's namespace and name: its rollouts record
// {"namespace":"collaboration","name":"spawn_agent"}, which the hook sees as
// "collaborationspawn_agent". No payload capture backs these names yet; captures go in
// testdata once a person has trusted a capture hook.
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
		"TaskStop":       ClassOther,
		"ScheduleWakeup": ClassOther,
		"Monitor":        ClassShell,
	} {
		class, unknown := classifyTool(name, false)
		if class != want || unknown {
			t.Errorf("%s: %s, unknown %v; want %s and known", name, class, unknown, want)
		}
	}
}

// Monitor runs a shell command in the background, so its command is read and reconciled as a
// shell's, never taken as paths.
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
