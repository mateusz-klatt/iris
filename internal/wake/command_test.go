package wake

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	commandSession = "019a6d62-1c75-7c28-8c15-5f7a17d4d110"
	commandOther   = "019a6d62-1c75-7c28-8c15-5f7a17d4d111"
	commandVersion = "0.167.0"
	commandSecret  = "SENSITIVE_COMMAND_OUTPUT_MUST_NOT_BE_PERSISTED"
	commandAccept  = "Queued message 019a6d62-22e0-7636-a145-7b7d38ff12b8 for thread " + commandSession + "."
	commandWarning = "Warning: message was queued, but could not resume thread " + commandSession + ": start a local daemon or specify a server with --remote"
	commandReject  = "Error: failed to queue session message: thread/queue/add failed: failed to read thread: invalid thread-store request: no rollout found for thread id " + commandSession + " (code -32603)"
)

type commandScenario struct {
	Out, Err           string
	VersionOut         string
	OutBytes, ErrBytes int
	Exit               int
	Hang, Signal       bool
	KeepPipes          bool
}

type commandRecord struct {
	Args                 []string
	CWD, Home, CodexHome string
}

// This stand-in records only controlled home sentinels, never the parent's full
// environment. It implements process behavior directly and calls no wake code.
// A unique init mode coexists with the state tests' separate subprocess helpers.
func init() {
	mode := os.Getenv("IRIS_WAKE_COMMAND_HELPER")
	if mode == "" {
		return
	}
	os.Exit(runCommandHelper(mode))
}

func runCommandHelper(mode string) int {
	control := os.Getenv("IRIS_WAKE_COMMAND_CONTROL")
	if mode == "pipe-keeper" {
		_ = os.WriteFile(control+".keeper-ready", nil, 0600)
		defer func() { _ = os.WriteFile(control+".keeper-done", nil, 0600) }()
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
			if _, err := os.Stat(control + ".release"); err == nil {
				return 0
			}
			time.Sleep(5 * time.Millisecond)
		}
		return 0
	}
	var scenario commandScenario
	if err := json.Unmarshal([]byte(os.Getenv("IRIS_WAKE_COMMAND_SCENARIO")), &scenario); err != nil {
		return 91
	}
	if len(os.Args) == 2 && os.Args[1] == "--version" && scenario.VersionOut != "" {
		scenario = commandScenario{Out: scenario.VersionOut}
	}
	cwd, err := os.Getwd()
	if err != nil {
		return 92
	}
	f, err := os.OpenFile(os.Getenv("IRIS_WAKE_COMMAND_RECORD"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return 93
	}
	err = json.NewEncoder(f).Encode(commandRecord{Args: os.Args[1:], CWD: cwd, Home: os.Getenv("HOME"), CodexHome: os.Getenv("CODEX_HOME")})
	if closeErr := f.Close(); err != nil || closeErr != nil {
		return 94
	}
	_ = os.WriteFile(control+".ready", nil, 0600)
	_, _ = io.WriteString(os.Stdout, scenario.Out)
	_, _ = io.WriteString(os.Stderr, scenario.Err)
	_, _ = os.Stdout.Write(bytes.Repeat([]byte("x"), scenario.OutBytes))
	_, _ = os.Stderr.Write(bytes.Repeat([]byte("x"), scenario.ErrBytes))
	if scenario.KeepPipes {
		child := exec.Command(os.Args[0])
		child.Env = append(os.Environ(), "IRIS_WAKE_COMMAND_HELPER=pipe-keeper")
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
		if err := child.Start(); err != nil {
			return 95
		}
		defer func() { _ = child.Process.Release() }()
		for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
			if _, err := os.Stat(control + ".keeper-ready"); err == nil {
				return scenario.Exit
			}
			time.Sleep(5 * time.Millisecond)
		}
		return 96
	}
	if scenario.Signal {
		self, err := os.FindProcess(os.Getpid())
		if err != nil || self.Kill() != nil {
			return 97
		}
	}
	if scenario.Hang || scenario.Signal {
		time.Sleep(time.Minute)
	}
	return scenario.Exit
}

func commandBinary(t *testing.T) string {
	t.Helper()
	path, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	source, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := source.Close(); err != nil {
			t.Errorf("close recording executable: %v", err)
		}
	}()
	dir := filepath.Join(t.TempDir(), "binary directory with spaces")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	name := "codex"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	path = filepath.Join(dir, name)
	destination, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0700)
	if err != nil {
		t.Fatal(err)
	}
	_, copyErr := io.Copy(destination, source)
	closeErr := destination.Close()
	if copyErr != nil || closeErr != nil {
		t.Fatalf("copy recording executable: %v, close %v", copyErr, closeErr)
	}
	// Launch the copy once before any test times it. macOS assesses an executable on its
	// first launch; on an Intel Mac with macOS 26 a fresh copy of the race-built test binary
	// took 2.1 s to launch the first time and 1.0 s the second, and under a full race run
	// that first launch passed the 5 s version timeout. This launch runs no test and no
	// helper mode, outside every timeout, so the tests time the commands, not that check.
	warm := exec.Command(path, "-test.run=^$")
	warm.Env = append(os.Environ(), "IRIS_WAKE_COMMAND_HELPER=", "IRIS_WAKE_PROXY_HELPER=", "IRIS_WAKE_STATE_HELPER=")
	if out, err := warm.CombinedOutput(); err != nil {
		t.Fatalf("first launch of the copied executable: %v\n%s", err, out)
	}
	return path
}

func commandSetup(t *testing.T, binary string, scenario commandScenario) (*commands, string, string) {
	t.Helper()
	t.Setenv("PATH", filepath.Dir(binary))
	t.Setenv("IRIS_WAKE_COMMAND_HELPER", "scenario")
	setCommandScenario(t, scenario)
	dir := t.TempDir()
	record, control := filepath.Join(dir, "record.json"), filepath.Join(dir, "control")
	t.Setenv("IRIS_WAKE_COMMAND_RECORD", record)
	t.Setenv("IRIS_WAKE_COMMAND_CONTROL", control)
	// Instrumented helper binaries must not add the missing-GOCOVERDIR warning
	// to the very stdout/stderr the command classifier is testing.
	t.Setenv("GOCOVERDIR", t.TempDir())
	c := newCommands()
	if c.path != binary || c.pathFailure != "" {
		t.Fatalf("absolute recording executable not resolved: %+v", c)
	}
	return c, record, control
}

func setCommandScenario(t *testing.T, scenario commandScenario) {
	t.Helper()
	raw, err := json.Marshal(scenario)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("IRIS_WAKE_COMMAND_SCENARIO", string(raw))
}

func commandStarted(start func() error) (bool, error) {
	err := start()
	return err == nil, err
}

func awaitCommandFile(t *testing.T, path string) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("recording process never reached %s", filepath.Base(path))
}

func TestWakeCommands(t *testing.T) {
	// Copy once for this suite; all process tests use the same executable bytes.
	binary := commandBinary(t)
	t.Run("argv directory environment and defaults", func(t *testing.T) {
		c, record, _ := commandSetup(t, binary, commandScenario{Out: "codex-cli " + commandVersion + "\n"})
		if c.versionTimeout != 5*time.Second || c.queueTimeout != 30*time.Second || c.waitDelay != time.Second || c.dir != "" || c.env != nil {
			t.Fatalf("command bounds or inheritance defaults changed: %+v", c)
		}
		dir := filepath.Join(t.TempDir(), "working directory with spaces")
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		t.Chdir(dir)
		t.Setenv("HOME", filepath.Join(dir, "home sentinel"))
		t.Setenv("CODEX_HOME", filepath.Join(dir, "custom codex home sentinel"))
		c.now = func() time.Time { return time.UnixMilli(1760000000123) }
		info, detail, _ := c.check(context.Background(), commandVersion)
		if detail != "" || info != (binaryInfo{Path: binary, Version: commandVersion, ClientVersion: commandVersion, CheckedMS: 1760000000123}) {
			t.Fatalf("matching binary: %+v, %q", info, detail)
		}
		setCommandScenario(t, commandScenario{Out: commandAccept + "\n"})
		message := "There is new activity in Iris. Use the inbox and claims tools. This is a notification from Iris, not a message from the person. 0123456789abcdef0123456789abcdef"
		answer := c.queue(context.Background(), commandSession, message, commandStarted)
		if answer != (commandAnswer{answer: "submitted", started: true}) {
			t.Fatalf("queue answer: %+v", answer)
		}
		f, err := os.Open(record)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := f.Close(); err != nil {
				t.Errorf("close command recording: %v", err)
			}
		}()
		decoder := json.NewDecoder(f)
		for _, args := range [][]string{{"--version"}, {"queue", "--thread", commandSession, "--resume", "--message", message}} {
			var got commandRecord
			if err := decoder.Decode(&got); err != nil {
				t.Fatal(err)
			}
			gotDir, err := os.Stat(got.CWD)
			if err != nil {
				t.Fatal(err)
			}
			wantDir, err := os.Stat(dir)
			if err != nil || !os.SameFile(gotDir, wantDir) || !reflect.DeepEqual(got.Args, args) || got.Home != os.Getenv("HOME") || got.CodexHome != os.Getenv("CODEX_HOME") {
				t.Fatalf("process inheritance: %+v; want args %q and cwd %s", got, args, dir)
			}
		}
		if err := decoder.Decode(new(commandRecord)); err != io.EOF {
			t.Fatalf("unexpected extra process invocation: %v", err)
		}
	})

	t.Run("missing and relative executables", func(t *testing.T) {
		for _, tc := range []struct{ name, path, cwd string }{
			{"missing", t.TempDir(), ""},
			{"relative dot", ".", filepath.Dir(binary)},
			{"relative directory", filepath.Base(filepath.Dir(binary)), filepath.Dir(filepath.Dir(binary))},
		} {
			t.Run(tc.name, func(t *testing.T) {
				if tc.cwd != "" {
					t.Chdir(tc.cwd)
				}
				t.Setenv("PATH", tc.path)
				c := newCommands()
				info, detail, differs := c.check(context.Background(), commandVersion)
				if c.path != "" || detail != "Codex executable is not available on PATH" || differs || info.Path != "" || info.Version != "" {
					t.Fatalf("unavailable executable admitted: %+v, %q", info, detail)
				}
			})
		}
	})

	t.Run("initialize version absent starts no process", func(t *testing.T) {
		c, record, _ := commandSetup(t, binary, commandScenario{Out: commandSecret})
		info, detail, _ := c.check(context.Background(), "")
		if detail != "Codex initialization did not name its version" || info.Version != "" {
			t.Fatalf("missing initialize version: %+v, %q", info, detail)
		}
		if _, err := os.Stat(record); !os.IsNotExist(err) {
			t.Fatalf("missing initialize version launched a process: %v", err)
		}
	})

	t.Run("version results", func(t *testing.T) {
		for _, tc := range []struct {
			name     string
			scenario commandScenario
			want     string
		}{
			{"mismatch", commandScenario{Out: "codex-cli 0.166.0\n"}, "Codex executable and initialized client versions differ"},
			{"sensitive mismatch", commandScenario{Out: "codex-cli " + commandSecret + "\n"}, "Codex executable and initialized client versions differ"},
			{"nonzero", commandScenario{Out: "codex-cli " + commandVersion + "\n", Err: commandSecret, Exit: 2}, "Codex version command did not complete successfully"},
			{"empty", commandScenario{}, "Codex version response named no version"},
			{"whitespace", commandScenario{Out: " \r\n\t \n"}, "Codex version response named no version"},
			{"overflow stdout", commandScenario{OutBytes: (4 << 10) + 1}, "Codex version response exceeded its bound"},
			{"overflow stderr", commandScenario{ErrBytes: (4 << 10) + 1}, "Codex version response exceeded its bound"},
			{"combined output overflow", commandScenario{OutBytes: 2 << 10, ErrBytes: (2 << 10) + 1}, "Codex version response exceeded its bound"},
			{"matching stderr", commandScenario{Err: "codex-cli " + commandVersion + "\r\n"}, ""},
			{"blank leading line", commandScenario{Out: "\r\n  \n codex-cli " + commandVersion + "\n"}, ""},
			{"exact size", commandScenario{Out: strings.Repeat(" ", (4<<10)-len("codex-cli "+commandVersion+"\n")) + "codex-cli " + commandVersion + "\n"}, ""},
		} {
			t.Run(tc.name, func(t *testing.T) {
				c, _, _ := commandSetup(t, binary, tc.scenario)
				info, detail, differs := c.check(context.Background(), commandVersion)
				if detail != tc.want || (detail == "" && info.Version != commandVersion) || (detail != "" && info.Version != "") {
					t.Fatalf("version classification: %+v, %q; want %q", info, detail, tc.want)
				}
				// Only a version the executable did name, and that is not the client's, is a
				// version difference; a check that could not read one is not.
				if differs != strings.HasSuffix(tc.name, "mismatch") {
					t.Fatalf("%s: version differs = %t", tc.name, differs)
				}
				raw, err := json.Marshal(struct {
					Binary binaryInfo
					Detail string
				}{info, detail})
				if err != nil || bytes.Contains(raw, []byte(commandSecret)) {
					t.Fatalf("unexpected command output retained in binary metadata or detail: %v", err)
				}
				if tc.name == "sensitive mismatch" {
					s := testStore(t)
					next := testDocument(testAttempt(1, "binary_check", "failed"))
					next.Binary, next.LastFailure = info, &failure{AtMS: 1, Detail: detail}
					mustPublish(t, s, mustSnapshot(t, s), next)
					persisted, err := os.ReadFile(s.path())
					if err != nil || bytes.Contains(persisted, []byte(commandSecret)) {
						t.Fatalf("unexpected command output persisted: %v", err)
					}
				}
			})
		}
	})

	t.Run("version timeout and missing binary at execution", func(t *testing.T) {
		c, _, _ := commandSetup(t, binary, commandScenario{Out: commandSecret, Hang: true})
		c.versionTimeout = 100 * time.Millisecond
		if info, detail, _ := c.check(context.Background(), commandVersion); detail != "Codex version command did not complete successfully" || info.Version != "" || strings.Contains(detail, commandSecret) {
			t.Fatalf("version timeout: %+v, %q", info, detail)
		}
		c.path = filepath.Join(t.TempDir(), "missing-codex")
		if info, detail, _ := c.check(context.Background(), commandVersion); detail != "Codex version command did not complete successfully" || info.Version != "" {
			t.Fatalf("version start failure: %+v, %q", info, detail)
		}
	})

	t.Run("real queue outcomes", func(t *testing.T) {
		for _, tc := range []struct {
			name     string
			scenario commandScenario
			answer   string
		}{
			{"submitted", commandScenario{Out: commandAccept + "\n"}, "submitted"},
			{"stored stderr", commandScenario{Out: commandAccept + "\n", Err: commandWarning + "\n"}, "stored"},
			{"stored stdout after", commandScenario{Out: commandAccept + "\n" + commandWarning + "\n"}, "stored"},
			{"stored stdout before", commandScenario{Out: commandWarning + "\n" + commandAccept + "\n"}, "stored"},
			{"exact rejection", commandScenario{Err: commandReject + "\n", Exit: 1}, "failed"},
			{"other thread rejection", commandScenario{Err: strings.ReplaceAll(commandReject, commandSession, commandOther) + "\n", Exit: 1}, "unknown"},
			{"rejection extra text", commandScenario{Err: commandReject + "\n" + commandSecret + "\n", Exit: 1}, "unknown"},
			{"rejection changed code", commandScenario{Err: strings.ReplaceAll(commandReject, "-32603", "-32602") + "\n", Exit: 1}, "unknown"},
			{"rejection suffix missing", commandScenario{Err: strings.TrimSuffix(commandReject, " (code -32603)") + "\n", Exit: 1}, "unknown"},
			{"rejection stdout", commandScenario{Out: commandReject + "\n", Exit: 1}, "unknown"},
			{"rejection wrong exit", commandScenario{Err: commandReject + "\n", Exit: 2}, "unknown"},
			{"rejection success exit", commandScenario{Err: commandReject + "\n"}, "unknown"},
			{"accepted nonzero", commandScenario{Out: commandAccept + "\n", Exit: 2}, "unknown"},
			{"signal", commandScenario{Out: commandAccept + "\n", Signal: true}, "unknown"},
			{"unexpected output", commandScenario{Out: commandSecret}, "unknown"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				c, _, _ := commandSetup(t, binary, tc.scenario)
				got := c.queue(context.Background(), commandSession, "fixed test prompt", commandStarted)
				if got.answer != tc.answer || !got.started || strings.Contains(got.detail, commandSecret) {
					t.Fatalf("queue classification: %+v, want %s", got, tc.answer)
				}
			})
		}
	})

	t.Run("admission refusal never starts versus start failure", func(t *testing.T) {
		c, record, _ := commandSetup(t, binary, commandScenario{Out: commandAccept + "\n"})
		got := c.queue(context.Background(), commandSession, "fixed test prompt", func(func() error) (bool, error) { return false, nil })
		if got != (commandAnswer{answer: "aborted", detail: "Root binding changed before command start"}) {
			t.Fatalf("refused admission: %+v", got)
		}
		if _, err := os.Stat(record); !os.IsNotExist(err) {
			t.Fatalf("refused admission started the process: %v", err)
		}
		c.path = filepath.Join(t.TempDir(), "missing-codex")
		got = c.queue(context.Background(), commandSession, "fixed test prompt", commandStarted)
		if got != (commandAnswer{answer: "failed", detail: "Codex queue command could not start"}) {
			t.Fatalf("failed Start: %+v", got)
		}
	})

	t.Run("cancellation after start is unknown", func(t *testing.T) {
		c, _, control := commandSetup(t, binary, commandScenario{Out: commandAccept + "\n", Hang: true})
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		answers := make(chan commandAnswer, 1)
		go func() { answers <- c.queue(ctx, commandSession, "fixed test prompt", commandStarted) }()
		awaitCommandFile(t, control+".ready")
		cancel()
		if got := <-answers; got != (commandAnswer{answer: "unknown", detail: "Codex queue command exceeded its bound", started: true}) {
			t.Fatalf("cancelled queue: %+v", got)
		}
	})

	t.Run("queue deadline is unknown", func(t *testing.T) {
		c, _, _ := commandSetup(t, binary, commandScenario{Out: commandAccept + "\n", Hang: true})
		c.queueTimeout = 500 * time.Millisecond
		if got := c.queue(context.Background(), commandSession, "fixed test prompt", commandStarted); got != (commandAnswer{answer: "unknown", detail: "Codex queue command exceeded its bound", started: true}) {
			t.Fatalf("queue deadline: %+v", got)
		}
	})

	t.Run("inherited pipes are bounded and unknown", func(t *testing.T) {
		c, _, control := commandSetup(t, binary, commandScenario{Out: commandAccept + "\n", KeepPipes: true})
		c.waitDelay = 50 * time.Millisecond
		t.Cleanup(func() {
			if err := os.WriteFile(control+".release", nil, 0600); err != nil {
				t.Error(err)
			}
			awaitCommandFile(t, control+".keeper-done")
		})
		before := time.Now()
		got := c.queue(context.Background(), commandSession, "fixed test prompt", commandStarted)
		if got != (commandAnswer{answer: "unknown", detail: "Codex queue response was not a known outcome", started: true}) || time.Since(before) > 3*time.Second {
			t.Fatalf("inherited pipes: %+v, duration %s", got, time.Since(before))
		}
		if _, err := os.Stat(control + ".keeper-ready"); err != nil {
			t.Fatalf("test did not retain the command pipes: %v", err)
		}
	})

	t.Run("stdout and stderr have independent limits", func(t *testing.T) {
		c, _, _ := commandSetup(t, binary, commandScenario{OutBytes: (32 << 10) + 1, ErrBytes: (32 << 10) + 1})
		if got := c.queue(context.Background(), commandSession, "fixed test prompt", commandStarted); got != (commandAnswer{answer: "unknown", detail: "Codex queue response was not a known outcome", started: true}) {
			t.Fatalf("combined output below each separate cap was treated as overflow: %+v", got)
		}
		for _, stderr := range []bool{false, true} {
			for _, size := range []int{64 << 10, (64 << 10) + 1} {
				t.Run(fmt.Sprintf("stderr_%t_size_%d", stderr, size), func(t *testing.T) {
					scenario := commandScenario{OutBytes: size}
					if stderr {
						scenario = commandScenario{Out: commandAccept + "\n", ErrBytes: size}
					}
					c, _, _ := commandSetup(t, binary, scenario)
					got := c.queue(context.Background(), commandSession, "fixed test prompt", commandStarted)
					want := "Codex queue response was not a known outcome"
					if size > 64<<10 {
						want = "Codex queue command exceeded its bound"
					}
					if got.answer != "unknown" || !got.started || got.detail != want {
						t.Fatalf("output limit: %+v, want %q", got, want)
					}
				})
			}
		}
	})
}

func TestWakeSessionUUIDAndRandomPromptIdentity(t *testing.T) {
	for _, tc := range []struct {
		input, want string
	}{
		{commandSession, commandSession},
		{strings.ToUpper(commandSession), commandSession},
		{"", ""},
		{commandSession + "\n", ""},
		{strings.Replace(commandSession, "-", "0", 1), ""},
		{"g" + commandSession[1:], ""},
		{"é" + commandSession[2:], ""},
		{" " + commandSession[1:], ""},
	} {
		if got, ok := canonicalSession(tc.input); got != tc.want || ok != (tc.want != "") {
			t.Fatalf("canonical session %q: %q, %t; want %q", tc.input, got, ok, tc.want)
		}
	}
	seen := map[string]bool{}
	for range 32 {
		id := attemptID()
		decoded, err := hex.DecodeString(id)
		if err != nil || len(decoded) != 16 || strings.ToLower(id) != id || seen[id] {
			t.Fatalf("reservation identity is not unique canonical 128-bit hex: %q, %v", id, err)
		}
		seen[id] = true
		want := "There is new activity in Iris. Use the inbox and claims tools. This is a notification from Iris, not a message from the person. " + id
		if got := promptFor(id); got != want {
			t.Fatalf("prompt changed: %q, want %q", got, want)
		}
	}
}

func TestWakeQueueParserAcceptsOnlyCompleteRecordedLines(t *testing.T) {
	for _, ending := range []string{"", "\n", "\r\n"} {
		answer, detail := classifyQueue(commandSession, commandAccept+ending, "", nil)
		if answer != "submitted" || detail != "" {
			t.Fatalf("accepted line ending %q: %s, %q", ending, answer, detail)
		}
		for _, stdout := range []string{commandAccept + ending, commandAccept + "\r\n" + commandWarning + ending, commandWarning + "\n" + commandAccept + ending} {
			stderr := ""
			if stdout == commandAccept+ending {
				stderr = commandWarning + ending
			}
			if answer, _ := classifyQueue(commandSession, stdout, stderr, nil); answer != "stored" {
				t.Fatalf("stored line ending %q: %q, %q: %s", ending, stdout, stderr, answer)
			}
		}
	}
	for _, tc := range []struct{ name, stdout, stderr string }{
		{"empty", "", ""},
		{"bare final CR", commandAccept + "\r", ""},
		{"bare warning CR", commandAccept + "\n", commandWarning + "\r"},
		{"bare CR within output", commandAccept + "\r" + commandWarning, ""},
		{"leading blank", "\n" + commandAccept + "\n", ""},
		{"trailing blank", commandAccept + "\n\n", ""},
		{"leading space", " " + commandAccept, ""},
		{"trailing space", commandAccept + " ", ""},
		{"other thread", strings.ReplaceAll(commandAccept, commandSession, commandOther), ""},
		{"accepted on stderr", "", commandAccept + "\n"},
		{"empty queue id", "Queued message  for thread " + commandSession + ".", ""},
		{"space in queue id", "Queued message two words for thread " + commandSession + ".", ""},
		{"tab in queue id", "Queued message two\twords for thread " + commandSession + ".", ""},
		{"unicode space in queue id", "Queued message two\u2003words for thread " + commandSession + ".", ""},
		{"control in queue id", "Queued message bad\x00id for thread " + commandSession + ".", ""},
		{"no acceptance prefix", "message was queued", ""},
		{"missing acceptance suffix", strings.TrimSuffix(commandAccept, "."), ""},
		{"foreign warning", commandAccept + "\n", strings.ReplaceAll(commandWarning, commandSession, commandOther)},
		{"warning extra output", commandAccept + "\n", commandWarning + "\n" + commandSecret + "\n"},
		{"duplicate acceptance", commandAccept + "\n" + commandAccept + "\n", ""},
		{"secret stderr", commandAccept + "\n", commandSecret},
	} {
		t.Run(tc.name, func(t *testing.T) {
			answer, detail := classifyQueue(commandSession, tc.stdout, tc.stderr, nil)
			if answer != "unknown" || detail != "Codex queue response was not a known outcome" || strings.Contains(detail, commandSecret) {
				t.Fatalf("unexpected output accepted: %s, %q", answer, detail)
			}
		})
	}
}

func TestWakeOutputLimitPreservesPrefixAndNeverReturnsAShortWrite(t *testing.T) {
	buffer := &limitedOutput{limit: 5}
	for _, part := range []string{"ab", "cde", "f", "later"} {
		if n, err := buffer.Write([]byte(part)); err != nil || n != len(part) {
			t.Fatalf("output writer changed process I/O semantics: %d, %v", n, err)
		}
	}
	if string(buffer.data) != "abcde" || !buffer.overflow {
		t.Fatalf("output did not retain bounded prefix: %q, overflow %t", buffer.data, buffer.overflow)
	}
	concurrent := &limitedOutput{limit: 16}
	var done sync.WaitGroup
	for range 8 {
		done.Go(func() {
			if n, err := concurrent.Write([]byte("aaaa")); err != nil || n != 4 {
				t.Errorf("concurrent output write: %d, %v", n, err)
			}
		})
	}
	done.Wait()
	if string(concurrent.data) != strings.Repeat("a", 16) || !concurrent.overflow {
		t.Fatalf("concurrent outputs exceeded or corrupted the cap: %q, %t", concurrent.data, concurrent.overflow)
	}
}
