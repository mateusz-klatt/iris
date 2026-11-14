package hook

import (
	"strings"

	"github.com/mateusz-klatt/iris/internal/proc"
)

// Iris asks a model to arm wake only in an interactive session (ADR 031). A print-mode run, a
// subcommand such as a protocol server, or arguments Iris cannot read get no request, because an
// arming request in a headless run adds a step to its answer. Neither client marks the mode in a
// hook payload or the environment (measured on agy 1.5.8 and Grok 1.0.77), so it is read from
// the verified client process: its arguments, parsed by that client's grammar, and whether it
// reads from a terminal.

// clientMode is what a client's arguments say about its session.
type clientMode int

const (
	// modeUnknown is an argument Iris does not know, so the mode cannot be told.
	modeUnknown clientMode = iota
	// modePrint is a run that prints one answer and exits.
	modePrint
	// modeCommand is a subcommand: a protocol server, a management command, a headless agent.
	modeCommand
	// modeInteractive is an interactive session, whatever its input.
	modeInteractive
	// modeInput is a session that is interactive only when it reads from a terminal: given no
	// prompt flag, agy reads its prompt from standard input when that is not a terminal.
	modeInput
)

// wakeNotAsked prefixes the observed-log entry that says why a session got no arming request.
// Each reason is a fixed text, with nothing from a payload or the arguments in it.
const wakeNotAsked = "wake not asked: "

func set(names ...string) map[string]bool {
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[n] = true
	}
	return m
}

// agy's root flags (agy 1.5.8, measured by its parse errors). Its parser is Go's flag package: one
// dash or two, a value attached with = or as the next argument even when that starts with a dash.
var (
	agyBools  = set("c", "continue", "dangerously-skip-permissions", "disable-slash-commands", "new-project", "remote-agent", "remote-control", "sandbox")
	agyValues = set("add-dir", "agent", "conversation", "effort", "i", "input-format", "json-schema", "log-file", "mode", "model", "output-format", "p", "print", "print-timeout", "project", "prompt", "prompt-interactive", "settings")
	agyPrint  = set("p", "print", "prompt")
	// -i needs a terminal: without one agy exits before any hook runs, so a running agy given it
	// is interactive.
	agyPrompted = set("i", "prompt-interactive")
)

// agyMode reads agy's arguments. agy refuses a positional prompt, so its only positional
// arguments are subcommands such as acp.
func agyMode(args []string) clientMode {
	mode := modeInput
	for i := 1; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			if i+1 < len(args) {
				return modeCommand
			}
			break
		}
		name, flag := strings.CutPrefix(a, "-")
		if !flag || name == "" {
			return modeCommand
		}
		name = strings.TrimPrefix(name, "-")
		if name == "" || name[0] == '-' || name[0] == '=' {
			return modeUnknown
		}
		name, _, attached := strings.Cut(name, "=")
		switch {
		case agyPrint[name]:
			return modePrint
		case agyBools[name]:
		case agyValues[name]:
			if agyPrompted[name] {
				mode = modeInteractive
			}
			if !attached {
				if i++; i >= len(args) {
					return modeUnknown
				}
			}
		default:
			return modeUnknown
		}
	}
	return mode
}

// Grok's options (Grok 1.0.77, its --help). Its parser is clap: long options with two dashes,
// short ones with one that may be grouped, a value attached or as the next argument, and an
// optional value taken only from a next argument that is not an option.
var (
	grokBools    = set("always-approve", "continue", "debug", "disable-web-search", "fork-session", "fullscreen", "help", "include-partial-messages", "minimal", "no-alt-screen", "no-plan", "no-subagents", "oauth", "restore-code", "verbatim", "version")
	grokValues   = set("add-dir", "agent", "agents", "allow", "allowed-tools", "allowedTools", "cwd", "debug-file", "deny", "disallowed-tools", "disallowedTools", "effort", "json-schema", "leader-socket", "max-turns", "model", "output-format", "permission-mode", "prompt-file", "prompt-json", "reasoning-effort", "ref", "rules", "sandbox", "session-id", "single", "system-prompt", "system-prompt-override", "tools", "worktree-ref")
	grokOptional = set("resume", "worktree")
	grokPrint    = set("single", "prompt-file", "prompt-json")
	// grokCommands are Grok's subcommands. Usage is `[PROMPT] [COMMAND]`: a first positional
	// argument that is none of them is the interactive session's prompt, and one after it is a
	// subcommand.
	grokCommands = set("agent", "clone", "completions", "dashboard", "disk-usage", "doctor", "du", "export", "help", "inspect", "leader", "login", "logout", "mcp", "memory", "models", "plugin", "sessions", "setup", "trace", "update", "usage", "v", "version", "worker", "worktree", "wrap")
)

// grokMode reads Grok's arguments.
func grokMode(args []string) clientMode {
	prompt := false
	for i := 1; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			return modeInput
		case strings.HasPrefix(a, "--"):
			name, _, attached := strings.Cut(a[2:], "=")
			switch {
			case grokPrint[name]:
				return modePrint
			case grokBools[name]:
			case grokValues[name]:
				if !attached {
					if i++; i >= len(args) {
						return modeUnknown
					}
				}
			case grokOptional[name]:
				if !attached && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
					i++
				}
			default:
				return modeUnknown
			}
		case len(a) > 1 && a[0] == '-':
			mode, next := grokShorts(a[1:], args, i)
			if mode != modeInput {
				return mode
			}
			i = next
		case grokCommands[a]:
			return modeCommand
		case prompt:
			// After the prompt, clap reads a positional argument as the subcommand.
			return modeUnknown
		default:
			prompt = true
		}
	}
	return modeInput
}

// grokShorts reads one group of short options at args[i]. An option that takes a value takes the
// rest of the group, or else the next argument. It returns the index of the last argument used.
func grokShorts(group string, args []string, i int) (clientMode, int) {
	for j := 0; j < len(group); j++ {
		switch group[j] {
		case 'p':
			return modePrint, i
		case 'c', 'h', 'v':
			continue
		case 'm', 's':
			if j+1 == len(group) {
				if i++; i >= len(args) {
					return modeUnknown, i
				}
			}
			return modeInput, i
		case 'r', 'w':
			if j+1 == len(group) && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
			}
			return modeInput, i
		default:
			return modeUnknown, i
		}
	}
	return modeInput, i
}

// clientArgs and clientTerminal read a verified client process's arguments and whether it reads
// from a terminal, from the process table. Tests replace them.
var (
	clientArgs     = proc.Args
	clientTerminal = proc.ReadsTerminal
)

// Kimi's options (Kimi 2.6.1, its --help and parse errors). Its parser is commander: long options
// with two dashes, short ones with one that may be grouped, a value attached or as the next
// argument even when that starts with a dash, booleans that refuse a value, and an optional value
// taken only from a next argument that is not an option. Its positional arguments are subcommands.
var (
	kimiBools    = set("auto", "continue", "help", "plan", "version", "yolo")
	kimiValues   = set("add-dir", "agent", "agent-file", "model", "output-format", "prompt", "skills-dir")
	kimiOptional = set("session")
	kimiCommands = set("acp", "app", "doctor", "export", "fork", "help", "import-sessions", "install-desktop", "login", "migrate", "provider", "rc", "remote", "server", "session", "update", "upgrade", "vis", "web")
)

// kimiRewritten is the argv a Kimi 2.6.1 process leaves after it names itself: "kimi-code" and an
// empty string for every argument it was given, so its flags cannot be read. That exact form, and
// no other, leaves the mode to its input; a prompt piped into a bare kimi opens its TUI rather
// than running headless.
func kimiRewritten(args []string) bool {
	if len(args) < 2 || args[0] != "kimi-code" {
		return false
	}
	for _, a := range args[1:] {
		if a != "" {
			return false
		}
	}
	return true
}

// kimiMode reads Kimi's arguments. Kimi has no flag that makes a session interactive, so a session
// with no print flag or subcommand is interactive only when it reads a terminal.
func kimiMode(args []string) clientMode {
	if kimiRewritten(args) {
		return modeInput
	}
	for i := 1; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			if i+1 < len(args) {
				return modeCommand
			}
			return modeInput
		case strings.HasPrefix(a, "--"):
			name, _, attached := strings.Cut(a[2:], "=")
			switch {
			case name == "prompt":
				return modePrint
			case kimiBools[name] && !attached:
			case kimiValues[name]:
				if !attached {
					if i++; i >= len(args) {
						return modeUnknown
					}
				}
			case kimiOptional[name]:
				if !attached && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
					i++
				}
			default:
				return modeUnknown
			}
		case len(a) > 1 && a[0] == '-':
			mode, next := kimiShorts(a[1:], args, i)
			if mode != modeInput {
				return mode
			}
			i = next
		case kimiCommands[a]:
			return modeCommand
		default:
			return modeUnknown
		}
	}
	return modeInput
}

// kimiShorts reads one group of short options at args[i], as grokShorts does for Grok's.
func kimiShorts(group string, args []string, i int) (clientMode, int) {
	for j := 0; j < len(group); j++ {
		switch group[j] {
		case 'p':
			return modePrint, i
		case 'c', 'h', 'V', 'y':
			continue
		case 'm':
			if j+1 == len(group) {
				if i++; i >= len(args) {
					return modeUnknown, i
				}
			}
			return modeInput, i
		case 'S':
			if j+1 == len(group) && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
			}
			return modeInput, i
		default:
			return modeUnknown, i
		}
	}
	return modeInput, i
}

// interactive reports whether the client process anc runs an interactive session, the only kind
// Iris asks a model to arm wake in. Otherwise why says why not, for the observed log. A process
// whose arguments or input cannot be read is not interactive: a request that should not have
// been made changes a headless answer, while a request not made only delays wake.
func interactive(anc proc.Ancestor, mode func([]string) clientMode) (ok bool, why string) {
	args, err := clientArgs(anc.PID, anc.StartMS)
	if err != nil || len(args) == 0 {
		return false, "arguments unreadable"
	}
	switch mode(args) {
	case modePrint:
		return false, "print mode"
	case modeCommand:
		return false, "subcommand"
	case modeUnknown:
		return false, "unknown argument"
	case modeInteractive:
		return true, ""
	}
	term, err := clientTerminal(anc.PID, anc.StartMS)
	if err != nil {
		return false, "input unreadable"
	}
	if !term {
		return false, "input is not a terminal"
	}
	return true, ""
}
