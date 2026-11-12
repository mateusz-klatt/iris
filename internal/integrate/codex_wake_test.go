package integrate

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func assertCodexWake(t *testing.T, l Layout, configured, enabled bool) {
	t.Helper()
	gotConfigured, gotEnabled, err := CodexWake(l)
	if err != nil || gotConfigured != configured || gotEnabled != enabled {
		t.Fatalf("Codex wake configured/enabled = %v/%v, %v; want %v/%v", gotConfigured, gotEnabled, err, configured, enabled)
	}
}

func TestCodexWakePreferenceSurvivesInstallDriftAndExplicitChanges(t *testing.T) {
	l := testLayout(t)
	c := codexClient{}
	files, _ := c.Files(l)
	const original = "model = \"my-model\"\n\n[mcp_servers.other]\ncommand = \"other\"\n"
	seedFile(t, files[1], original)
	assertCodexWake(t, l, false, false)
	if err := c.Install(l, nil); err != nil {
		t.Fatal(err)
	}
	assertCodexWake(t, l, true, false)
	if err := InstallCodexWake(l, true); err != nil {
		t.Fatal(err)
	}
	assertCodexWake(t, l, true, true)
	first := readFiles(t, files)
	if !strings.Contains(first[1], original) || !strings.Contains(first[1], `args = ["mcp", "--client", "codex", "--wake"]`) {
		t.Fatalf("wake installation lost user configuration or its exact argument: %s", first[1])
	}
	for _, install := range []func() error{func() error { return c.Install(l, nil) }, func() error { return InstallCodexWake(l, true) }} {
		if err := install(); err != nil {
			t.Fatal(err)
		}
		if got := readFiles(t, files); got[0] != first[0] || got[1] != first[1] {
			t.Fatal("ordinary or explicit reintegration changed an already-enabled integration")
		}
	}
	if drift, err := l.Drift(c); err != nil || len(drift) != 0 {
		t.Fatalf("enabled wake drift = %v, %v", drift, err)
	}
	assertCodexWake(t, l, true, true)
	manifest, err := loadManifest(l.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	backup := manifest.Files[files[1]].Backup
	if backup == "" || string(mustRead(t, filepath.Join(l.StateDir, backup))) != original {
		t.Fatal("wake changes replaced the first original backup")
	}
	if err := InstallCodexWake(l, false); err != nil {
		t.Fatal(err)
	}
	assertCodexWake(t, l, true, false)
	if err := c.Install(l, nil); err != nil {
		t.Fatal(err)
	}
	assertCodexWake(t, l, true, false)
	if err := c.Remove(l, nil); err != nil {
		t.Fatal(err)
	}
	if got := string(mustRead(t, files[1])); got != original {
		t.Fatalf("removal did not restore the untouched user's configuration: %s", got)
	}
	assertCodexWake(t, l, false, false)
}

func TestCodexWakeDriftAndRemovalPreserveUserAdditions(t *testing.T) {
	l := testLayout(t)
	c := codexClient{}
	files, _ := c.Files(l)
	if drift, err := l.Drift(c); err != nil || len(drift) != 2 {
		t.Fatalf("fresh dry run = %v, %v", drift, err)
	}
	for _, path := range append(files, l.StateDir) {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("dry run created %s: %v", path, err)
		}
	}
	assertCodexWake(t, l, false, false)
	if err := InstallCodexWake(l, true); err != nil {
		t.Fatal(err)
	}
	const addition = "\n[projects.\"/user/repo\"]\ntrust_level = \"trusted\"\n"
	raw := string(mustRead(t, files[1])) + addition
	seedFile(t, files[1], raw)
	if drift, err := l.Drift(c); err != nil || len(drift) != 0 {
		t.Fatalf("user additions changed wake drift = %v, %v", drift, err)
	}
	if got := string(mustRead(t, files[1])); got != raw {
		t.Fatal("dry run wrote an enabled configuration")
	}
	if err := c.Remove(l, nil); err != nil {
		t.Fatal(err)
	}
	if got := string(mustRead(t, files[1])); strings.TrimSpace(got) != strings.TrimSpace(addition) {
		t.Fatalf("removal lost user additions or retained Iris's wake: %s", got)
	}
}

func TestCodexWakeReadsTOMLSyntaxAndOnlyAValidCodexInvocation(t *testing.T) {
	for name, tc := range map[string]struct {
		config              string
		configured, enabled bool
	}{
		"empty":                              {},
		"enabled":                            {"[mcp_servers.iris]\ncommand='iris'\nargs=['mcp','--client','codex','--wake']\n", true, true},
		"disabled":                           {"[mcp_servers.iris]\ncommand='/opt/iris'\nargs=['mcp','--client','codex']\n", true, false},
		"quoted and multiline":               {"[mcp_servers.\"iris\"]\ncommand='iris'\nargs=[\n'mcp', # a command\n'--wake',\n'--client',\n'codex',\n]\n", true, true},
		"inline":                             {"mcp_servers={iris={command='iris',args=['mcp','--client','codex','--wake']}}\n", true, true},
		"dotted":                             {"mcp_servers.iris.command='iris'\nmcp_servers.iris.args=['mcp','--client=codex','--wake']\n", true, true},
		"windows path":                       {"[mcp_servers.iris]\ncommand='C:\\tools\\iris.exe'\nargs=['mcp','--client','codex','--wake']\n", true, true},
		"explicit false":                     {"[mcp_servers.iris]\ncommand='iris'\nargs=['mcp','--client','codex','--wake=false']\n", true, false},
		"explicit true":                      {"[mcp_servers.iris]\ncommand='iris'\nargs=['mcp','--client','codex','--wake=true']\n", true, true},
		"wake text in another value":         {"note='--wake'\n[mcp_servers.iris]\ncommand='iris'\nargs=['mcp','--client','codex']\n", true, false},
		"table text inside a literal string": {"note='''\n[mcp_servers.iris]\ncommand='iris'\nargs=['mcp','--client','codex','--wake']\n'''\n", false, false},
		"another server":                     {"[mcp_servers.other]\ncommand='iris'\nargs=['mcp','--client','codex','--wake']\n", false, false},
		"wrong command":                      {"[mcp_servers.iris]\ncommand='another-program'\nargs=['mcp','--client','codex','--wake']\n", false, false},
		"missing command":                    {"[mcp_servers.iris]\nargs=['mcp','--client','codex','--wake']\n", false, false},
		"numeric command":                    {"[mcp_servers.iris]\ncommand=4\nargs=['mcp','--client','codex','--wake']\n", false, false},
		"missing args":                       {"[mcp_servers.iris]\ncommand='iris'\n", false, false},
		"scalar args":                        {"[mcp_servers.iris]\ncommand='iris'\nargs='mcp --client codex --wake'\n", false, false},
		"nonstring argument":                 {"[mcp_servers.iris]\ncommand='iris'\nargs=['mcp',3]\n", false, false},
		"another subcommand":                 {"[mcp_servers.iris]\ncommand='iris'\nargs=['hook','--client','codex','--wake']\n", false, false},
		"another client":                     {"[mcp_servers.iris]\ncommand='iris'\nargs=['mcp','--client','claude','--wake']\n", false, false},
		"extra positional argument":          {"[mcp_servers.iris]\ncommand='iris'\nargs=['mcp','--client','codex','--wake','extra']\n", false, false},
		"flag prefix is not wake":            {"[mcp_servers.iris]\ncommand='iris'\nargs=['mcp','--client','codex','--wake-extra']\n", false, false},
		"malformed flag value":               {"[mcp_servers.iris]\ncommand='iris'\nargs=['mcp','--client','codex','--wake=invalid']\n", false, false},
	} {
		t.Run(name, func(t *testing.T) {
			l := testLayout(t)
			file, _ := CodexConfig(l)
			seedFile(t, file, tc.config)
			assertCodexWake(t, l, tc.configured, tc.enabled)
			if got := string(mustRead(t, file)); got != tc.config {
				t.Fatal("inspection changed the configuration")
			}
		})
	}
}

func TestCodexWakeRefusesBrokenConfigurationBeforeWritingHooks(t *testing.T) {
	for name, raw := range map[string]string{
		"invalid TOML":                   "broken = [\n",
		"duplicate table":                "[mcp_servers.iris]\n[mcp_servers.iris]\n",
		"server shape":                   "mcp_servers = 4\n",
		"broken markers":                 markerStart + "\n[mcp_servers.iris]\ncommand='iris'\n",
		"unmarked table":                 "[mcp_servers.iris]\ncommand='iris'\nargs=['mcp','--client','codex','--wake']\n",
		"markers inside multiline text":  "note='''\n" + markerStart + "\n[mcp_servers.iris]\ncommand='iris'\nargs=['mcp','--client','codex','--wake']\n" + markerEnd + "\n'''\n",
		"markers inside one line string": "note = \"" + markerStart + " content " + markerEnd + "\"\n",
		"extra start marker":             wrapBlock("[mcp_servers.iris]\ncommand='iris'\nargs=['mcp','--client','codex','--wake']\n") + markerStart + "\n",
		"extra end marker":               wrapBlock("[mcp_servers.iris]\ncommand='iris'\nargs=['mcp','--client','codex','--wake']\n") + markerEnd + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			l := testLayout(t)
			files, _ := (codexClient{}).Files(l)
			seedFile(t, files[1], raw)
			if err := InstallCodexWake(l, true); !errors.Is(err, ErrBroken) {
				t.Fatalf("unsafe configuration was accepted: %v", err)
			}
			if got := string(mustRead(t, files[1])); got != raw {
				t.Fatalf("refused configuration was changed: %s", got)
			}
			for _, path := range []string{files[0], l.StateDir} {
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("refusal wrote %s: %v", path, err)
				}
			}
		})
	}
}

func TestCodexWakeReportsReadAndLayoutErrorsAndKeepsBrokenHooks(t *testing.T) {
	l := homeless(t)
	if _, _, err := CodexWake(l); err == nil {
		t.Fatal("inspection guessed a home")
	}
	if err := InstallCodexWake(l, true); err == nil {
		t.Fatal("installation guessed a home")
	}
	l = testLayout(t)
	files, _ := (codexClient{}).Files(l)
	if err := os.MkdirAll(files[1], 0o700); err != nil {
		t.Fatal(err)
	}
	if _, _, err := CodexWake(l); err == nil {
		t.Fatal("an unreadable configuration became disabled state")
	}
	if err := InstallCodexWake(l, true); err == nil {
		t.Fatal("an unreadable configuration was replaced")
	}
	l = testLayout(t)
	files, _ = (codexClient{}).Files(l)
	seedFile(t, files[0], "{")
	if err := InstallCodexWake(l, true); !errors.Is(err, ErrBroken) {
		t.Fatalf("broken hooks were replaced: %v", err)
	}
	if got := string(mustRead(t, files[0])); got != "{" {
		t.Fatal("refusing broken hooks changed them")
	}
	seedFile(t, files[1], "broken = [\n")
	if _, _, err := CodexWake(l); !errors.Is(err, ErrBroken) {
		t.Fatalf("inspection converted malformed TOML to disabled state: %v", err)
	}
}

func TestCodexWakePreservesManagedCustomBinaryAcrossItsMove(t *testing.T) {
	l := testLayout(t)
	l.LookPath = func(string) (string, error) { return "/tools/iris-preview", nil }
	if err := InstallCodexWake(l, true); err != nil {
		t.Fatalf("custom Iris binary cannot be installed: %v", err)
	}
	assertCodexWake(t, l, true, true)
	file, _ := CodexConfig(l)
	if got := string(mustRead(t, file)); !strings.Contains(got, `command = "/tools/iris-preview"`) {
		t.Fatalf("custom Iris binary was lost: %s", got)
	}
	l.LookPath = func(string) (string, error) { return "/new/iris-dev", nil }
	assertCodexWake(t, l, true, true)
	before := string(mustRead(t, file))
	if drift, err := l.Drift(codexClient{}); err != nil || len(drift) != 2 {
		t.Fatalf("custom-binary move dry run = %v, %v", drift, err)
	}
	if string(mustRead(t, file)) != before {
		t.Fatal("custom-binary move dry run wrote the configuration")
	}
	if err := (codexClient{}).Install(l, nil); err != nil {
		t.Fatal(err)
	}
	assertCodexWake(t, l, true, true)
	if got := string(mustRead(t, file)); !strings.Contains(got, `command = "/new/iris-dev"`) || strings.Contains(got, "/tools/iris-preview") {
		t.Fatalf("reintegration did not move the managed binary while preserving wake: %s", got)
	}
}

func TestCodexWakeRecognizesAnExpectedCustomBinaryWithoutClaimingUnrelatedServers(t *testing.T) {
	l := testLayout(t)
	l.LookPath = func(string) (string, error) { return "/tools/iris-preview", nil }
	file, _ := CodexConfig(l)
	const config = "[mcp_servers.iris]\ncommand='/tools/iris-preview'\nargs=['mcp','--client','codex','--wake']\n"
	seedFile(t, file, config)
	assertCodexWake(t, l, true, true)
	l.LookPath = func(string) (string, error) { return "/other/iris-dev", nil }
	assertCodexWake(t, l, false, false)
	if err := InstallCodexWake(l, true); !errors.Is(err, ErrBroken) {
		t.Fatalf("an unrelated unmarked command was adopted: %v", err)
	}
	if got := string(mustRead(t, file)); got != config {
		t.Fatal("refusing an unrelated server changed it")
	}
}

func TestCodexWakeCustomCommandRequiresARealManagedTable(t *testing.T) {
	const table = "[mcp_servers.iris]\ncommand='unrelated'\nargs=['mcp','--client','codex','--wake']\n"
	for name, raw := range map[string]string{
		"copied markers inside a string":      "note='''\n" + markerStart + "\n" + table + markerEnd + "\n'''\n" + table,
		"invalid marker data inside a string": "note=\"\"\"\n" + markerStart + "\n'''unterminated\n" + markerEnd + "\n\"\"\"\n" + table,
		"only one marker":                     markerStart + "\n" + table,
		"reversed markers":                    markerEnd + "\n" + table + markerStart + "\n",
		"another managed table":               markerStart + "\n[mcp_servers.other]\ncommand='other'\n" + markerEnd + "\n" + table,
	} {
		t.Run(name, func(t *testing.T) {
			l := testLayout(t)
			file, _ := CodexConfig(l)
			seedFile(t, file, raw)
			assertCodexWake(t, l, false, false)
			if got := string(mustRead(t, file)); got != raw {
				t.Fatal("inspecting untrusted marker data changed it")
			}
		})
	}
}

func TestCodexWakeRefusesMarkersInUserDataBesideAnEscapedIrisTable(t *testing.T) {
	l := testLayout(t)
	files, _ := (codexClient{}).Files(l)
	const original = "note = \"\"\"\n" + markerStart + "\nKEEP_USER_NOTE\n" + markerEnd + "\n\"\"\"\n" +
		"[mcp_servers.\"i\\u0072is\"]\ncommand='iris'\nargs=['mcp','--client','codex','--wake']\n"
	const hooks = `{"hooks":{"Stop":[{"hooks":[{"command":"user hook"}]}]}}`
	seedFile(t, files[1], original)
	seedFile(t, files[0], hooks)
	err := (codexClient{}).Install(l, nil)
	if !errors.Is(err, ErrBroken) || string(mustRead(t, files[1])) != original || string(mustRead(t, files[0])) != hooks {
		t.Fatalf("marker text in user data changed the configuration or hooks: %v", err)
	}
	if _, err := os.Stat(l.StateDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("refusing marker data still wrote integration state: %v", err)
	}
}

func TestCodexWakeRefusesACommandPathThatCannotBeWrittenAsValidTOML(t *testing.T) {
	l := testLayout(t)
	// A carriage return is legal in a Unix filename. Until the shared writer can quote it,
	// installation must refuse it before changing either client file.
	l.LookPath = func(string) (string, error) { return "/tools/iris\rpreview", nil }
	files, _ := (codexClient{}).Files(l)
	if err := InstallCodexWake(l, true); !errors.Is(err, ErrBroken) {
		t.Fatalf("an unrepresentable command path produced a configuration: %v", err)
	}
	for _, file := range append(files, l.StateDir) {
		if _, err := os.Stat(file); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("invalid generated TOML wrote %s: %v", file, err)
		}
	}
}
