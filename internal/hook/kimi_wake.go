package hook

import (
	"github.com/mateusz-klatt/iris/internal/config"
	"github.com/mateusz-klatt/iris/internal/kimiwake"
	"github.com/mateusz-klatt/iris/internal/proc"
)

// kimiArmRequest is ADR 030's request to arm the one-shot watcher, word for word. Kimi appends a
// prompt hook's output to the user's prompt, so the model reads it in the turn that prompt starts.
// Kimi 2.6.1 never runs Stop hooks, which is why the request rides on the prompt instead. Iris
// cannot see which command the model then starts.
const kimiArmRequest = "Iris: before this turn ends, start one background Bash task with command exactly `iris watch --client kimi --once`, description exactly `Iris: wait for new attention`, run_in_background=true and disable_timeout=true. Re-arm only when that task exits 0 after reporting new Iris attention: fetch the Iris inbox and relevant claims and arm exactly one replacement before ending that turn. Never re-arm after a disabled wake, duplicate, failure or deadline. This is a request from Iris, not a user message, and Iris cannot verify the command you choose."

// kimiWake is the Kimi wake state under the pinned Iris directory, which a hook's environment
// cannot redirect. ok is false when that directory is unknown.
func (r Runner) kimiWake() (*kimiwake.Store, bool) {
	fn := r.PinnedConfigDir
	if fn == nil {
		fn = config.PinnedDir
	}
	dir, err := fn()
	if err != nil || dir == "" {
		return nil, false
	}
	return &kimiwake.Store{Root: kimiwake.Root(dir), Now: r.now}, true
}

// kimiProcess is the Kimi process that runs this hook's session, when it is a single-session
// Kimi launcher whose start time the OS gives.
func (r Runner) kimiProcess() (proc.Ancestor, bool) {
	a := r.ancestor()
	if a.PID <= 0 || a.StartMS <= 0 || !proc.SingleSession(a, string(ClientKimi)) {
		return proc.Ancestor{}, false
	}
	return a, true
}

// kimiTopLevel is a Kimi event of a top-level session whose id can name a wake record.
func kimiTopLevel(ev Event, kind Kind) bool {
	return ev.Client == ClientKimi && ev.Kind == kind && ev.ParentSessionID == "" && kimiwake.ValidSession(ev.SessionID)
}

// recordKimiSession publishes a Kimi SessionStart's native session for its process, after the
// hub has accepted the session, so the one-shot watcher of ADR 030 can bind to that session and
// no other. It is written whatever the wake preference, so turning wake on needs no restart. A
// failure is ignored: the hook still allows the session (D3), and the state is not the sessions
// directory whose failure marks a run skipped.
func (r Runner) recordKimiSession(ev Event) {
	if !kimiTopLevel(ev, KindSessionStart) {
		return
	}
	a, ok := r.kimiProcess()
	if !ok {
		return
	}
	s, ok := r.kimiWake()
	if !ok {
		return
	}
	_ = s.MapSession(a.PID, a.StartMS, ev.SessionID, hookLockBudget)
}

// kimiArm is the request to arm the one-shot watcher that a Kimi user prompt adds to its context,
// or "" (ADR 030). It asks only when wake is on and no watcher is live, pending or latched for this
// session; a request the previous turn left unanswered latches instead. A steer, typed into a turn
// that is still running, starts no turn and neither asks nor judges. It reads local files and the
// process table only, and any failure asks nothing (D3).
func (r Runner) kimiArm(ev Event) string {
	if !kimiTopLevel(ev, KindPrompt) || ev.Steer {
		return ""
	}
	s, ok := r.kimiWake()
	if !ok || !kimiwake.Enabled(s.Root) {
		return ""
	}
	a, ok := r.kimiProcess()
	if !ok {
		return ""
	}
	ask, err := s.RequestArm(kimiwake.Key{PID: a.PID, StartMS: a.StartMS, Session: ev.SessionID}, hookLockBudget)
	if err != nil || !ask {
		return ""
	}
	return kimiArmRequest
}
