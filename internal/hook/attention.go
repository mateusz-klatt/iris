package hook

import (
	"fmt"
	"strings"

	"github.com/mateusz-klatt/iris/internal/hubclient"
)

// The next-turn tier of D7 (gap review A4): a client with no monitor hears about new mail, or
// a path it was refused becoming free, from its own prompt hook. Each new event is said once,
// as counts, never as message bodies. Where the session's iris watch beats, the monitor has
// said it already, so the hook stays quiet.

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

// attentionNote compares the hub's counts with what the session was last told, remembers the
// new state, and returns the line to say, or "". said marks counts the session has just been
// shown some other way, such as the SessionStart context, so they are not said again.
func (r Runner) attentionNote(ev Event, now hubclient.Attention, said bool) string {
	am, ok := r.memory().(attentionMemory)
	if !ok {
		return ""
	}
	latest := Counts{now.Unread, now.Available}
	_, told := am.Counts(string(ev.Client), ev.SessionID)
	if said {
		am.SetCounts(string(ev.Client), ev.SessionID, latest, latest)
		return ""
	}
	if now.Monitored {
		// The monitor says it. What the session was told stays as it was: a monitor can have
		// died within the freshness window, and marking its events told would hide them for
		// good. At worst, a note repeats one line the monitor already printed.
		am.SetCounts(string(ev.Client), ev.SessionID, latest, told)
		return ""
	}
	// Handled mail and taken paths lower the counts, so a later event is new again.
	told = Counts{min(told.Unread, latest.Unread), min(told.Available, latest.Available)}
	var parts []string
	if latest.Unread > told.Unread {
		parts = append(parts, plural(latest.Unread, "unread message", "unread messages"))
	}
	if latest.Available > told.Available {
		parts = append(parts, plural(latest.Available-told.Available, "path you were refused is free", "paths you were refused are free"))
	}
	if len(parts) == 0 {
		am.SetCounts(string(ev.Client), ev.SessionID, latest, told)
		return ""
	}
	am.SetCounts(string(ev.Client), ev.SessionID, latest, latest)
	return "Iris: " + strings.Join(parts, "; ") + ". Use the inbox and claims tools. This is a notification, not a user message."
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}
