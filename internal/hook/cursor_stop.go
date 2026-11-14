package hook

import (
	"context"
	"errors"
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
	if ev.LoopCount > 0 || !r.cursorWakeEnabled() || !r.cursorInteractive() {
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

// readCursorArgs is set per operating system. cursorCommandLine is what the hook calls,
// so a test can substitute a command line without a Cursor process.
var readCursorArgs func(pid int, startMS int64) ([]string, error)

// cursorCommandLine reads the Cursor process's arguments. Tests replace it.
// An error means the mode cannot be told, and the hook does not follow up (ADR 031).
var cursorCommandLine = dispatchCursorArgs

func dispatchCursorArgs(pid int, startMS int64) ([]string, error) {
	if readCursorArgs == nil {
		return nil, errors.New("arguments unreadable")
	}
	return readCursorArgs(pid, startMS)
}

// cursorInteractive reports whether this stop belongs to an interactive cursor-agent.
// Print mode, a subcommand and arguments Iris cannot read get no follow-up.
func (r Runner) cursorInteractive() bool {
	anc := r.ancestor()
	args, err := cursorCommandLine(anc.PID, anc.StartMS)
	if err != nil {
		return false
	}
	return cursorInteractiveArgs(args)
}

// cursorInteractiveArgs applies cursor-agent's flag grammar. -p and --print are print mode.
// A positional prompt without one of those is still the interactive TUI.
func cursorInteractiveArgs(args []string) bool {
	if len(args) == 0 {
		return false
	}
	for i := 1; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			break
		}
		if a == "-p" || a == "--print" || strings.HasPrefix(a, "--print=") {
			return false
		}
		if strings.HasPrefix(a, "--") {
			name, _, attached := strings.Cut(a[2:], "=")
			switch {
			case cursorBools[name]:
			case cursorValues[name]:
				if !attached && !cursorOptional[name] {
					if i++; i >= len(args) {
						return false
					}
				}
			case cursorOptional[name]:
				if !attached && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
					i++
				}
			default:
				return false
			}
			continue
		}
		if len(a) > 1 && a[0] == '-' {
			if !cursorShorts(a[1:], args, &i) {
				return false
			}
			continue
		}
		if cursorCommands[a] && a != "agent" && a != "resume" {
			return false
		}
	}
	return true
}

func cursorShorts(group string, args []string, i *int) bool {
	for j := 0; j < len(group); j++ {
		switch group[j] {
		case 'p':
			return false
		case 'f', 'h', 'v':
		case 'H', 'e':
			if j+1 < len(group) {
				return true
			}
			if *i++; *i >= len(args) {
				return false
			}
			return true
		case 'w':
			if j+1 == len(group) && *i+1 < len(args) && !strings.HasPrefix(args[*i+1], "-") {
				*i++
			}
		default:
			return false
		}
	}
	return true
}

var (
	cursorBools    = setCursor("stream-partial-output", "plan", "force", "yolo", "auto-review", "approve-mcps", "trust", "list-models", "skip-worktree-setup", "help", "version", "continue")
	cursorValues   = setCursor("api-key", "header", "endpoint", "output-format", "mode", "model", "sandbox", "workspace", "add-dir", "plugin-dir", "worktree-base")
	cursorOptional = setCursor("resume", "worktree")
	cursorCommands = setCursor("persist", "install-shell-integration", "uninstall-shell-integration", "login", "logout", "mcp", "plugin", "worker", "status", "whoami", "models", "team", "bedrock", "about", "update", "create-chat", "generate-rule", "rule", "agent", "ls", "resume", "help")
)

func setCursor(names ...string) map[string]bool {
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[n] = true
	}
	return m
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
