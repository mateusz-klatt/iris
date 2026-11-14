package hook

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/mateusz-klatt/iris/internal/proc"
)

// asInteractive makes every client process an interactive session on a terminal, as the tests
// that are not about the mode need.
func asInteractive(t *testing.T) {
	t.Helper()
	readAs(t, []string{"client"}, nil, true, nil)
}

// grokProcess is the grok process a hook of Grok's interactive session finds above it.
func grokProcess() (proc.Ancestor, error) {
	return proc.Ancestor{PID: 3700, StartMS: 1794611214330, Name: "grok"}, nil
}

// readAs makes every client process have args and read a terminal, or fail with the errors given.
func readAs(t *testing.T, args []string, argsErr error, terminal bool, terminalErr error) *int {
	t.Helper()
	oldArgs, oldTerminal := clientArgs, clientTerminal
	t.Cleanup(func() { clientArgs, clientTerminal = oldArgs, oldTerminal })
	asked := 0
	clientArgs = func(int, int64) ([]string, error) { return args, argsErr }
	clientTerminal = func(int, int64) (bool, error) {
		asked++
		return terminal, terminalErr
	}
	return &asked
}

func TestAgyModeReadsEveryFlagForm(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want clientMode
	}{
		// The measured runs of agy 1.5.8.
		{[]string{"agy", "-p", "reply with exactly PONG", "--model", "gemini-3.8-flash-low"}, modePrint},
		{[]string{"agy", "--model", "gemini-3.8-flash-low"}, modeInput},
		{[]string{"agy", "-i", "reply with exactly PONG", "--model", "gemini-3.8-flash-low"}, modeInteractive},
		{[]string{"agy", "--dangerously-skip-permissions", "--conversation=988ac0fb-0e27-4a85-9ce0-44b99133be68"}, modeInput},
		// Every spelling of print mode, the delegation bridges' among them.
		{[]string{"agy", "--print", "x"}, modePrint},
		{[]string{"agy", "--prompt=x"}, modePrint},
		{[]string{"agy", "-print=x"}, modePrint},
		{[]string{"agy", "--p=x"}, modePrint},
		{[]string{"agy", "--model", "m", "-p", "x"}, modePrint},
		{[]string{"agy", "-p", "x", "--not-a-flag"}, modePrint},
		// A value is the next argument even when it looks like a flag.
		{[]string{"agy", "--model", "-p"}, modeInput},
		{[]string{"agy", "-i", "-p"}, modeInteractive},
		{[]string{"agy", "--prompt-interactive=fix it", "-c"}, modeInteractive},
		{[]string{"agy", "agy"}, modeCommand},
		// Booleans, with or without a value.
		{[]string{"agy"}, modeInput},
		{[]string{"agy", "-c", "--continue=true", "--sandbox", "--remote-control=false"}, modeInput},
		// Subcommands, the protocol server among them, and what follows --.
		{[]string{"agy", "acp"}, modeCommand},
		{[]string{"agy", "mcp", "list"}, modeCommand},
		{[]string{"agy", "--model", "m", "acp"}, modeCommand},
		{[]string{"agy", "--", "acp"}, modeCommand},
		{[]string{"agy", "-"}, modeCommand},
		{[]string{"agy", "--"}, modeInput},
		// What agy itself refuses, or Iris does not know.
		{[]string{"agy", "--model"}, modeUnknown},
		{[]string{"agy", "---p=x"}, modeUnknown},
		{[]string{"agy", "--=x"}, modeUnknown},
		{[]string{"agy", "-=x"}, modeUnknown},
		{[]string{"agy", "--zz-undefined"}, modeUnknown},
	} {
		if got := agyMode(tc.args); got != tc.want {
			t.Errorf("%q: %d, want %d", tc.args, got, tc.want)
		}
	}
}

func TestGrokModeReadsEveryFlagForm(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want clientMode
	}{
		// The delegation bridge's form and an interactive session that resumed.
		{[]string{"grok", "--verbatim", "-p", "reply PONG"}, modePrint},
		{[]string{"grok", "--resume", "01a1add1-712e-7e22-a2a9-97ad1e9005b9"}, modeInput},
		// Every spelling of print mode.
		{[]string{"grok", "--single", "x"}, modePrint},
		{[]string{"grok", "--single=x"}, modePrint},
		{[]string{"grok", "--prompt-file", "prompt.md"}, modePrint},
		{[]string{"grok", "--prompt-json=[]"}, modePrint},
		{[]string{"grok", "-cp", "x"}, modePrint},
		{[]string{"grok", "-m", "grok-4.7", "-p", "x"}, modePrint},
		{[]string{"grok", "fix the bug", "--single", "x"}, modePrint},
		{[]string{"grok", "-w", "-p", "x"}, modePrint},
		{[]string{"grok", "--resume=s", "--single", "x"}, modePrint},
		// A short option with a value takes the rest of its group, or the next argument.
		{[]string{"grok", "-mp"}, modeInput},
		{[]string{"grok", "-m", "-p"}, modeInput},
		{[]string{"grok", "-s", "4f3c", "-c"}, modeInput},
		// An optional value is taken only from an argument that is not an option.
		{[]string{"grok", "-r"}, modeInput},
		{[]string{"grok", "-r", "abc", "--fullscreen"}, modeInput},
		{[]string{"grok", "-rabc"}, modeInput},
		{[]string{"grok", "-w", "name"}, modeInput},
		{[]string{"grok", "--worktree=name"}, modeInput},
		{[]string{"grok", "--worktree", "--verbatim"}, modeInput},
		{[]string{"grok", "-cvh"}, modeInput},
		// The first positional argument is a subcommand or the session's prompt.
		{[]string{"grok", "agent", "x"}, modeCommand},
		{[]string{"grok", "--model", "m", "mcp", "list"}, modeCommand},
		{[]string{"grok", "fix the bug"}, modeInput},
		{[]string{"grok", "fix", "agent"}, modeInput},
		{[]string{"grok", "-"}, modeInput},
		{[]string{"grok", "--", "-p"}, modeInput},
		// What Grok itself refuses, or Iris does not know.
		{[]string{"grok", "-m"}, modeUnknown},
		{[]string{"grok", "--model"}, modeUnknown},
		{[]string{"grok", "--zz-undefined"}, modeUnknown},
		{[]string{"grok", "-x"}, modeUnknown},
	} {
		if got := grokMode(tc.args); got != tc.want {
			t.Errorf("%q: %d, want %d", tc.args, got, tc.want)
		}
	}
}

// A session is interactive when its arguments say so, or when they leave it to the input and the
// input is a terminal. Anything else, including what cannot be read, gets no request.
func TestInteractiveReadsTheClientProcess(t *testing.T) {
	anc := proc.Ancestor{PID: 7, StartMS: 8, Name: "agy"}
	denied := errors.New("denied")
	for _, tc := range []struct {
		name        string
		args        []string
		argsErr     error
		terminal    bool
		terminalErr error
		want        bool
		why         string
		reads       int
	}{
		{"arguments refused", nil, denied, true, nil, false, "arguments unreadable", 0},
		{"no arguments", []string{}, nil, true, nil, false, "arguments unreadable", 0},
		{"print mode", []string{"agy", "-p", "x"}, nil, true, nil, false, "print mode", 0},
		{"subcommand", []string{"agy", "acp"}, nil, true, nil, false, "subcommand", 0},
		{"unknown flag", []string{"agy", "--zz"}, nil, true, nil, false, "unknown argument", 0},
		{"prompted", []string{"agy", "-i", "x"}, nil, false, nil, true, "", 0},
		{"input refused", []string{"agy"}, nil, true, denied, false, "input unreadable", 1},
		{"piped input", []string{"agy"}, nil, false, nil, false, "input is not a terminal", 1},
		{"terminal", []string{"agy"}, nil, true, nil, true, "", 1},
	} {
		reads := readAs(t, tc.args, tc.argsErr, tc.terminal, tc.terminalErr)
		ok, why := interactive(anc, agyMode)
		if ok != tc.want || why != tc.why || *reads != tc.reads {
			t.Errorf("%s: %v %q, %d terminal reads", tc.name, ok, why, *reads)
		}
	}
}

// The three Stop payloads of agy 1.5.8, from -p, a piped prompt and -i, are the same apart from
// their identities. Only the process tells them apart: print mode and a piped prompt get no
// request and no record, and an interactive session is asked once.
func TestAntigravityStopAsksOnlyAnInteractiveSession(t *testing.T) {
	for _, tc := range []struct {
		capture  string
		args     []string
		terminal bool
		ask      bool
		why      string
	}{
		{"stop_print.json", []string{"agy", "-p", "reply with exactly PONG", "--model", "gemini-3.8-flash-low"}, false, false, "print mode"},
		{"stop_piped.json", []string{"agy", "--model", "gemini-3.8-flash-low"}, false, false, "input is not a terminal"},
		{"stop_interactive.json", []string{"agy", "-i", "reply with exactly PONG", "--model", "gemini-3.8-flash-low"}, true, true, ""},
	} {
		t.Run(tc.capture, func(t *testing.T) {
			configDir, token, getenv := agyTestEnv(t)
			enableAgyWake(t, configDir)
			readAs(t, tc.args, nil, tc.terminal, nil)
			payload, err := os.ReadFile(filepath.Join("testdata", "antigravity", tc.capture))
			if err != nil {
				t.Fatal(err)
			}
			const pid, start = 6019, int64(1794616885310)
			out := (Runner{
				ClientName: "antigravity",
				EventName:  "Stop",
				Stdin:      bytes.NewReader(payload),
				Getenv:     getenv,
				Ancestor:   func() (proc.Ancestor, error) { return proc.Ancestor{PID: pid, StartMS: start, Name: "agy"}, nil },
				IrisPath:   func() string { return "/usr/local/bin/iris" },
				after:      never,
				logAfter:   never,
			}).Run()
			asked := strings.Contains(string(out.Stdout), `"decision":"continue"`)
			state := readAntigravityProcState(antigravityHelpersMemory(configDir).dir, pid, start)
			requested := state.Helper != nil && state.Helper.Status == "arm_requested"
			if asked != tc.ask || requested != tc.ask {
				t.Fatalf("asked %v, requested %v: %s", asked, requested, out.Stdout)
			}
			seen, err := ReadRecentObservations(token, 10)
			if err != nil {
				t.Fatal(err)
			}
			notes := []string{}
			for _, f := range seen {
				notes = append(notes, f.Error)
			}
			want := []string{}
			if tc.why != "" {
				want = []string{wakeNotAsked + tc.why}
			}
			if !slices.Equal(notes, want) {
				t.Fatalf("observed %q, want %q", notes, want)
			}
		})
	}
}

// Grok's Stop asks for the monitor only in an interactive session of a grok process. A print-mode
// run and a process that is not grok are not asked, and the observed log says why.
func TestGrokStopAsksOnlyAnInteractiveSession(t *testing.T) {
	payload, err := os.ReadFile(filepath.Join("testdata", "grok", "Stop.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		process string
		args    []string
		block   bool
		why     string
	}{
		{"print mode", "grok", []string{"grok", "--verbatim", "-p", "reply PONG"}, false, "print mode"},
		{"headless agent", "grok", []string{"grok", "agent", "x"}, false, "subcommand"},
		{"not grok", "node", []string{"node"}, false, "no grok process"},
		{"interactive", "grok", []string{"grok", "--resume", "01a1add1-712e-7e22-a2a9-97ad1e9005b9"}, true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			readAs(t, tc.args, nil, true, nil)
			env := grokGetenv(t, nil)
			out := (Runner{
				ClientName: "grok",
				EventName:  "Stop",
				Stdin:      bytes.NewReader(payload),
				Getenv:     env,
				Ancestor: func() (proc.Ancestor, error) {
					a, err := grokProcess()
					a.Name = tc.process
					return a, err
				},
				after:    never,
				logAfter: never,
			}).Run()
			if blocked := string(out.Stdout) == string(grokBlock(grokWatchReason).Stdout); blocked != tc.block {
				t.Fatalf("blocked %v: %s", blocked, out.Stdout)
			}
			seen, err := ReadRecentObservations(env("IRIS_TOKEN_FILE"), 10)
			if err != nil {
				t.Fatal(err)
			}
			notes := []string{}
			for _, f := range seen {
				notes = append(notes, f.Error)
			}
			want := []string{}
			if tc.why != "" {
				want = []string{wakeNotAsked + tc.why}
			}
			if !slices.Equal(notes, want) {
				t.Fatalf("observed %q, want %q", notes, want)
			}
		})
	}
}
