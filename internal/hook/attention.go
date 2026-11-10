package hook

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/mateusz-klatt/iris/internal/hubclient"
)

// The next-turn tier of D7 (gap review A4): a client with no monitor hears about new mail, or
// a path it was refused becoming free, from its own hooks, as counts, never as message bodies.
// Where the session's iris watch beats, the monitor has said it already, so the hook stays
// quiet. A hub that names attention gives each pending event an identity, and a note is said
// when the identity is newer than the one the session was told (ADR 017, decision 4), and once
// more ten minutes later if it is still the same (decision 5).

// carriesPromptContext is whether a client's prompt hook can put text in front of the model,
// as section 3 of the architecture records it. Grok's prompt hook carries none, and Cursor's
// only in the IDE, so they make no call for it.
func carriesPromptContext(c Client) bool {
	switch c {
	case ClientClaude, ClientCodex, ClientCopilot, ClientKimi, ClientAntigravity:
		return true
	default:
		return false
	}
}

// carriesToolContext is whether a client shows an allowed pre-edit hook's context to the model.
// Measured on macOS with a marker in the hook's output: Claude Code and Codex 0.167.0 did, for
// PreToolUse and PostToolUse alike. Kimi 2.5.2 did not, and Antigravity's pre-tool answer has no
// context field (its PreInvocation hook, which runs before every model call, already says it).
func carriesToolContext(c Client) bool { return c == ClientClaude || c == ClientCodex }

func promptEvent(ev Event) bool {
	return ev.Kind == KindPrompt || (ev.Kind == KindPreInvocation && ev.Client == ClientAntigravity)
}

// secondNoteAfter is decision 5's wait: an identity still the newest pending this long after the
// session was told it is said once more, by a prompt hook.
const secondNoteAfter = 10 * time.Minute

// consumeWake is the first thing a Codex prompt hook does with its session, before repository
// discovery and any network call, so a hub outage or a failed registration cannot leave an
// executed wake unconsumed (ADR 017, decision 3).
func (r Runner) consumeWake(ev Event) {
	wm, ok := r.memory().(wakeMemory)
	if !ok {
		return
	}
	if err := wm.consumeWake(string(ev.Client), ev.SessionID, ev.Prompt); err != nil {
		r.recordFailure(stageSessionFiles, err, nil)
	}
}

// toldRevision is the told state's revision before the hook asks the hub, for attentionNote. One
// the lock kept from being read is fromReplay, so that answer is no evidence of a restore, and
// the failure goes to the log doctor reads.
func (r Runner) toldRevision(ev Event) int64 {
	am, ok := r.memory().(attentionMemory)
	if !ok {
		return 0
	}
	rev, err := am.toldRevision(string(ev.Client), ev.SessionID)
	if err != nil {
		r.recordFailure(stageSessionFiles, err, nil)
	}
	return rev
}

// fromReplay is the revision a touch answer is decided with. A touch answer can be the replay of
// an earlier call's, attention and all, since the hub keeps it so that a retried call is
// idempotent. So it is never evidence that the hub went back: no told state has this revision.
const fromReplay int64 = -1

// attentionNote decides what the session is told of its attention, and remembers it, under the
// sessions directory's lock. said marks attention the session has just been shown some other
// way, such as the SessionStart context. rev is toldRevision from before the hub answered, or
// fromReplay for a touch answer. It returns the line to say, or "". When the lock or the write
// fails it says nothing, so what was not remembered is not said either, and the failure goes to
// the log doctor reads (D3). If ctx is already done when it is checked under the lock, no note is
// decided and the told state is not written: the caller would not show the note.
func (r Runner) attentionNote(ctx context.Context, ev Event, now hubclient.Attention, said bool, rev int64) string {
	am, ok := r.memory().(attentionMemory)
	if !ok {
		return ""
	}
	var note string
	if err := am.updateAttention(string(ev.Client), ev.SessionID, func(t *toldState) bool {
		if ctx.Err() != nil {
			return false
		}
		note = decide(t, now, said, promptEvent(ev), rev, r.now().UnixMilli())
		return true
	}); err != nil {
		r.recordFailure(stageSessionFiles, err, nil)
		return ""
	}
	return note
}

// decide applies the rules to the told state t and the hub's answer now, at the time at.
func decide(t *toldState, now hubclient.Attention, said, prompt bool, rev, at int64) string {
	latest := Counts{now.Unread, now.Available}
	if !now.Identity.Known {
		return countsNote(t, latest, said, now.Monitored)
	}
	id := now.Identity
	// A told cursor above the hub's high-water mark means the hub went back, as a restore does
	// (ADR 017, "A restore keeps the stream"), and the told state is void. Only an answer that no
	// other hook's change of the told identity preceded shows that: a slower hook's older answer
	// is below the newer telling as well.
	wentBack := t.Stream != "" && t.Rev == rev && t.Cursor > now.HighWater
	if wentBack {
		*t = toldState{Latest: t.Latest, Rev: t.Rev + 1}
	}
	if said {
		t.Latest, t.Told = latest, latest
		switch {
		case id.Pending && newer(t, id):
			t.tell(id, at)
		case id.Pending && id.Cursor == t.Cursor:
			// The context restates what was told before, as a resumed session's does: that is
			// its second telling, so no prompt hook repeats it (decision 5).
			t.Twice = true
		}
		return ""
	}
	if now.Monitored {
		// The monitor says it. What the session was told stays as it was: a monitor can have
		// died within the freshness window, and marking its events told would hide them for
		// good. At worst, a note repeats one line the monitor already printed.
		t.Latest = latest
		return ""
	}
	if !id.Pending {
		t.Latest, t.Told = latest, lower(t.Told, latest)
		return ""
	}
	if t.Stream == "" && !wentBack {
		// No identity told yet: a file from before ADR 017, or a session with nothing pending
		// since it began. The counts decide this once, as they did before.
		note := countsNote(t, latest, false, false)
		t.tell(id, at)
		return note
	}
	switch {
	case newer(t, id):
		t.tell(id, at)
	case prompt && !t.Twice && id.Cursor == t.Cursor && at-t.At >= secondNoteAfter.Milliseconds():
		t.Twice = true
	default:
		// Told already. Within a stream the told cursor goes down only after a restore the hub
		// shows, so a slower hook's older answer cannot reopen what was said.
		t.Latest, t.Told = latest, lower(t.Told, latest)
		return ""
	}
	t.Latest, t.Told = latest, latest
	return noteLine(totals(latest))
}

// newer is decision 4's test: another stream, or a larger cursor in the same one.
func newer(t *toldState, id hubclient.Identity) bool {
	return id.Stream != t.Stream || id.Cursor > t.Cursor
}

// tell records id as told at at, not yet said a second time.
func (t *toldState) tell(id hubclient.Identity, at int64) {
	t.Stream, t.Cursor, t.At, t.Twice = id.Stream, id.Cursor, at, false
	t.Rev++
}

// countsNote is the rule from before ADR 017, for a hub that names no attention. It compares the
// hub's counts with the counts last told, and lowers those when the hub's counts fall. A message
// handled and another arriving leave the count as it was, which is why a hub now names attention.
func countsNote(t *toldState, latest Counts, said, monitored bool) string {
	if said {
		t.Latest, t.Told = latest, latest
		return ""
	}
	if monitored {
		t.Latest = latest
		return ""
	}
	told := lower(t.Told, latest)
	var parts []string
	if latest.Unread > told.Unread {
		parts = append(parts, plural(latest.Unread, "unread message", "unread messages"))
	}
	if latest.Available > told.Available {
		parts = append(parts, plural(latest.Available-told.Available, "path you were refused is free", "paths you were refused are free"))
	}
	if len(parts) == 0 {
		t.Latest, t.Told = latest, told
		return ""
	}
	t.Latest, t.Told = latest, latest
	return noteLine(parts)
}

func lower(told, latest Counts) Counts {
	return Counts{min(told.Unread, latest.Unread), min(told.Available, latest.Available)}
}

// totals is what an identity's note says: everything pending, since the identity, not the
// counts, said that something is new.
func totals(c Counts) []string {
	var parts []string
	if c.Unread > 0 {
		parts = append(parts, plural(c.Unread, "unread message", "unread messages"))
	}
	if c.Available > 0 {
		parts = append(parts, plural(c.Available, "path you were refused is free", "paths you were refused are free"))
	}
	return parts
}

func noteLine(parts []string) string {
	return "Iris: " + strings.Join(parts, "; ") + ". Use the inbox and claims tools. This is a notification, not a user message."
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}
