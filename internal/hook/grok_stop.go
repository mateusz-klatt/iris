package hook

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// grokWatchReason is the Stop continuation. It asks for one long-running monitor. Stop's
// backgroundTasks entry carries the description "[monitor] <description>" and not the command,
// so the ask cannot check which command is running.
const grokWatchReason = "Start the monitor tool once with persistent true and command exactly: iris watch --client grok. Then stop. Iris sees a running monitor, not its command, so this asks you; it does not check the command."

// GrokStop is the part of a Grok Stop payload that decides whether to ask for a monitor.
// Other clients leave it nil. A malformed task list is not treated as an empty one.
type GrokStop struct {
	Reason     string
	HookActive bool
	HasMonitor bool
	Malformed  bool
}

// arm reports whether this Stop is allowed to ask. Only end_turn asks, and only once per
// turn: stopHookActive is the continuation of an ask already made. shutdown and
// channel_closed are the session ending, and a running monitor of any command is enough,
// because the payload does not include the command.
func (s GrokStop) arm() bool {
	if s.Malformed || s.HookActive || s.HasMonitor || s.Reason != "end_turn" {
		return false
	}
	return true
}

func applyGrokStop(ev *Event, p commonPayload) {
	if ev.Client != ClientGrok || ev.Kind != KindStop {
		return
	}
	s := GrokStop{Reason: p.Reason}
	if p.StopHookActive != nil && *p.StopHookActive {
		s.HookActive = true
	}
	if p.StopHookActiveSnake != nil && *p.StopHookActiveSnake {
		s.HookActive = true
	}
	has, bad := grokTasks(p.BackgroundTasks)
	has2, bad2 := grokTasks(p.BackgroundTasksSnake)
	s.HasMonitor = has || has2
	s.Malformed = bad || bad2
	ev.GrokStop = &s
}

// grokTasks reports a task whose type is exactly "monitor". A value that is present and not
// an array is malformed, so the hook does not ask. Missing and null are an empty list.
func grokTasks(raw json.RawMessage) (hasMonitor, malformed bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return false, false
	}
	var tasks []struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(raw, &tasks) != nil {
		return false, true
	}
	for _, t := range tasks {
		if t.Type == "monitor" {
			return true, false
		}
	}
	return false, false
}

func grokBlock(reason string) Output {
	return Output{Stdout: jsonLine(grokDec{Decision: "block", Reason: reason})}
}

// grokMonitorAsk blocks end_turn until a monitor exists, when wake is enabled. ok is false
// when the hook should continue with its ordinary allow. A wake file that cannot be read
// does not block (D3): Stop has a 1.5s budget and does not call the hub to decide. Only an
// interactive session is asked (ADR 031); for any other, note gets the reason.
func (r Runner) grokMonitorAsk(ev Event, note func(string)) (Output, bool) {
	if ev.GrokStop == nil || !ev.GrokStop.arm() {
		return Output{}, false
	}
	if !r.grokWakeEnabled() {
		return Output{}, false
	}
	anc := r.ancestor()
	if anc.PID <= 0 || anc.StartMS <= 0 || !isGrokProcess(anc.Name) {
		note(wakeNotAsked + "no grok process")
		return Output{}, false
	}
	if ok, why := interactive(anc, grokMode); !ok {
		note(wakeNotAsked + why)
		return Output{}, false
	}
	return grokBlock(grokWatchReason), true
}

// grokWakeEnabled reads the local marker. Absent means on, including an integration installed
// before the marker existed. The exact trimmed text "off" means off. When GROK_HOME, HOME and
// USERPROFILE are all empty the marker is unread and wake stays on: falling through to the
// process home would let a test, or a hook whose environment was cleared, read a person's
// real ~/.grok.
func (r Runner) grokWakeEnabled() bool {
	getenv := r.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	return grokWakeOn(getenv)
}

func grokWakeOn(getenv func(string) string) bool {
	dir := getenv("GROK_HOME")
	if dir == "" {
		home := getenv("HOME")
		if home == "" {
			home = getenv("USERPROFILE")
		}
		if home == "" {
			return true
		}
		dir = filepath.Join(home, ".grok")
	}
	raw, err := os.ReadFile(filepath.Join(dir, "iris-wake"))
	if err != nil {
		return os.IsNotExist(err)
	}
	return strings.TrimSpace(string(raw)) != "off"
}
