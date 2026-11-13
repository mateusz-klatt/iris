package hook

import (
	"github.com/mateusz-klatt/iris/internal/config"
	"github.com/mateusz-klatt/iris/internal/kimiwake"
	"github.com/mateusz-klatt/iris/internal/proc"
)

// kimiWatchReason is the Stop continuation of ADR 030, word for word. Kimi shows a blocking Stop
// hook's stderr to the model, which then starts the one-shot watcher as a managed background task.
// Kimi 2.6.1 gives Stop no task list, so Iris cannot see which command the model chose.
const kimiWatchReason = "Start one background Bash task with command exactly `iris watch --client kimi --once`, description exactly `Iris: wait for new attention`, run_in_background=true and disable_timeout=true. Then stop. Re-arm only when this task exits 0 after reporting new Iris attention: fetch the Iris inbox and relevant claims and arm exactly one replacement before ending. Never re-arm after a disabled wake, duplicate, failure or deadline. Iris asks for this but cannot verify the command you chose."

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

// kimiWakeAsk asks the model to start the one-shot watcher at the end of a turn, when wake is on
// and no watcher is live, pending or latched for this session (ADR 030). It reads local files and
// the process table only and calls no hub, and any failure lets the turn end (D3). ok is false
// when the hook continues with its ordinary allow.
func (r Runner) kimiWakeAsk(ev Event) (Output, bool) {
	if !kimiTopLevel(ev, KindStop) {
		return Output{}, false
	}
	s, ok := r.kimiWake()
	if !ok || !kimiwake.Enabled(s.Root) {
		return Output{}, false
	}
	a, ok := r.kimiProcess()
	if !ok {
		return Output{}, false
	}
	ask, err := s.RequestArm(kimiwake.Key{PID: a.PID, StartMS: a.StartMS, Session: ev.SessionID}, hookLockBudget)
	if err != nil || !ask {
		return Output{}, false
	}
	return Output{Stderr: []byte(kimiWatchReason + "\n"), Exit: 2}, true
}
