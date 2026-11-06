package integrate

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fakePlugins struct {
	calls []string
}

func (f *fakePlugins) MarketplaceAdd(dir string) error {
	f.calls = append(f.calls, "add "+dir)
	return nil
}

func (f *fakePlugins) Install(id string) error {
	f.calls = append(f.calls, "install "+id)
	return nil
}

func (f *fakePlugins) Uninstall(id string) error {
	f.calls = append(f.calls, "uninstall "+id)
	return nil
}

func (f *fakePlugins) MarketplaceRemove(name string) error {
	f.calls = append(f.calls, "remove "+name)
	return nil
}

func testLayout(t *testing.T) Layout {
	t.Helper()
	home := t.TempDir()
	link := filepath.Join(home, "bin", "iris")
	ver := filepath.Join(home, "versions", "1.2.3")
	if err := os.MkdirAll(ver, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	target, err := PlaceFake(ver, "iris")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	return Layout{
		Home:     home,
		StateDir: filepath.Join(home, "state"),
		Getenv:   func(string) string { return "" },
		LookPath: func(name string) (string, error) {
			if name == "iris" {
				return link, nil
			}
			return "", errors.New("not found")
		},
		GOOS: "linux",
	}
}

func TestAllClientsRoundTrip(t *testing.T) {
	if got := len(All()); got != len(Names) {
		t.Fatalf("registered %d, want %d", got, len(Names))
	}
	for _, c := range All() {
		t.Run(c.Name(), func(t *testing.T) {
			l := testLayout(t)
			plugins := &fakePlugins{}
			if err := c.Install(l, plugins); err != nil {
				t.Fatal(err)
			}
			files, err := c.Files(l)
			if err != nil {
				t.Fatal(err)
			}
			first := readFiles(t, files)
			if err := c.Install(l, plugins); err != nil {
				t.Fatal(err)
			}
			second := readFiles(t, files)
			for i := range files {
				if first[i] != second[i] {
					t.Fatalf("%s changed on re-run\n%s\n%s", files[i], first[i], second[i])
				}
				if strings.Contains(first[i], "versions/1.2.3") || strings.Contains(first[i], `versions\1.2.3`) {
					t.Fatalf("%s uses a versioned path\n%s", files[i], first[i])
				}
			}
			if _, err := c.Canary(l); err != nil {
				t.Fatal(err)
			}
			if err := c.Remove(l, plugins); err != nil {
				t.Fatal(err)
			}
			for _, f := range files {
				if _, err := os.Stat(f); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("%s still exists after remove", f)
				}
			}
			if c.Name() == Claude {
				joined := strings.Join(plugins.calls, "\n")
				if !strings.Contains(joined, "install iris@iris") || !strings.Contains(joined, "uninstall iris@iris") {
					t.Fatal(plugins.calls)
				}
			}
		})
	}
}

func TestPreservesUserContentAndRestoresIt(t *testing.T) {
	for _, c := range All() {
		t.Run(c.Name(), func(t *testing.T) {
			l := testLayout(t)
			files, err := c.Files(l)
			if err != nil {
				t.Fatal(err)
			}
			original := map[string]string{}
			for _, f := range files {
				if err := os.MkdirAll(filepath.Dir(f), 0o700); err != nil {
					t.Fatal(err)
				}
				var body string
				switch {
				case strings.HasSuffix(f, "monitors.json"):
					body = "[\n  {\"name\": \"user\", \"command\": \"USER_KEEP\"}\n]\n"
				case strings.HasSuffix(f, ".json"):
					body = "{\n  \"userNote\": \"USER_KEEP\"\n}\n"
				case strings.HasSuffix(f, ".toml"):
					body = "user = \"USER_KEEP\"\n"
				default:
					body = "USER_KEEP\n"
				}
				if err := os.WriteFile(f, []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
				original[f] = body
			}
			if err := c.Install(l, &fakePlugins{}); err != nil {
				t.Fatal(err)
			}
			for _, f := range files {
				got, err := os.ReadFile(f)
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(string(got), "USER_KEEP") {
					t.Fatalf("%s lost user content\n%s", f, got)
				}
			}
			if err := c.Remove(l, &fakePlugins{}); err != nil {
				t.Fatal(err)
			}
			for _, f := range files {
				got, err := os.ReadFile(f)
				if err != nil {
					t.Fatal(err)
				}
				if string(got) != original[f] {
					t.Fatalf("%s\n got %q\nwant %q", f, got, original[f])
				}
			}
		})
	}
}

func TestBrokenFileIsRefused(t *testing.T) {
	for _, c := range All() {
		t.Run(c.Name(), func(t *testing.T) {
			l := testLayout(t)
			files, err := c.Files(l)
			if err != nil {
				t.Fatal(err)
			}
			var target string
			for _, f := range files {
				if strings.HasSuffix(f, ".json") {
					target = f
					break
				}
			}
			if target == "" {
				t.Fatal("no json file")
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(target, []byte("{"), 0o644); err != nil {
				t.Fatal(err)
			}
			err = c.Install(l, &fakePlugins{})
			if !errors.Is(err, ErrBroken) {
				t.Fatalf("err %v", err)
			}
			got, err := os.ReadFile(target)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != "{" {
				t.Fatalf("overwrote broken file: %q", got)
			}
		})
	}
}

func TestBrokenTOMLMarkerIsRefused(t *testing.T) {
	for _, name := range []string{Codex, Grok, Kimi} {
		c, _ := ByName(name)
		l := testLayout(t)
		files, err := c.Files(l)
		if err != nil {
			t.Fatal(err)
		}
		var target string
		for _, f := range files {
			if strings.HasSuffix(f, ".toml") {
				target = f
			}
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			t.Fatal(err)
		}
		body := "# >>> iris >>>\n[[hooks]]\nevent = \"Nope\"\n"
		if err := os.WriteFile(target, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := c.Install(l, &fakePlugins{}); !errors.Is(err, ErrBroken) {
			t.Fatalf("%s: %v", name, err)
		}
		got, _ := os.ReadFile(target)
		if string(got) != body {
			t.Fatalf("%s overwrote %q", name, got)
		}
	}
}

func TestTimeoutsAndKimiKeys(t *testing.T) {
	l := testLayout(t)
	plugins := &fakePlugins{}
	for _, c := range All() {
		if err := c.Install(l, plugins); err != nil {
			t.Fatal(c.Name(), err)
		}
	}
	codex, _ := ByName(Codex)
	files, _ := codex.Files(l)
	text, _ := os.ReadFile(files[0])
	if !strings.Contains(string(text), `"timeout": 1`) || !strings.Contains(string(text), `"timeout": 3`) {
		t.Fatalf("codex timeouts\n%s", text)
	}
	claude, _ := ByName(Claude)
	cfiles, _ := claude.Files(l)
	var hooks string
	for _, f := range cfiles {
		if strings.HasSuffix(f, filepath.Join("hooks", "hooks.json")) {
			hooks = string(mustRead(t, f))
		}
		if strings.HasSuffix(f, "SKILL.md") && !strings.Contains(string(mustRead(t, f)), notificationPlain) {
			t.Fatal("skill missing notification text")
		}
	}
	if !strings.Contains(hooks, `"timeout": 1`) {
		t.Fatalf("claude session end timeout\n%s", hooks)
	}
	kimi, _ := ByName(Kimi)
	kfiles, _ := kimi.Files(l)
	body := string(mustRead(t, kfiles[0]))
	if err := validateHookKeys(body); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, `event = "PreToolUse"`) || strings.Contains(body, "permission") {
		t.Fatalf("kimi block\n%s", body)
	}
	if Human := codex.Human(); Human != "Review hooks once in /hooks" {
		t.Fatal(Human)
	}
}

func TestCodexCommandWindows(t *testing.T) {
	l := testLayout(t)
	l.GOOS = "windows"
	l.LookPath = func(name string) (string, error) {
		if name == "iris" {
			return `C:\Program Files\iris\iris.exe`, nil
		}
		return "", errors.New("not found")
	}
	codex, _ := ByName(Codex)
	if err := codex.Install(l, &fakePlugins{}); err != nil {
		t.Fatal(err)
	}
	in, err := codex.Canary(l)
	if err != nil {
		t.Fatal(err)
	}
	if !in.Comspec {
		t.Fatal("windows canary is not a cmd.exe /C command")
	}
	want := `"C:\Program Files\iris\iris.exe" hook --client codex --event SessionStart`
	if in.Shell != want {
		t.Fatalf("%s", in.Shell)
	}
}

func TestExecPluginsArgs(t *testing.T) {
	var got []string
	p := ExecPlugins{Run: func(name string, args ...string) error {
		got = append(got, name+" "+strings.Join(args, " "))
		return nil
	}}
	if err := p.MarketplaceAdd("/tmp/plug"); err != nil {
		t.Fatal(err)
	}
	if err := p.Install("iris@iris"); err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, "\n") != "claude plugin marketplace add /tmp/plug\nclaude plugin install iris@iris" {
		t.Fatal(got)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func readFiles(t *testing.T, files []string) []string {
	t.Helper()
	out := make([]string, len(files))
	for i, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		out[i] = string(b)
		if strings.TrimSpace(out[i]) == "" {
			t.Fatalf("empty %s", f)
		}
	}
	return out
}
