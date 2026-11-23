package proc

import (
	"errors"
	"testing"
)

func TestNearestReportsAnEmptyWalk(t *testing.T) {
	if _, err := nearest(7, func(int) (Snapshot, bool) { return Snapshot{}, false }, nil); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	got, err := nearest(7, func(pid int) (Snapshot, bool) {
		if pid == 7 {
			return Snapshot{PID: 7, PPID: 6, Name: "iris"}, true
		}
		return Snapshot{PID: 6, PPID: 1, Name: "claude", StartMS: 9}, true
	}, nil)
	if err != nil || got != (Ancestor{PID: 6, PPID: 1, Name: "claude", StartMS: 9}) {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestNearestFromStopsAtBrokenChains(t *testing.T) {
	walk := func(tree map[int]Snapshot) (Snapshot, bool) {
		return NearestFrom(9, func(pid int) (Snapshot, bool) {
			s, ok := tree[pid]
			return s, ok
		}, nil)
	}
	if _, ok := walk(map[int]Snapshot{}); ok {
		t.Fatal("this process cannot be read")
	}
	if _, ok := walk(map[int]Snapshot{9: {PID: 9, PPID: 8}}); ok {
		t.Fatal("the parent cannot be read")
	}
	got, ok := walk(map[int]Snapshot{9: {PID: 9, PPID: 8}, 8: {PID: 8, PPID: 8, Name: "helper"}})
	if !ok || got.PID != 8 {
		t.Fatalf("a process that is its own parent: %+v %v", got, ok)
	}
	got, ok = walk(map[int]Snapshot{9: {PID: 9, PPID: 8}, 8: {PID: 8, PPID: 0, Name: "helper"}})
	if !ok || got.PID != 8 {
		t.Fatalf("a process with no parent: %+v %v", got, ok)
	}
}

func TestClientNameCanonicalizesEveryKnownProcess(t *testing.T) {
	for _, tc := range []struct {
		name, client string
		known        bool
	}{
		{"/opt/Claude.EXE", "claude", true},
		{"codex", "codex", true},
		{"copilot", "copilot", true},
		{`C:\Tools\Cursor.exe`, "cursor", true},
		{"cursor-agent", "cursor", true},
		{"agent", "", false},
		{"agy", "antigravity", true},
		{"ANTIGRAVITY", "antigravity", true},
		{"grok", "grok", true},
		{"kimi", "kimi", true},
		{"kimi-code", "kimi", true},
		{"node", "", false},
	} {
		client, known := ClientName(tc.name)
		if client != tc.client || known != tc.known {
			t.Errorf("%q: got %q, %v; want %q, %v", tc.name, client, known, tc.client, tc.known)
		}
	}
}

func TestClientNameWithEnvRecognizesAgentOnlyAsMeasuredCursor(t *testing.T) {
	cursorEnv := func(key string) string {
		return map[string]string{"CURSOR_PROJECT_DIR": "/project", "CURSOR_VERSION": "2026.11.22"}[key]
	}
	projectOnly := func(key string) string {
		return map[string]string{"CURSOR_PROJECT_DIR": "/project"}[key]
	}
	for _, tc := range []struct {
		name   string
		getenv func(string) string
		client string
		known  bool
	}{
		{name: "agent", getenv: cursorEnv, client: "cursor", known: true},
		{name: `C:\Tools\Agent.EXE`, getenv: cursorEnv, client: "cursor", known: true},
		{name: "agent", getenv: projectOnly},
		{name: "agent"},
		{name: "cursor-agent", client: "cursor", known: true},
		{name: "claude", getenv: cursorEnv, client: "claude", known: true},
		{name: "codex", getenv: cursorEnv, client: "codex", known: true},
	} {
		client, known := ClientNameWithEnv(tc.name, tc.getenv)
		if client != tc.client || known != tc.known {
			t.Errorf("%q: got %q, %v; want %q, %v", tc.name, client, known, tc.client, tc.known)
		}
	}
}

func TestSingleSessionKnowsOnlyMeasuredLaunchers(t *testing.T) {
	for _, c := range []struct {
		name, client string
		want         bool
	}{
		{"cursor-agent", "cursor", true},
		{`C:\Tools\Cursor.exe`, "cursor", false},
		{"antigravity", "antigravity", true},
		{"agy", "antigravity", true},
		{"electron", "antigravity", false},
		{"kimi", "kimi", true},
		{"kimi-code", "kimi", true},
		{"python", "kimi", false},
		{"claude", "claude", false},
	} {
		if got := SingleSession(Ancestor{PID: 1, Name: c.name}, c.client); got != c.want {
			t.Errorf("%s as %s = %v", c.name, c.client, got)
		}
	}
}

func TestSingleSessionCodexNeedsTheExecCommand(t *testing.T) {
	a := Ancestor{PID: 42, Name: "codex"}
	line := func(raw string, err error) func(string) ([]byte, error) {
		return func(path string) ([]byte, error) {
			if path != "/proc/42/cmdline" {
				t.Fatal(path)
			}
			return []byte(raw), err
		}
	}
	if !singleSession(a, "codex", line("codex\x00exec\x00Reply with ok\x00", nil)) {
		t.Fatal("codex exec")
	}
	if singleSession(a, "codex", line("codex\x00app-server\x00--config\x00exec\x00", nil)) {
		t.Fatal("app-server serves many sessions")
	}
	if singleSession(a, "codex", line("codex", nil)) {
		t.Fatal("no command")
	}
	if singleSession(a, "codex", line("", errors.New("unreadable"))) {
		t.Fatal("no evidence is not positive evidence")
	}
	if singleSession(Ancestor{PID: 42, Name: "node"}, "codex", line("codex\x00exec\x00", nil)) {
		t.Fatal("another process")
	}
}

func TestPublicSingleSessionReadsTheProcessTable(t *testing.T) {
	if SingleSession(Ancestor{PID: 0x7FFFFFF0, Name: "codex"}, "codex") {
		t.Fatal("no such process")
	}
}
