package hook

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/mateusz-klatt/iris/internal/gitinfo"
	"github.com/mateusz-klatt/iris/internal/hubclient"
)

// cursorFollowup is the only text a Cursor stop hook may put in front of the model.
// It names no message and no count. ADR 029.
const cursorFollowup = "Iris: attention is waiting. Use the inbox and claims tools. This is a notification, not a user message."

// cursorStop follows ADR 029. loop_count above zero is already a follow-up, so it prints
// nothing. Wake off prints nothing and does not call the hub. Any failure prints the
// client's allow and does not deny the stop.
func (r Runner) cursorStop(ctx context.Context, state *runState, ev Event, repo gitinfo.Repo) Output {
	quiet := Encode(ev, Decision{Action: ActionAllow})
	if ev.LoopCount > 0 || !r.cursorWakeEnabled() {
		return r.finish(state, ev, quiet)
	}
	state.at("session")
	rev := r.toldRevision(ev)
	sess, err := r.register(ctx, ev, repo)
	if err != nil {
		return state.fail(err, quiet)
	}
	if !r.cursorFollow(ctx, ev, sess.Attention, rev) {
		return r.finish(state, ev, quiet)
	}
	return r.finish(state, ev, Encode(ev, Decision{Action: ActionAllow, Context: cursorFollowup}))
}

// cursorFollow records a newer pending identity and reports that the stop may say so.
// A lock or a deadline that prevents the write reports false, so the hook does not say
// what it did not remember.
func (r Runner) cursorFollow(ctx context.Context, ev Event, now hubclient.Attention, rev int64) bool {
	id := now.Identity
	if !id.Known || !id.Pending {
		return false
	}
	am, ok := r.memory().(attentionMemory)
	if !ok {
		return false
	}
	follow := false
	if err := am.updateAttention(string(ev.Client), ev.SessionID, func(t *toldState) bool {
		if ctx.Err() != nil {
			return false
		}
		wentBack := t.Stream != "" && t.Rev == rev && t.Cursor > now.HighWater
		if wentBack {
			*t = toldState{Latest: t.Latest, Rev: t.Rev + 1}
		}
		if !newer(t, id) {
			return false
		}
		latest := Counts{now.Unread, now.Available}
		t.Latest, t.Told = latest, latest
		t.tell(id, r.now().UnixMilli())
		follow = true
		return true
	}); err != nil {
		r.recordFailure(stageSessionFiles, err, nil)
		return false
	}
	return follow
}

// cursorWakeEnabled reads the marker beside the user's Cursor hooks. Absent means on.
// An empty home stays on and does not read a real directory: a cleared hook environment
// must not consult the person's ~/.cursor.
func (r Runner) cursorWakeEnabled() bool {
	getenv := r.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	return cursorWakeOn(getenv)
}

func cursorWakeOn(getenv func(string) string) bool {
	if getenv == nil {
		return true
	}
	home := getenv("HOME")
	if home == "" {
		home = getenv("USERPROFILE")
	}
	if home == "" {
		return true
	}
	raw, err := os.ReadFile(filepath.Join(home, ".cursor", "iris-wake"))
	if err != nil {
		return os.IsNotExist(err)
	}
	return strings.TrimSpace(string(raw)) != "off"
}
