package integrate

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func env(pairs map[string]string) func(string) string {
	return func(key string) string { return pairs[key] }
}

// homeless is a layout with no way to find a home directory.
func homeless(t *testing.T) Layout {
	t.Helper()
	t.Setenv("HOME", "")
	t.Setenv("USERPROFILE", "")
	return Layout{Getenv: env(nil), GOOS: "linux", LookPath: func(string) (string, error) { return "", errors.New("none") }}
}

func needsPermissions(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" || os.Getuid() == 0 {
		t.Skip("file modes are not enforced here")
	}
}

func readOnly(t *testing.T, dir string) {
	t.Helper()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
}

func TestByNameAndAliases(t *testing.T) {
	for alias, want := range map[string]string{
		"claude-code": Claude, "codex": Codex, "copilot": Copilot, "cursor-agent": Cursor,
		"agy": Antigravity, "antigravity": Antigravity, "grok": Grok, " KIMI-code ": Kimi,
	} {
		c, ok := ByName(alias)
		if !ok || c.Name() != want {
			t.Errorf("%q: %v %v", alias, c, ok)
		}
	}
	if c, ok := ByName("emacs"); ok {
		t.Fatalf("unknown client: %v", c)
	}
}

func TestHumanStepsAreOnlyForCodex(t *testing.T) {
	for _, c := range All() {
		if got := c.Human(); (got != "") != (c.Name() == Codex) {
			t.Errorf("%s: %q", c.Name(), got)
		}
	}
}

func TestDetectFollowsLookPath(t *testing.T) {
	got := Detect(func(bin string) (string, error) {
		switch bin {
		case "agent":
			return "/bin/agent", nil
		case "kimi":
			return "", nil
		default:
			return "", errors.New("none")
		}
	})
	if strings.Join(got, ",") != Cursor {
		t.Fatalf("%v", got)
	}
}

func TestDetectUsesThePathByDefault(t *testing.T) {
	dir := t.TempDir()
	if _, err := PlaceFake(dir, "grok"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	if got := Detect(nil); strings.Join(got, ",") != Grok {
		t.Fatalf("%v", got)
	}
}

func TestBinary(t *testing.T) {
	l := Layout{LookPath: func(bin string) (string, error) {
		if bin == "agent" {
			return "/bin/agent", nil
		}
		return "", errors.New("missing " + bin)
	}}
	if got, err := l.Binary(Cursor); err != nil || got != "/bin/agent" {
		t.Fatal(got, err)
	}
	if _, err := l.Binary(Codex); err == nil || !strings.Contains(err.Error(), "missing codex") {
		t.Fatal(err)
	}
	if _, err := l.Binary("emacs"); err == nil || err.Error() != "not found" {
		t.Fatal(err)
	}
}

func TestBinaryUsesThePathByDefault(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if _, err := (Layout{}).Binary(Grok); err == nil {
		t.Fatal("found a binary on an empty path")
	}
}

func TestEnvironmentAccessors(t *testing.T) {
	t.Setenv("IRIS_TEST_VALUE", "from process")
	if got := (Layout{}).GetenvOr("IRIS_TEST_VALUE"); got != "from process" {
		t.Fatal(got)
	}
	l := Layout{Home: "/h", GOOS: "plan9", Getenv: env(map[string]string{"X": "y"})}
	if got := l.GetenvOr("X"); got != "y" {
		t.Fatal(got)
	}
	if got := l.OS(); got != "plan9" {
		t.Fatal(got)
	}
	if got := (Layout{}).OS(); got != runtime.GOOS {
		t.Fatal(got)
	}
	if got, err := l.HomeDir(); err != nil || got != "/h" {
		t.Fatal(got, err)
	}
}

func TestHomeOrder(t *testing.T) {
	cases := []struct {
		vars map[string]string
		want string
	}{
		{map[string]string{"HOME": "/unix", "USERPROFILE": "/win"}, "/unix"},
		{map[string]string{"USERPROFILE": "/win"}, "/win"},
	}
	for _, tc := range cases {
		got, err := (Layout{Getenv: env(tc.vars)}).home()
		if err != nil || got != tc.want {
			t.Errorf("%v: %q %v", tc.vars, got, err)
		}
	}
	if _, err := homeless(t).home(); err == nil {
		t.Fatal("no home but no error")
	}
}

func TestStateOrder(t *testing.T) {
	join := filepath.Join
	cases := []struct {
		name string
		l    Layout
		want string
	}{
		{"explicit", Layout{StateDir: "/s", Home: "/h"}, "/s"},
		{"override", Layout{Home: "/h", Getenv: env(map[string]string{"IRIS_STATE_DIR": "/o"})}, "/o"},
		{"xdg", Layout{Home: "/h", GOOS: "linux", Getenv: env(map[string]string{"XDG_STATE_HOME": "/x"})}, join("/x", "iris")},
		{"linux", Layout{Home: "/h", GOOS: "linux", Getenv: env(nil)}, join("/h", ".local", "state", "iris")},
		{"xdg is not for windows", Layout{Home: "/h", GOOS: "windows", Getenv: env(map[string]string{"XDG_STATE_HOME": "/x", "LOCALAPPDATA": "/l"})}, join("/l", "iris")},
		{"windows default", Layout{Home: "/h", GOOS: "windows", Getenv: env(nil)}, join("/h", "AppData", "Local", "iris")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.l.state()
			if err != nil || got != tc.want {
				t.Fatalf("%q %v, want %q", got, err, tc.want)
			}
		})
	}
	if _, err := homeless(t).state(); err == nil {
		t.Fatal("no home but no error")
	}
}

func TestEnvDirOverridesTheHomeLocation(t *testing.T) {
	l := Layout{Home: "/h", Getenv: env(map[string]string{"CODEX_HOME": "/c"})}
	files, err := codexClient{}.Files(l)
	if err != nil || files[0] != filepath.Join("/c", "hooks.json") {
		t.Fatal(files, err)
	}
}

func TestIrisFallsBackToTheBareName(t *testing.T) {
	missing := func(string) (string, error) { return "", errors.New("none") }
	if got := (Layout{GOOS: "linux", LookPath: missing}).Iris(); got != "iris" {
		t.Fatal(got)
	}
	if got := (Layout{GOOS: "windows", LookPath: missing}).Iris(); got != "iris.exe" {
		t.Fatal(got)
	}
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	want, err := PlaceFake(dir, "iris")
	if err != nil {
		t.Fatal(err)
	}
	if got := (Layout{}).Iris(); got != want {
		t.Fatalf("%q want %q", got, want)
	}
}

func TestQuoting(t *testing.T) {
	if got := cmdQuote("plain"); got != "plain" {
		t.Fatal(got)
	}
	if got := cmdQuote(`a b"c`); got != `"a b""c"` {
		t.Fatal(got)
	}
	if got := shellQuote(""); got != "''" {
		t.Fatal(got)
	}
	if got := shellQuote("it's"); got != `'it'\''s'` {
		t.Fatal(got)
	}
}

func TestWindowsHookCommandQuotesEqualsInExecutable(t *testing.T) {
	const path = `C:\tools\installed=true\iris.exe`
	const want = `"C:\tools\installed=true\iris.exe" hook --client codex --event SessionStart`
	if got := hookCmd(path, Codex, "SessionStart"); got != want {
		t.Fatalf("a Windows executable path containing '=' must stay one command token: got %q, want %q", got, want)
	}
}

func TestManifestProblems(t *testing.T) {
	dir := t.TempDir()
	path := manifestPath(dir)
	seedFile(t, path, "{")
	if _, err := loadManifest(dir); err == nil || !strings.Contains(err.Error(), "unreadable") {
		t.Fatal(err)
	}
	seedFile(t, path, "{}")
	m, err := loadManifest(dir)
	if err != nil || m.Files == nil {
		t.Fatal(m, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := loadManifest(dir); err == nil {
		t.Fatal("a directory read as a manifest")
	}
}

func writeNext([]byte) ([]byte, error) { return []byte("next\n"), nil }

func TestUpdateRefusals(t *testing.T) {
	dir := t.TempDir()
	l := Layout{StateDir: filepath.Join(dir, "state")}

	folder := filepath.Join(dir, "folder")
	if err := os.Mkdir(folder, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := l.update(Codex, folder, writeNext); !errors.Is(err, ErrBroken) {
		t.Fatal(err)
	}

	file := filepath.Join(dir, "file")
	seedFile(t, file, "x")
	if err := l.update(Codex, filepath.Join(file, "below"), writeNext); err == nil {
		t.Fatal("wrote beneath a file")
	}
}

func TestUpdateCannotReadTheFile(t *testing.T) {
	needsPermissions(t)
	dir := t.TempDir()
	l := Layout{StateDir: filepath.Join(dir, "state")}
	path := filepath.Join(dir, "secret")
	seedFile(t, path, "x")
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	if err := l.update(Codex, path, writeNext); !errors.Is(err, os.ErrPermission) {
		t.Fatal(err)
	}
}

func TestUpdateDoesNotWriteWithoutARecord(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	path := filepath.Join(dir, "config")
	seedFile(t, path, "before")

	seedFile(t, manifestPath(state), "{")
	err := Layout{StateDir: state}.update(Codex, path, writeNext)
	if err == nil {
		t.Fatal("a corrupt manifest was ignored")
	}
	if got := string(mustRead(t, path)); got != "before" {
		t.Fatalf("wrote %q", got)
	}

	if err := os.Remove(manifestPath(state)); err != nil {
		t.Fatal(err)
	}
	seedFile(t, filepath.Join(state, "integrate", "backup"), "a file where the directory goes")
	if err := (Layout{StateDir: state}).update(Codex, path, writeNext); err == nil {
		t.Fatal("no backup, but the write went ahead")
	}
	if got := string(mustRead(t, path)); got != "before" {
		t.Fatalf("wrote %q", got)
	}
}

func TestUpdateWithoutAStateDirectoryIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	if err := homeless(t).update(Codex, path, writeNext); err == nil {
		t.Fatal("no error")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
}

func TestUpdateKeepsTheFirstBackup(t *testing.T) {
	dir := t.TempDir()
	l := Layout{StateDir: filepath.Join(dir, "state")}
	path := filepath.Join(dir, "config")
	seedFile(t, path, "original")
	for _, next := range []string{"one", "two"} {
		if err := l.update(Codex, path, func([]byte) ([]byte, error) { return []byte(next), nil }); err != nil {
			t.Fatal(err)
		}
	}
	m, err := loadManifest(l.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(l.StateDir, m.Files[path].Backup)
	if got := string(mustRead(t, backup)); got != "original" {
		t.Fatalf("%q", got)
	}
}

func TestAtomicWriteFailures(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "file")
	seedFile(t, file, "x")
	if err := atomicWrite(filepath.Join(file, "below"), nil); err == nil {
		t.Fatal("wrote beneath a file")
	}
	folder := filepath.Join(dir, "folder")
	seedFile(t, filepath.Join(folder, "kept"), "x")
	if err := atomicWrite(folder, []byte("y")); err == nil {
		t.Fatal("replaced a directory")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("temporary file left behind: %v", entries)
	}
}

func TestAtomicWriteInAReadOnlyDirectory(t *testing.T) {
	needsPermissions(t)
	dir := t.TempDir()
	readOnly(t, dir)
	if err := atomicWrite(filepath.Join(dir, "file"), nil); !errors.Is(err, os.ErrPermission) {
		t.Fatal(err)
	}
}

func TestAtomicWriteKeepsTheMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no such mode")
	}
	path := filepath.Join(t.TempDir(), "file")
	seedFile(t, path, "x")
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := atomicWrite(path, []byte("y")); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatal(info.Mode())
	}
}

func TestMarshalJSONReportsAValueItCannotEncode(t *testing.T) {
	if _, err := marshalJSON(make(chan int)); err == nil {
		t.Fatal("no error")
	}
}

func TestUnwireProblems(t *testing.T) {
	if err := homeless(t).unwire(Codex, stripClient(Codex)); err == nil {
		t.Fatal("no home but no error")
	}
	state := t.TempDir()
	seedFile(t, manifestPath(state), "{")
	if err := (Layout{StateDir: state}).unwire(Codex, stripClient(Codex)); err == nil {
		t.Fatal("corrupt manifest ignored")
	}
}

func TestUnwireCannotReadOrChangeAFile(t *testing.T) {
	needsPermissions(t)
	l := testLayout(t)
	c, _ := ByName(Codex)
	if err := c.Install(l, nil); err != nil {
		t.Fatal(err)
	}
	files, _ := c.Files(l)
	if err := os.Chmod(files[0], 0); err != nil {
		t.Fatal(err)
	}
	if err := c.Remove(l, nil); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("unreadable: %v", err)
	}
	if err := os.Chmod(files[0], 0o644); err != nil {
		t.Fatal(err)
	}
	editByHand(t, files[0])
	readOnly(t, filepath.Dir(files[0]))
	if err := c.Remove(l, nil); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("unwritable: %v", err)
	}
	if !strings.Contains(string(mustRead(t, files[0])), "--client codex") {
		t.Fatal("a failed removal changed the file")
	}
}

func TestRemoveLeavesForeignHooksShapedOddly(t *testing.T) {
	data := []byte(`{"hooks":{"Stop":1,"Start":[{"hooks":[{"command":"mine"}]}]},"iris":{"kept":true}}`)
	next, removed, err := stripClient(Codex)("hooks.json", data)
	if err != nil || removed {
		t.Fatal(removed, err)
	}
	for _, want := range []string{`"Stop": 1`, `"mine"`, `"iris"`} {
		if !strings.Contains(string(next), want) {
			t.Fatalf("lost %s\n%s", want, next)
		}
	}
}

func TestStripMonitorsRefusesWhatIsNotAList(t *testing.T) {
	if _, _, err := stripMonitors([]byte("{")); !errors.Is(err, ErrBroken) {
		t.Fatal(err)
	}
	next, removed, err := stripMonitors([]byte("  "))
	if err != nil || removed || string(next) != "[]\n" {
		t.Fatalf("%q %v %v", next, removed, err)
	}
}

// With two matches, the searches return the same one every time: the one under the first key.
// Ranging over the map made the answer, and the lines covered, depend on Go's map order.
func TestTreeSearchesAreDeterministic(t *testing.T) {
	doc := map[string]any{}
	for _, k := range []string{"m", "c", "x", "a", "q", "b"} {
		doc[k] = map[string]any{"commandWindows": "iris --event SessionStart " + k}
	}
	for range 50 {
		if got, ok := findNamedString(doc, "commandWindows", "SessionStart"); !ok || got != "iris --event SessionStart a" {
			t.Fatalf("%q %v", got, ok)
		}
	}
	if got := inOrder(map[string]any{"b": 2, "a": 1, "c": 3}); len(got) != 3 || got[0] != 1 || got[1] != 2 || got[2] != 3 {
		t.Fatal(got)
	}
}
