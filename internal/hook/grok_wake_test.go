package hook

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/mateusz-klatt/iris/internal/gitinfo"
	"github.com/mateusz-klatt/iris/internal/proc"
)

const grokSession = "01a1add1-712e-7e22-a2a9-97ad1e9005b9"

func grokAncestor(pid int, start int64, name string) func() (proc.Ancestor, error) {
	return func() (proc.Ancestor, error) {
		return proc.Ancestor{PID: pid, StartMS: start, Name: name}, nil
	}
}

func grokGetenv(t *testing.T, extra map[string]string) func(string) string {
	t.Helper()
	base := failureEnv(t)
	return func(key string) string {
		if extra != nil {
			if v, ok := extra[key]; ok {
				return v
			}
		}
		return base(key)
	}
}

func grokRun(t *testing.T, env func(string) string, anc func() (proc.Ancestor, error), event, payload string, hub Hub) Output {
	t.Helper()
	root := gitRepo(t)
	// A Windows temp path contains backslashes. Those are illegal JSON escapes,
	// so the payload must use forward slashes, which Windows accepts as a path.
	body := strings.ReplaceAll(payload, "ROOT", filepath.ToSlash(root))
	return Runner{
		ClientName: "grok",
		EventName:  event,
		Stdin:      strings.NewReader(body),
		Hub:        hub,
		Getenv:     env,
		Ancestor:   anc,
		Find: func(string) (gitinfo.Repo, error) {
			return gitinfo.Find(root)
		},
		after:    never,
		logAfter: never,
	}.Run()
}

func grokPID(t *testing.T, env func(string) string, pid int, start int64) string {
	t.Helper()
	dir, err := grokPIDDirectory(env)
	if err != nil || dir == "" {
		t.Fatalf("pid directory: %q %v", dir, err)
	}
	return grokPIDFile(dir, pid, start)
}

func TestGrokSessionStartWritesThePidFileAndASubagentDoesNotReplaceIt(t *testing.T) {
	env := grokGetenv(t, nil)
	anc := grokAncestor(4242, 1700000000000, "/usr/local/bin/grok")
	hub := scriptHub{}
	out := grokRun(t, env, anc, "SessionStart", `{"session_id":"`+grokSession+`","cwd":"ROOT","hook_event_name":"SessionStart"}`, hub)
	if out.Exit != 0 || !strings.Contains(string(out.Stdout), "systemMessage") || !strings.Contains(string(out.Stdout), "Iris handle:") {
		t.Fatalf("session start: %#v", out)
	}
	file := grokPID(t, env, 4242, 1700000000000)
	info, err := os.Stat(file)
	if err != nil || (runtime.GOOS != "windows" && info.Mode().Perm() != 0o600) {
		t.Fatalf("pid file mode: %v %v", info, err)
	}
	dirInfo, err := os.Stat(filepath.Dir(file))
	if err != nil || (runtime.GOOS != "windows" && dirInfo.Mode().Perm() != 0o700) {
		t.Fatalf("pid directory mode: %v %v", dirInfo, err)
	}
	got, ok := ReadGrokSession(filepath.Dir(file), 4242, 1700000000000)
	if !ok || got != grokSession {
		t.Fatalf("stored %q %v", got, ok)
	}
	if _, ok := ReadGrokSession(filepath.Dir(file), 4242, 1700000000001); ok {
		t.Fatal("a different start time read the session")
	}

	child := "01a1add0-f889-76d3-9b16-1775afd996e6"
	out = grokRun(t, env, anc, "SubagentStart", `{"session_id":"`+grokSession+`","cwd":"ROOT","hook_event_name":"SubagentStart","subagentId":"`+child+`"}`, hub)
	if out.Exit != 0 {
		t.Fatal(out)
	}
	if got, _ := os.ReadFile(file); strings.TrimSpace(string(got)) != grokSession {
		t.Fatalf("subagent start replaced the session: %q", got)
	}
	out = grokRun(t, env, anc, "SessionEnd", `{"session_id":"`+child+`","cwd":"ROOT","hook_event_name":"SessionEnd"}`, hub)
	if out.Exit != 0 {
		t.Fatal(out)
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatalf("subagent end removed the parent session: %v", err)
	}

	// A nested SessionStart is not how Grok reports a subagent today. The file still belongs
	// to the top-level session when a parent id is present.
	r := Runner{Getenv: env, Ancestor: anc}
	r.recordGrokSession(Event{Client: ClientGrok, Kind: KindSessionStart, SessionID: child, ParentSessionID: grokSession})
	if got, _ := os.ReadFile(file); strings.TrimSpace(string(got)) != grokSession {
		t.Fatalf("nested start replaced the session: %q", got)
	}

	out = grokRun(t, env, anc, "SessionEnd", `{"session_id":"`+grokSession+`","cwd":"ROOT","hook_event_name":"SessionEnd"}`, hub)
	if out.Exit != 0 {
		t.Fatal(out)
	}
	if _, err := os.Stat(file); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("parent end left the pid file: %v", err)
	}
}

func TestGrokSessionFileIsSkippedUnlessTheSessionJoined(t *testing.T) {
	env := grokGetenv(t, nil)
	anc := grokAncestor(7, 8, "grok")
	cases := []struct {
		name, event, body string
		hub               Hub
		ancestor          func() (proc.Ancestor, error)
	}{
		{"register failed", "SessionStart", `{"session_id":"` + grokSession + `","cwd":"ROOT","hook_event_name":"SessionStart"}`, scriptHub{regErr: errors.New("down")}, anc},
		{"not grok", "SessionStart", `{"session_id":"` + grokSession + `","cwd":"ROOT","hook_event_name":"SessionStart"}`, scriptHub{}, grokAncestor(7, 8, "python")},
		{"no start time", "SessionStart", `{"session_id":"` + grokSession + `","cwd":"ROOT","hook_event_name":"SessionStart"}`, scriptHub{}, grokAncestor(7, 0, "grok")},
		{"bad session id", "SessionStart", `{"session_id":"has space","cwd":"ROOT","hook_event_name":"SessionStart"}`, scriptHub{}, anc},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := grokPID(t, env, 7, 8)
			out := grokRun(t, env, tc.ancestor, tc.event, tc.body, tc.hub)
			if out.Exit != 0 {
				t.Fatal(out)
			}
			if _, err := os.Stat(before); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("wrote %s: %v", before, err)
			}
		})
	}
	out := Runner{
		ClientName: "grok",
		EventName:  "SessionStart",
		Stdin:      strings.NewReader(`{"session_id":"` + grokSession + `","cwd":"/definitely/not/a/repo","hook_event_name":"SessionStart"}`),
		Hub:        scriptHub{},
		Getenv:     env,
		Ancestor:   anc,
		after:      never,
		logAfter:   never,
	}.Run()
	if out.Exit != 0 || strings.Contains(string(out.Stdout), "systemMessage") {
		t.Fatalf("outside a repo: %#v", out)
	}
	if _, err := os.Stat(grokPID(t, env, 7, 8)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("outside a repo wrote a pid file: %v", err)
	}
}

func TestGrokSessionWriteFailureStillStartsTheSession(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(parent, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := grokGetenv(t, map[string]string{"IRIS_TOKEN_FILE": filepath.Join(parent, "hub.token")})
	out := grokRun(t, env, grokAncestor(7, 8, "GROK.EXE"), "SessionStart", `{"session_id":"`+grokSession+`","cwd":"ROOT","hook_event_name":"SessionStart"}`, scriptHub{})
	if out.Exit != 0 || !strings.Contains(string(out.Stdout), "systemMessage") {
		t.Fatalf("%#v", out)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(env("IRIS_TOKEN_FILE")), grokPIDDirName)); err == nil {
		t.Fatal("pid directory was created beside a file")
	}
}

func TestGrokSessionFileSurvivesAFailedEndAndADifferentSession(t *testing.T) {
	env := grokGetenv(t, nil)
	anc := grokAncestor(9, 10, "grok")
	grokRun(t, env, anc, "SessionStart", `{"session_id":"`+grokSession+`","cwd":"ROOT","hook_event_name":"SessionStart"}`, scriptHub{})
	file := grokPID(t, env, 9, 10)
	out := grokRun(t, env, anc, "SessionEnd", `{"session_id":"`+grokSession+`","cwd":"ROOT","hook_event_name":"SessionEnd"}`, scriptHub{endErr: errors.New("down")})
	if out.Exit != 0 {
		t.Fatal(out)
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatalf("failed end removed the pid file: %v", err)
	}
	out = grokRun(t, env, grokAncestor(0, 0, "grok"), "SessionEnd", `{"session_id":"`+grokSession+`","cwd":"ROOT","hook_event_name":"SessionEnd"}`, scriptHub{})
	if out.Exit != 0 {
		t.Fatal(out)
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatalf("an end with no process identity removed the file: %v", err)
	}
	out = grokRun(t, env, anc, "SessionEnd", `{"session_id":"other-session","cwd":"ROOT","hook_event_name":"SessionEnd"}`, scriptHub{})
	if out.Exit != 0 {
		t.Fatal(out)
	}
	if got, _ := os.ReadFile(file); strings.TrimSpace(string(got)) != grokSession {
		t.Fatalf("a different session removed the file: %q", got)
	}
	// No environment means no directory to clean, so the file stays.
	out = grokRun(t, nil, anc, "SessionEnd", `{"session_id":"`+grokSession+`","cwd":"ROOT","hook_event_name":"SessionEnd"}`, scriptHub{})
	if out.Exit != 0 {
		t.Fatal(out)
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatalf("an end with no environment removed the file: %v", err)
	}
}

func TestGrokSessionProtectionAndUnreadableFiles(t *testing.T) {
	orig := protectPath
	t.Cleanup(func() { protectPath = orig })
	env := grokGetenv(t, nil)
	anc := grokAncestor(3, 4, "grok")
	protectPath = func(string) error { return errors.New("acl") }
	out := grokRun(t, env, anc, "SessionStart", `{"session_id":"`+grokSession+`","cwd":"ROOT","hook_event_name":"SessionStart"}`, scriptHub{})
	if out.Exit != 0 || !strings.Contains(string(out.Stdout), "Iris handle:") {
		t.Fatal(out)
	}
	dir, _ := grokPIDDirectory(env)
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("protection failure left %v", entries)
	}

	protectPath = orig
	grokRun(t, env, anc, "SessionStart", `{"session_id":"`+grokSession+`","cwd":"ROOT","hook_event_name":"SessionStart"}`, scriptHub{})
	file := grokPIDFile(dir, 3, 4)
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(file, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := forgetGrokSession(dir, 3, 4, grokSession); err == nil {
		t.Fatal("a directory where the pid file should be was treated as a session")
	}
	if info, err := os.Stat(file); err != nil || !info.IsDir() {
		t.Fatalf("forget removed a directory it could not read: %v %v", info, err)
	}
	out = grokRun(t, env, anc, "SessionEnd", `{"session_id":"`+grokSession+`","cwd":"ROOT","hook_event_name":"SessionEnd"}`, scriptHub{})
	if out.Exit != 0 {
		t.Fatal(out)
	}

	if err := os.WriteFile(filepath.Join(dir, "3-5"), []byte("not a session\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := ReadGrokSession(dir, 3, 5); ok {
		t.Fatal("an invalid line was returned as a session id")
	}
	long := strings.Repeat("a", 129)
	if err := os.WriteFile(filepath.Join(dir, "3-6"), []byte(long+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := ReadGrokSession(dir, 3, 6); ok {
		t.Fatal("an overlong line was returned as a session id")
	}
	if err := os.WriteFile(filepath.Join(dir, "3-7"), []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := ReadGrokSession(dir, 3, 7); ok {
		t.Fatal("a blank line was a session id")
	}
	if _, ok := ReadGrokSession("", 3, 4); ok {
		t.Fatal("an empty directory was a session")
	}
	if _, ok := ReadGrokSession(dir, 0, 4); ok {
		t.Fatal("pid 0 was a session")
	}
	if _, ok := ReadGrokSession(dir, 3, 0); ok {
		t.Fatal("a missing start time was a session")
	}
	if err := forgetGrokSession(dir, 99, 99, grokSession); err != nil {
		t.Fatal(err)
	}
	if err := writeGrokSession(dir, 8, 9, "ab.cd_ef:gh"); err != nil {
		t.Fatal(err)
	}
	if got, ok := ReadGrokSession(dir, 8, 9); !ok || got != "ab.cd_ef:gh" {
		t.Fatalf("punctuation in a session id: %q %v", got, ok)
	}

	if runtime.GOOS == "windows" {
		// A mode of 0500 does not stop CreateTemp on Windows. The refusal still has
		// to be handled, so the test supplies the error the OS will not.
		origCreate := createGrokTemp
		createGrokTemp = func(string, string) (*os.File, error) {
			return nil, errors.New("access denied")
		}
		if err := writeGrokSession(t.TempDir(), 1, 2, grokSession); err == nil {
			t.Fatal("wrote when the temporary file could not be created")
		}
		createGrokTemp = origCreate
	} else if os.Geteuid() != 0 {
		locked := filepath.Join(t.TempDir(), "locked")
		if err := os.Mkdir(locked, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })
		if err := writeGrokSession(locked, 1, 2, grokSession); err == nil {
			t.Fatal("wrote into a directory the process cannot create a file in")
		}
	}
	blocked := filepath.Join(dir, "1-2")
	if err := os.Mkdir(blocked, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeGrokSession(dir, 1, 2, grokSession); err == nil {
		t.Fatal("renamed a session onto a directory")
	}
	if matches, _ := filepath.Glob(filepath.Join(dir, ".tmp-*")); len(matches) != 0 {
		t.Fatalf("rename failure left %v", matches)
	}
	n := 0
	protectPath = func(string) error {
		n++
		if n >= 2 {
			return errors.New("acl")
		}
		return nil
	}
	if err := writeGrokSession(dir, 1, 3, grokSession); err == nil {
		t.Fatal("file protection failure was stored")
	}
	if _, err := os.Stat(grokPIDFile(dir, 1, 3)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if matches, _ := filepath.Glob(filepath.Join(dir, ".tmp-*")); len(matches) != 0 {
		t.Fatalf("protection failure left %v", matches)
	}
}

func TestGrokWatchEnvReadsTheNearestGrokAndDoesNotOverride(t *testing.T) {
	env := grokGetenv(t, nil)
	dir, err := grokPIDDirectory(env)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeGrokSession(dir, 11, 12, grokSession); err != nil {
		t.Fatal(err)
	}
	nearest := grokAncestor(11, 12, `C:\Tools\grok.exe`)
	got := GrokSessionEnv(env, nearest)("GROK_SESSION_ID")
	if got != grokSession {
		t.Fatalf("watch env %q", got)
	}
	held := GrokSessionEnv(func(key string) string {
		if key == "GROK_SESSION_ID" {
			return "from-env"
		}
		return env(key)
	}, nearest)("GROK_SESSION_ID")
	if held != "from-env" {
		t.Fatalf("environment was overridden: %q", held)
	}
	if got := GrokSessionEnv(env, grokAncestor(11, 99, "grok"))("GROK_SESSION_ID"); got != "" {
		t.Fatalf("mismatched start time returned %q", got)
	}
	if got := GrokSessionEnv(env, grokAncestor(11, 12, "bash"))("GROK_SESSION_ID"); got != "" {
		t.Fatalf("a shell returned %q", got)
	}
	if got := GrokSessionEnv(env, grokAncestor(0, 12, "grok"))("GROK_SESSION_ID"); got != "" {
		t.Fatalf("pid 0 returned %q", got)
	}
	if got := GrokSessionEnv(env, grokAncestor(11, 0, "grok"))("GROK_SESSION_ID"); got != "" {
		t.Fatalf("start time 0 returned %q", got)
	}
	if got := GrokSessionEnv(env, func() (proc.Ancestor, error) { return proc.Ancestor{}, errors.New("none") })("GROK_SESSION_ID"); got != "" {
		t.Fatal(got)
	}
	if got := GrokSessionEnv(env, nil)("GROK_SESSION_ID"); got != "" {
		t.Fatal(got)
	}
	if got := GrokSessionEnv(env, nearest)("OTHER"); got != "" {
		t.Fatal(got)
	}
	if got := GrokSessionEnv(nil, nil)("PATH"); got == "" || got != os.Getenv("PATH") {
		t.Fatalf("nil getenv %q", got)
	}
	if dir, err := grokPIDDirectory(nil); dir != "" || err != nil {
		t.Fatalf("nil getenv directory %q %v", dir, err)
	}
}

func TestGrokWatchEnvWithoutATokenPathReadsNothing(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("USERPROFILE", "")
	// Windows os.UserConfigDir reads AppData, so clearing the Unix homes is not enough.
	t.Setenv("AppData", "")
	env := func(string) string { return "" }
	got := GrokSessionEnv(env, grokAncestor(1, 2, "grok"))("GROK_SESSION_ID")
	if got != "" {
		t.Fatalf("unresolved token path returned %q", got)
	}
	out := grokRun(t, env, grokAncestor(1, 2, "grok"), "SessionStart", `{"session_id":"`+grokSession+`","cwd":"ROOT","hook_event_name":"SessionStart"}`, scriptHub{})
	if out.Exit != 0 || !strings.Contains(string(out.Stdout), "systemMessage") {
		t.Fatal(out)
	}
}

func TestGrokStopAsksUntilAMonitorExists(t *testing.T) {
	env := grokGetenv(t, nil)
	block := grokBlock(grokWatchReason)
	cases := []struct {
		name   string
		body   string
		ask    bool
		env    func(string) string
		client string
	}{
		{"end turn", `{"session_id":"s","cwd":"ROOT","hook_event_name":"Stop","reason":"end_turn","stopHookActive":false,"backgroundTasks":[]}`, true, env, "grok"},
		{"captured fixture", "", true, env, "grok"},
		{"shell is not a monitor", `{"session_id":"s","cwd":"ROOT","reason":"end_turn","backgroundTasks":[{"type":"shell","description":"iris watch --client grok"}]}`, true, env, "grok"},
		{"monitor type is enough", `{"session_id":"s","cwd":"ROOT","reason":"end_turn","backgroundTasks":[{"type":"shell"},{"type":"monitor","description":"[monitor] something"}]}`, false, env, "grok"},
		{"description is not proof", `{"session_id":"s","cwd":"ROOT","reason":"end_turn","backgroundTasks":[{"type":"Monitor","description":"[monitor] iris watch --client grok"}]}`, true, env, "grok"},
		{"snake case monitor", `{"session_id":"s","cwd":"ROOT","reason":"end_turn","background_tasks":[{"type":"monitor"}]}`, false, env, "grok"},
		{"stop hook already active", `{"session_id":"s","cwd":"ROOT","reason":"end_turn","stop_hook_active":true,"backgroundTasks":[]}`, false, env, "grok"},
		{"camel active", `{"session_id":"s","cwd":"ROOT","reason":"end_turn","stopHookActive":true}`, false, env, "grok"},
		{"shutdown fixture", "", false, env, "grok"},
		{"channel closed", `{"session_id":"s","cwd":"ROOT","reason":"channel_closed"}`, false, env, "grok"},
		{"missing reason", `{"session_id":"s","cwd":"ROOT","hook_event_name":"Stop"}`, false, env, "grok"},
		{"malformed tasks", `{"session_id":"s","cwd":"ROOT","reason":"end_turn","backgroundTasks":{"type":"monitor"}}`, false, env, "grok"},
		{"malformed snake tasks", `{"session_id":"s","cwd":"ROOT","reason":"end_turn","backgroundTasks":[],"background_tasks":"nope"}`, false, env, "grok"},
		{"null tasks", `{"session_id":"s","cwd":"ROOT","reason":"end_turn","backgroundTasks":null}`, true, env, "grok"},
		{"outside a repo", `{"session_id":"s","cwd":"/definitely/not/a/repo","reason":"end_turn","backgroundTasks":[]}`, true, env, "grok"},
		{"other client", `{"session_id":"s","cwd":"ROOT","hook_event_name":"Stop","reason":"end_turn","backgroundTasks":[]}`, false, env, "claude"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := tc.body
			event := "Stop"
			if tc.name == "captured fixture" {
				raw, err := os.ReadFile("testdata/grok/Stop.json")
				if err != nil {
					t.Fatal(err)
				}
				body = string(raw)
			}
			if tc.name == "shutdown fixture" {
				raw, err := os.ReadFile("testdata/grok/Stop-shutdown.json")
				if err != nil {
					t.Fatal(err)
				}
				body = string(raw)
			}
			var calls int
			out := Runner{
				ClientName: tc.client,
				EventName:  event,
				Stdin:      strings.NewReader(strings.ReplaceAll(body, "ROOT", filepath.ToSlash(t.TempDir()))),
				Hub:        &countingHub{fn: func() { calls++ }},
				Getenv:     tc.env,
				after:      never,
				logAfter:   never,
			}.Run()
			if calls != 0 {
				t.Fatalf("Stop called the hub %d times", calls)
			}
			if tc.ask {
				if out.Exit != block.Exit || string(out.Stdout) != string(block.Stdout) {
					t.Fatalf("stdout %q exit %d", out.Stdout, out.Exit)
				}
				return
			}
			if out.Exit != 0 || string(out.Stdout) != "{}\n" {
				t.Fatalf("stdout %q exit %d", out.Stdout, out.Exit)
			}
		})
	}
}

func TestGrokStopFollowsTheLocalWakeMarker(t *testing.T) {
	body := `{"session_id":"s","cwd":"/definitely/not/a/repo","reason":"end_turn","backgroundTasks":[]}`
	run := func(env func(string) string) Output {
		t.Helper()
		return Runner{ClientName: "grok", EventName: "Stop", Stdin: strings.NewReader(body), Getenv: env, after: never, logAfter: never}.Run()
	}
	home := t.TempDir()
	off := filepath.Join(home, "off")
	if err := os.MkdirAll(off, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(off, "iris-wake"), []byte("off\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out := run(grokGetenv(t, map[string]string{"GROK_HOME": off})); out.Exit != 0 || string(out.Stdout) != "{}\n" {
		t.Fatalf("off marker still asked: %q", out.Stdout)
	}
	on := filepath.Join(home, "on")
	if err := os.MkdirAll(on, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(on, "iris-wake"), []byte("on\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out := run(grokGetenv(t, map[string]string{"GROK_HOME": on, "HOME": off})); !strings.Contains(string(out.Stdout), `"decision":"block"`) {
		t.Fatalf("GROK_HOME on did not ask: %q", out.Stdout)
	}
	profile := filepath.Join(home, "profile")
	if err := os.MkdirAll(filepath.Join(profile, ".grok"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(profile, ".grok", "iris-wake"), []byte("off\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out := run(grokGetenv(t, map[string]string{"USERPROFILE": profile})); string(out.Stdout) != "{}\n" {
		t.Fatalf("USERPROFILE off still asked: %q", out.Stdout)
	}
	if out := run(grokGetenv(t, map[string]string{})); !strings.Contains(string(out.Stdout), `"decision":"block"`) {
		t.Fatalf("no home did not ask: %q", out.Stdout)
	}
	unixHome := filepath.Join(home, "unix")
	if err := os.MkdirAll(filepath.Join(unixHome, ".grok"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unixHome, ".grok", "iris-wake"), []byte("off\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out := run(grokGetenv(t, map[string]string{"HOME": unixHome})); string(out.Stdout) != "{}\n" {
		t.Fatalf("HOME off still asked: %q", out.Stdout)
	}
	empty := filepath.Join(home, "empty")
	if err := os.MkdirAll(empty, 0o700); err != nil {
		t.Fatal(err)
	}
	if out := run(grokGetenv(t, map[string]string{"GROK_HOME": empty})); !strings.Contains(string(out.Stdout), `"decision":"block"`) {
		t.Fatalf("missing marker did not ask: %q", out.Stdout)
	}
	broken := filepath.Join(home, "broken")
	if err := os.MkdirAll(filepath.Join(broken, "iris-wake"), 0o700); err != nil {
		t.Fatal(err)
	}
	if out := run(grokGetenv(t, map[string]string{"GROK_HOME": broken})); string(out.Stdout) != "{}\n" {
		t.Fatalf("unreadable marker still asked: %q", out.Stdout)
	}
	other := filepath.Join(home, "other")
	if err := os.MkdirAll(other, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, "iris-wake"), []byte("OFF\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out := run(grokGetenv(t, map[string]string{"GROK_HOME": other})); !strings.Contains(string(out.Stdout), `"decision":"block"`) {
		t.Fatalf("non-off marker did not ask: %q", out.Stdout)
	}

	t.Setenv("GROK_HOME", off)
	if out := run(nil); string(out.Stdout) != "{}\n" {
		t.Fatalf("nil getenv ignored GROK_HOME: %q", out.Stdout)
	}
}

func TestCapturedGrokStopDecidesFromThePayload(t *testing.T) {
	end, err := os.ReadFile("testdata/grok/Stop.json")
	if err != nil {
		t.Fatal(err)
	}
	ev, err := Decode(ClientGrok, "Stop", end)
	if err != nil || ev.GrokStop == nil || !ev.GrokStop.arm() || ev.GrokStop.Reason != "end_turn" || ev.GrokStop.HookActive || ev.GrokStop.HasMonitor {
		t.Fatalf("%+v %v", ev.GrokStop, err)
	}
	shut, err := os.ReadFile("testdata/grok/Stop-shutdown.json")
	if err != nil {
		t.Fatal(err)
	}
	ev, err = Decode(ClientGrok, "Stop", shut)
	if err != nil || ev.GrokStop == nil || ev.GrokStop.arm() || ev.GrokStop.Reason != "shutdown" {
		t.Fatalf("%+v %v", ev.GrokStop, err)
	}
}
