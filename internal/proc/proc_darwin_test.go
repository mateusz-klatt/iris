package proc

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestLookupReadsThisProcess(t *testing.T) {
	before := time.Now().UnixMilli()
	got, ok := lookupOS(os.Getpid())
	if !ok || got.PID != os.Getpid() || got.PPID != os.Getppid() {
		t.Fatalf("%+v %v", got, ok)
	}
	if want := filepath.Base(os.Args[0]); got.Name != want {
		t.Fatalf("%q, want %q", got.Name, want)
	}
	if got.StartMS <= 0 || got.StartMS > before || got.Exe == "" {
		t.Fatalf("started at %d, now %d, exe %q", got.StartMS, before, got.Exe)
	}
}

func TestLookupKeepsTheKernelNameWhenArgumentsAreRefused(t *testing.T) {
	got, ok := lookupOS(1)
	if !ok || got.Name != "launchd" || got.PPID != 0 {
		t.Fatalf("%+v %v", got, ok)
	}
}

// startNamed runs sleep with a chosen argv[0], which is how a client that retitles itself appears.
func startNamed(t *testing.T, argv0 string) int {
	t.Helper()
	cmd := &exec.Cmd{Path: "/bin/sleep", Args: []string{argv0, "30"}}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return cmd.Process.Pid
}

func TestLookupNamesAClientFromItsFirstArgument(t *testing.T) {
	for argv0, want := range map[string]string{
		"claude":              "claude",
		"/opt/bin/Claude":     "claude",
		"not-a-client":        "sleep",
		"-zsh":                "sleep",
		"/usr/local/bin/node": "sleep",
	} {
		pid := startNamed(t, argv0)
		got, ok := lookupOS(pid)
		if !ok || got.Name != want {
			t.Fatalf("argv[0] %q: %+v %v, want %q", argv0, got, ok, want)
		}
	}
}

func TestProcargsLayoutReadsValidatedArguments(t *testing.T) {
	for name, tc := range map[string]struct {
		raw   string
		exe   string
		argv0 string
		valid bool
	}{
		"padded":    {"\x02\x00\x00\x00/path/2.1.319\x00\x00\x00\x00claude\x00--flag\x00", "/path/2.1.319", "claude", true},
		"unpadded":  {"\x01\x00\x00\x00/bin/sh\x00sh\x00", "/bin/sh", "sh", true},
		"no end":    {"\x01\x00\x00\x00/bin/sh\x00agent", "/bin/sh", "", false},
		"argc zero": {"\x00\x00\x00\x00/bin/sh\x00\x00agent\x00", "/bin/sh", "", false},
		"path only": {"\x00\x00\x00\x00/bin/sh\x00", "/bin/sh", "", false},
		"cut path":  {"\x01\x00\x00\x00/bin", "", "", false},
		"short":     {"\x01\x00", "", "", false},
		"empty":     {"", "", "", false},
	} {
		if got := executablePath([]byte(tc.raw)); got != tc.exe {
			t.Fatalf("%s exe: %q, want %q", name, got, tc.exe)
		}
		args, err := procargsArgs([]byte(tc.raw))
		if tc.valid && (err != nil || len(args) == 0 || args[0] != tc.argv0) {
			t.Fatalf("%s args: %v %v, want %q", name, args, err, tc.argv0)
		}
		if !tc.valid && err == nil {
			t.Fatalf("%s args: unexpected success %v", name, args)
		}
	}
}

func TestProcessNamePromotesTheAgentAlias(t *testing.T) {
	node := "/Users/user/.local/share/cursor-agent/versions/2026.11.22-e9042b2/node"
	index := "/Users/user/.local/share/cursor-agent/versions/2026.11.22-e9042b2/index.js"
	grok := "/Users/user/.grok/downloads/grok-1.0.92-macos-x86_64"
	cursorRaw := procargsBuffer(node, "/Users/user/.local/bin/agent", "--disable-sigusr1", "--use-system-ca", "--max-old-space-size=4096", index, "-p", "hi")
	name, exe, launcher := identityFromProcargs("node", cursorRaw)
	if name != "cursor-agent" || exe != node || launcher != index {
		t.Fatalf("cursor promotion: %q %q %q", name, exe, launcher)
	}
	systemNodeRaw := procargsBuffer("/usr/local/bin/node", "/Users/user/.local/bin/agent", "--disable-sigusr1", "--use-system-ca", index)
	name, exe, launcher = identityFromProcargs("node", systemNodeRaw)
	if name != "cursor-agent" || exe != "/usr/local/bin/node" || launcher != index {
		t.Fatalf("system node cursor promotion: %q %q %q", name, exe, launcher)
	}
	cursorAgentName, _, _ := identityFromProcargs("node", procargsBuffer(node, "/Users/user/.local/bin/cursor-agent", "--use-system-ca", index))
	if cursorAgentName != "cursor-agent" {
		t.Fatalf("cursor-agent symlink: %q", cursorAgentName)
	}
	grokName, grokExe, grokLauncher := identityFromProcargs("node", procargsBuffer(grok, "agent"))
	if grokName != "grok" || grokExe != grok || grokLauncher != "" {
		t.Fatalf("grok promotion: %q %q %q", grokName, grokExe, grokLauncher)
	}
	grokSymlinkName, grokSymlinkExe, _ := identityFromProcargs("grok-1.0.92-mac", procargsBuffer("/Users/user/.grok/bin/agent", "/Users/user/.grok/bin/agent"))
	if grokSymlinkName != "grok" || grokSymlinkExe != "/Users/user/.grok/bin/agent" {
		t.Fatalf("grok bin/agent symlink promotion: %q %q", grokSymlinkName, grokSymlinkExe)
	}
	grokNamed, _, _ := identityFromProcargs("grok-1.0.92-mac", procargsBuffer(grok, "/Users/user/.grok/bin/grok"))
	if grokNamed != "grok" {
		t.Fatalf("grok symlink: %q", grokNamed)
	}
	unknownName, unknownExe, _ := identityFromProcargs("node", procargsBuffer("/bin/sleep", "/Users/user/.local/bin/agent", "30"))
	if unknownName != "agent" || unknownExe != "/bin/sleep" {
		t.Fatalf("unidentified agent: %q %q", unknownName, unknownExe)
	}
	// Unterminated argv[0] or argc == 0 keeps p_comm rather than promoting an unvalidated "agent",
	// while keeping the executable path when it is independently valid.
	unterminated := procargsBuffer("/bin/sh", "agent")
	unterminated = unterminated[:len(unterminated)-1]
	kept, keptExe, keptLauncher := identityFromProcargs("sleep", unterminated)
	if kept != "sleep" || keptExe != "/bin/sh" || keptLauncher != "" {
		t.Fatalf("unterminated argv[0]: %q %q %q", kept, keptExe, keptLauncher)
	}
	zeroArgc := append([]byte{0, 0, 0, 0}, "/bin/sh\x00\x00agent\x00"...)
	kept, keptExe, keptLauncher = identityFromProcargs("sleep", zeroArgc)
	if kept != "sleep" || keptExe != "/bin/sh" || keptLauncher != "" {
		t.Fatalf("argc zero with agent: %q %q %q", kept, keptExe, keptLauncher)
	}
	zeroWithCursorNode := append([]byte{0, 0, 0, 0}, append([]byte(node), "\x00agent\x00"...)...)
	kept, keptExe, keptLauncher = identityFromProcargs("node", zeroWithCursorNode)
	if kept != "cursor-agent" || keptExe != node || keptLauncher != "" {
		t.Fatalf("argc zero with valid cursor exe: %q %q %q", kept, keptExe, keptLauncher)
	}
	tree := map[int]Snapshot{
		10: {PID: 10, PPID: 9, Name: "iris"},
		9:  {PID: 9, PPID: 2, Name: name, Exe: exe, Launcher: launcher},
		2:  {PID: 2, PPID: 1, Name: "claude"},
	}
	got, ok := NearestFrom(10, func(pid int) (Snapshot, bool) {
		s, ok := tree[pid]
		return s, ok
	}, nil)
	if !ok || got.Name != "cursor-agent" {
		t.Fatalf("promoted cursor did not stop before Claude: %+v %v", got, ok)
	}
	if client, known := ClientNameOf(got, nil); !known || client != "cursor" {
		t.Fatalf("promoted client %q %v", client, known)
	}
}

func TestProcessNameKeepsAnUnidentifiedAgentVisible(t *testing.T) {
	pid := startNamed(t, "agent")
	got, ok := lookupOS(pid)
	if !ok || got.Name != "agent" {
		t.Fatalf("p_comm replaced an unidentified agent: %+v %v", got, ok)
	}
	if client, known := ClientNameOf(got, nil); known {
		t.Fatalf("unidentified agent became %s", client)
	}
}

func procargsBuffer(exe string, args ...string) []byte {
	buf := []byte{byte(len(args)), 0, 0, 0}
	buf = append(buf, exe...)
	buf = append(buf, 0)
	for _, arg := range args {
		buf = append(buf, arg...)
		buf = append(buf, 0)
	}
	return buf
}

func TestExecutableOSReadsThisProcessAndRefusesKernelProcesses(t *testing.T) {
	if got, err := executableOS(os.Getpid(), Snapshot{}); err != nil || got == "" {
		t.Fatalf("got %q, %v", got, err)
	}
	if _, err := executableOS(1, Snapshot{}); err == nil {
		t.Fatal("expected kernel refusal for launchd")
	}
}
