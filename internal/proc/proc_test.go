package proc

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// A process that has exited is not running, even before its parent collects it: a zombie on
// Linux and macOS, and on Windows a process object that the parent's handle keeps. The child is
// this test binary running no tests, and it is collected only after the check.
func TestAnExitedProcessIsNotRunningBeforeItIsCollected(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	child := exec.Command(self, "-test.run=^$")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = child.Wait() }()
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if _, ok := Lookup(child.Process.Pid); !ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("an exited child that was not collected still counted as running")
		}
	}
}

func TestNearestFrom(t *testing.T) {
	tree := map[int]Snapshot{
		10: {PID: 10, PPID: 9, Name: "iris"},
		9:  {PID: 9, PPID: 8, Name: "/bin/bash"},
		8:  {PID: 8, PPID: 7, Name: "python3"},
		7:  {PID: 7, PPID: 2, Name: "grok"},
		2:  {PID: 2, PPID: 1, Name: "grok"},
		6:  {PID: 6, PPID: 5, Name: "iris"},
		5:  {PID: 5, PPID: 4, Name: `C:\Windows\System32\cmd.exe`},
		4:  {PID: 4, PPID: 1, Name: `C:\Tools\cursor-agent.exe`},
		3:  {PID: 3, PPID: 1, Name: "claude"},
	}
	get := func(pid int) (Snapshot, bool) {
		s, ok := tree[pid]
		return s, ok
	}
	got, ok := NearestFrom(10, get, nil)
	if !ok || got.PID != 7 || got.Name != "grok" {
		t.Fatalf("past a shell and a helper = %+v ok=%v", got, ok)
	}
	got, ok = NearestFrom(7, get, nil)
	if !ok || got.PID != 2 {
		t.Fatalf("stop at the first client = %+v", got)
	}
	got, ok = NearestFrom(6, get, nil)
	if !ok || got.PID != 4 {
		t.Fatalf("windows shell = %+v", got)
	}
	got, ok = NearestFrom(3, get, nil)
	if ok {
		t.Fatalf("init is not an ancestor worth recording, got %+v", got)
	}

	onlyHelper := map[int]Snapshot{
		4: {PID: 4, PPID: 3, Name: "iris"},
		3: {PID: 3, PPID: 1, Name: "python3"},
	}
	got, ok = NearestFrom(4, func(pid int) (Snapshot, bool) {
		s, ok := onlyHelper[pid]
		return s, ok
	}, nil)
	if !ok || got.Name != "python3" {
		t.Fatalf("fallback nearest non-shell = %+v", got)
	}

	agentTree := map[int]Snapshot{
		20: {PID: 20, PPID: 19, Name: "iris"},
		19: {PID: 19, PPID: 18, Name: "bash"},
		18: {PID: 18, PPID: 17, Name: "/opt/agent"},
		17: {PID: 17, PPID: 1, Name: "claude"},
	}
	agentGet := func(pid int) (Snapshot, bool) {
		s, ok := agentTree[pid]
		return s, ok
	}
	cursorEnv := func(key string) string {
		return map[string]string{"CURSOR_PROJECT_DIR": "/project", "CURSOR_VERSION": "2026.11.22"}[key]
	}
	got, ok = NearestFrom(20, agentGet, cursorEnv)
	if !ok || got.PID != 18 || got.Name != "/opt/agent" {
		t.Fatalf("Cursor's agent alias did not stop before outer Claude: %+v, %v", got, ok)
	}
	got, ok = NearestFrom(20, agentGet, nil)
	if !ok || got.PID != 18 || got.Name != "/opt/agent" {
		t.Fatalf("unidentified agent did not stop before outer Claude: %+v, %v", got, ok)
	}
	if client, known := ClientNameOf(got, nil); known {
		t.Fatalf("unidentified agent was assigned %s", client)
	}
}

func TestNearestFromResolvesMeasuredAgentAliases(t *testing.T) {
	cursorExe := "/home/user/.local/share/cursor-agent/versions/2026.11.22-e9042b2/node"
	cursorIndex := "/home/user/.local/share/cursor-agent/versions/2026.11.22-e9042b2/index.js"
	cursorScript := "/home/user/.local/share/cursor-agent/versions/2026.11.22-e9042b2/cursor-agent"
	grokExe := "/home/user/.grok/downloads/grok-1.0.92-linux-x86_64"
	for _, tc := range []struct {
		name   string
		agent  Snapshot
		client string
		known  bool
	}{
		{name: "grok executable", agent: Snapshot{PID: 18, PPID: 17, Name: "agent", Exe: grokExe}, client: "grok", known: true},
		{name: "grok .grok symlink target", agent: Snapshot{PID: 18, PPID: 17, Name: "/Users/user/.local/bin/agent", Exe: "/Users/user/.grok/bin/agent"}, client: "grok", known: true},
		{name: "cursor node and index.js", agent: Snapshot{PID: 18, PPID: 17, Name: "agent", Exe: cursorExe, Launcher: cursorIndex}, client: "cursor", known: true},
		{name: "cursor index.js launcher only", agent: Snapshot{PID: 18, PPID: 17, Name: "/Users/user/.local/bin/agent", Exe: "/usr/local/bin/node", Launcher: cursorIndex}, client: "cursor", known: true},
		{name: "cursor-agent executable", agent: Snapshot{PID: 18, PPID: 17, Name: "agent", Exe: cursorScript}, client: "cursor", known: true},
		{name: "unidentified agent", agent: Snapshot{PID: 18, PPID: 17, Name: "/opt/agent"}},
		{name: "cursor-agent unchanged", agent: Snapshot{PID: 18, PPID: 17, Name: "cursor-agent"}, client: "cursor", known: true},
		{name: "grok unchanged", agent: Snapshot{PID: 18, PPID: 17, Name: "grok"}, client: "grok", known: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tree := map[int]Snapshot{
				20: {PID: 20, PPID: 19, Name: "iris"},
				19: {PID: 19, PPID: 18, Name: "bash"},
				18: tc.agent,
				17: {PID: 17, PPID: 1, Name: "claude"},
			}
			got, ok := NearestFrom(20, func(pid int) (Snapshot, bool) {
				s, ok := tree[pid]
				return s, ok
			}, nil)
			if !ok || got.PID != 18 {
				t.Fatalf("walk %+v %v", got, ok)
			}
			client, known := ClientNameOf(got, nil)
			if client != tc.client || known != tc.known {
				t.Fatalf("client %q known %v", client, known)
			}
		})
	}
}

func TestAgentAliasEvidence(t *testing.T) {
	cursorNode := "/home/user/.local/share/cursor-agent/versions/2026.11.22-e9042b2/node"
	macCursorNode := "/Users/user/.local/share/cursor-agent/versions/2026.11.22-e9042b2/node"
	cursorIndex := "/home/user/.local/share/cursor-agent/versions/2026.11.22-e9042b2/index.js"
	macCursorIndex := "/Users/user/.local/share/cursor-agent/versions/2026.11.22-e9042b2/index.js"
	cursorScript := "/home/user/.local/share/cursor-agent/versions/2026.11.22-e9042b2/cursor-agent"
	grokExe := "/home/user/.grok/downloads/grok-1.0.92-linux-x86_64"
	macGrokExe := "/Users/user/.grok/downloads/grok-1.0.92-macos-x86_64"
	windowsNode := `C:\Users\me\AppData\Local\cursor-agent\versions\2026.11.22-e9042b2\node.exe`
	windowsGrok := `C:\Users\me\.grok\downloads\grok-1.0.92-windows-x86_64.exe`
	windowsAgent := `C:\Tools\agent.exe`
	cursorEnv := func(key string) string {
		return map[string]string{"CURSOR_PROJECT_DIR": "/project", "CURSOR_VERSION": "2026.11.22"}[key]
	}
	for _, tc := range []struct {
		name     string
		snap     Snapshot
		getenv   func(string) string
		client   string
		known    bool
		visible  string
		promoted string
		comm     string
	}{
		{
			name: "versioned grok executable", snap: Snapshot{Name: "agent", Exe: grokExe},
			client: "grok", known: true, visible: "grok", promoted: "grok", comm: "node",
		},
		{
			name: "macos grok executable is not promoted to cursor", snap: Snapshot{Name: "/Users/user/.local/bin/agent", Exe: macGrokExe},
			client: "grok", known: true, visible: "grok", promoted: "grok", comm: "grok-1.0.92-mac",
		},
		{
			name: "grok-* basename outside .grok", snap: Snapshot{Name: "agent", Exe: "/opt/bin/grok-canary"},
			client: "grok", known: true, visible: "grok", promoted: "grok", comm: "grok-canary",
		},
		{
			name: "grok symlink target under .grok", snap: Snapshot{Name: "agent", Exe: "/Users/user/.grok/bin/agent"},
			client: "grok", known: true, visible: "grok", promoted: "grok", comm: "agent",
		},
		{
			name: "grok symlink path under .grok", snap: Snapshot{Name: "/home/user/.grok/bin/agent"},
			client: "grok", known: true, visible: "grok", promoted: "grok", comm: "node",
		},
		{
			name: "windows grok image", snap: Snapshot{Name: windowsGrok, Exe: windowsGrok},
			client: "grok", known: true, visible: "grok", promoted: "grok", comm: "node",
		},
		{
			name: "deleted grok executable still matches", snap: Snapshot{Name: "agent", Exe: grokExe + " (deleted)"},
			client: "grok", known: true, visible: "grok", promoted: "grok", comm: "node",
		},
		{
			name: "cursor node", snap: Snapshot{Name: "agent", Exe: cursorNode},
			client: "cursor", known: true, visible: "cursor-agent", promoted: "cursor-agent", comm: "node",
		},
		{
			name: "macos full-path argv0 with cursor node", snap: Snapshot{Name: "/Users/user/.local/bin/agent", Exe: macCursorNode},
			client: "cursor", known: true, visible: "cursor-agent", promoted: "cursor-agent", comm: "node",
		},
		{
			name: "cursor index.js with a system node", snap: Snapshot{Name: "agent", Exe: "/usr/bin/node", Launcher: cursorIndex},
			client: "cursor", known: true, visible: "cursor-agent", promoted: "cursor-agent", comm: "node",
		},
		{
			name: "cursor-agent executable", snap: Snapshot{Name: "agent", Exe: cursorScript},
			client: "cursor", known: true, visible: "cursor-agent", promoted: "cursor-agent", comm: "node",
		},
		{
			name: "windows cursor node image", snap: Snapshot{Name: windowsNode, Exe: windowsNode},
			client: "cursor", known: true, visible: "cursor-agent", promoted: "cursor-agent", comm: "node",
		},
		{
			name: "system node is not cursor", snap: Snapshot{Name: "node", Exe: "/usr/bin/node"},
			visible: "node", promoted: "sleep", comm: "sleep",
		},
		{
			name:    "node directly under cursor-agent/versions without version segment is not cursor",
			snap:    Snapshot{Name: "agent", Exe: "/home/user/.local/share/cursor-agent/versions/node"},
			visible: "agent", promoted: "agent", comm: "node",
		},
		{
			name: "index.js outside the launcher", snap: Snapshot{Name: "agent", Launcher: "/tmp/index.js"},
			visible: "agent", promoted: "agent", comm: "node",
		},
		{
			name: "index.js missing version segment", snap: Snapshot{Name: "agent", Launcher: "/home/user/.local/share/cursor-agent/versions/index.js"},
			visible: "agent", promoted: "agent", comm: "node",
		},
		{
			name: "unrelated full-path agent", snap: Snapshot{Name: "/Users/user/.local/bin/agent", Exe: "/opt/unrelated/agent"},
			visible: "/Users/user/.local/bin/agent", promoted: "agent", comm: "sleep",
		},
		{
			name: "bare windows agent image", snap: Snapshot{Name: windowsAgent, Exe: windowsAgent},
			visible: windowsAgent, promoted: "agent", comm: "node",
		},
		{
			name: "markers do not relabel grok", snap: Snapshot{Name: "agent", Exe: grokExe}, getenv: cursorEnv,
			client: "grok", known: true, visible: "grok", promoted: "grok", comm: "node",
		},
		{
			name: "markers do not relabel grok .grok symlink target", snap: Snapshot{Name: "/Users/user/.local/bin/agent", Exe: "/Users/user/.grok/bin/agent"}, getenv: cursorEnv,
			client: "grok", known: true, visible: "grok", promoted: "grok", comm: "agent",
		},
		{
			name: "markers still name an unresolved agent", snap: Snapshot{Name: "agent"}, getenv: cursorEnv,
			client: "cursor", known: true, visible: "agent", promoted: "agent", comm: "node",
		},
		{
			name: "markers name a full-path unresolved agent", snap: Snapshot{Name: "/Users/user/.local/bin/agent", Exe: "/opt/unrelated/agent"}, getenv: cursorEnv,
			client: "cursor", known: true, visible: "/Users/user/.local/bin/agent", promoted: "agent", comm: "node",
		},
		{
			name: "a named client keeps its title", snap: Snapshot{Name: "grok", Exe: cursorNode},
			client: "grok", known: true, visible: "grok", promoted: "grok", comm: "grok",
		},
		{
			name: "cursor-agent is unchanged", snap: Snapshot{Name: "cursor-agent"},
			client: "cursor", known: true, visible: "cursor-agent", promoted: "cursor-agent", comm: "cursor-agent",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, known := ClientNameOf(tc.snap, tc.getenv)
			if client != tc.client || known != tc.known {
				t.Fatalf("ClientNameOf %q %v", client, known)
			}
			visible := resolveVisibleName(tc.snap.Name, tc.snap.Exe, tc.snap.Launcher)
			if visible != tc.visible {
				t.Fatalf("visible %q", visible)
			}
			promoted := promotedProcessName(tc.comm, tc.snap.Name, tc.snap.Exe, tc.snap.Launcher)
			if promoted != tc.promoted {
				t.Fatalf("promoted %q", promoted)
			}
		})
	}
	if got := promotedProcessName("node", "", cursorNode, ""); got != "cursor-agent" {
		t.Fatalf("empty argv[0] with Cursor's node: %q", got)
	}
	if got := promotedProcessName("sleep", "", "", ""); got != "sleep" {
		t.Fatalf("empty argv[0]: %q", got)
	}
	if got := promotedProcessName("sleep", "-zsh", "/bin/zsh", ""); got != "sleep" {
		t.Fatalf("login shell: %q", got)
	}
	if got := promotedProcessName("sleep", "/opt/bin/Claude", "", ""); got != "claude" {
		t.Fatalf("client path: %q", got)
	}
	name, exe, launcher := commandIdentity("python", "agent", grokExe+" (deleted)", nil)
	if name != "grok" || exe != grokExe || launcher != "" {
		t.Fatalf("command identity grok: %q %q %q", name, exe, launcher)
	}
	name, exe, launcher = commandIdentity("bash", "", "/bin/bash", []string{"/usr/bin/index.js"})
	if name != "bash" || exe != "/bin/bash" || launcher != "" {
		t.Fatalf("command identity without argv[0]: %q %q %q", name, exe, launcher)
	}
	name, exe, launcher = commandIdentity("node", "/Users/user/.local/bin/agent", "/usr/bin/node", []string{"/Users/user/.local/bin/agent", "--use-system-ca", "--max-old-space-size=4096", macCursorIndex, "-p", "hi"})
	if name != "cursor-agent" || exe != "/usr/bin/node" || launcher != macCursorIndex {
		t.Fatalf("command identity launcher after node flags: %q %q %q", name, exe, launcher)
	}
	if underDotGrok("/home/user/.grok-backup/bin/agent") {
		t.Fatal("a directory that only begins with .grok counted")
	}
}

func TestLookupLive(t *testing.T) {
	got, ok := Lookup(os.Getpid())
	if !ok || got.PID != os.Getpid() || got.Name == "" || got.StartMS <= 0 {
		t.Fatalf("current process = %+v, ok %t", got, ok)
	}
}

func TestNearestLive(t *testing.T) {
	a, err := Nearest()
	if err != nil {
		t.Fatal(err)
	}
	if a.PID <= 1 {
		t.Fatalf("pid %d", a.PID)
	}
	if runtime.GOOS == "linux" && a.StartMS <= 0 {
		t.Fatalf("start %d", a.StartMS)
	}
}

// TestNearestFrom covers the agent tree with and without the pair; this only checks that the
// live walk without markers finds a real process, whatever ancestry runs the test.
func TestNearestWithoutMarkersLive(t *testing.T) {
	t.Setenv("CURSOR_PROJECT_DIR", "/outer/cursor/project")
	t.Setenv("CURSOR_VERSION", "2026.11.22-e9042b2")
	a, err := NearestWithoutMarkers()
	if err != nil {
		t.Fatal(err)
	}
	if a.PID <= 1 {
		t.Fatalf("pid %d", a.PID)
	}
}

func TestExecutableAndTerminateValidation(t *testing.T) {
	self, ok := Lookup(os.Getpid())
	if !ok {
		t.Fatal("self lookup")
	}
	exe, err := Executable(self.PID, self.StartMS)
	if err != nil || !filepath.IsAbs(exe) {
		t.Fatalf("Executable(self) = %q, %v", exe, err)
	}
	img, err := OpenExecutable(self.PID, self.StartMS)
	if err != nil || img.Path != exe || img.Deleted {
		t.Fatalf("OpenExecutable(self) = %+v, %v", img, err)
	}
	cmd := img.Command(context.Background(), "-test.run=^$")
	if cmd.Path != img.Path || len(cmd.ExtraFiles) != 0 {
		t.Fatalf("non-deleted Command = path %q extraFiles %d", cmd.Path, len(cmd.ExtraFiles))
	}
	if err := img.Close(); err != nil {
		t.Fatalf("Close(path-backed) = %v", err)
	}
	if err := (*ProcessExecutable)(nil).Close(); err != nil {
		t.Fatalf("Close(nil) = %v", err)
	}
	dir := t.TempDir()
	reg := filepath.Join(dir, "agy")
	if err := os.WriteFile(reg, []byte("bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Executable(0, 100); !errors.Is(err, ErrNotFound) {
		t.Fatalf("pid 0: %v", err)
	}
	if _, err := Executable(10, 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("start 0: %v", err)
	}
	if _, err := executableFrom(10, 100, func(int) (Snapshot, bool) { return Snapshot{}, false }, nil, os.Stat); !errors.Is(err, ErrNotFound) {
		t.Fatal("missing snapshot")
	}
	if _, err := executableFrom(10, 100, func(int) (Snapshot, bool) { return Snapshot{PID: 10, StartMS: 99}, true }, nil, os.Stat); !errors.Is(err, ErrNotFound) {
		t.Fatal("mismatched startMS")
	}
	live := func(int) (Snapshot, bool) { return Snapshot{PID: 10, StartMS: 100}, true }
	for name, readExe := range map[string]func(int, Snapshot) (string, error){
		"error":    func(int, Snapshot) (string, error) { return "", errors.New("denied") },
		"empty":    func(int, Snapshot) (string, error) { return "", nil },
		"deleted":  func(int, Snapshot) (string, error) { return reg + " (deleted)", nil },
		"relative": func(int, Snapshot) (string, error) { return "agy", nil },
		"dir":      func(int, Snapshot) (string, error) { return dir, nil },
		"missing":  func(int, Snapshot) (string, error) { return filepath.Join(dir, "missing"), nil },
	} {
		if _, err := executableFrom(10, 100, live, readExe, os.Stat); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	calls := 0
	reused := func(int) (Snapshot, bool) {
		calls++
		if calls > 1 {
			return Snapshot{PID: 10, StartMS: 200}, true
		}
		return Snapshot{PID: 10, StartMS: 100}, true
	}
	if _, err := executableFrom(10, 100, reused, func(int, Snapshot) (string, error) { return reg, nil }, os.Stat); !errors.Is(err, ErrNotFound) {
		t.Fatalf("PID reuse after stat must fail: %v", err)
	}
	if got, err := executableFrom(10, 100, live, func(int, Snapshot) (string, error) { return reg, nil }, os.Stat); err != nil || got != reg {
		t.Fatalf("valid regular file = %q, %v", got, err)
	}
	if err := Terminate(0, 100); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Terminate pid 0: %v", err)
	}
	if err := Terminate(self.PID, self.StartMS+1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Terminate mismatched startMS: %v", err)
	}
}
