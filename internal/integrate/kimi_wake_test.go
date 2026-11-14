package integrate

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/kimiwake"
	"github.com/mateusz-klatt/iris/internal/proc"
)

// kimiWatcherEnv makes this test binary, started again, stand in for a live watcher.
const kimiWatcherEnv = "IRIS_INTEGRATE_TEST_KIMI_WATCHER"

func assertKimiWake(t *testing.T, layout Layout, configured, enabled bool) {
	t.Helper()
	gotConfigured, gotEnabled, err := KimiWake(layout)
	if err != nil || gotConfigured != configured || gotEnabled != enabled {
		t.Fatalf("Kimi wake = %v/%v, %v; want %v/%v", gotConfigured, gotEnabled, err, configured, enabled)
	}
}

// readOrAbsent is a file's text, or "absent" when there is no file.
func readOrAbsent(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "absent"
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func kimiPreference(t *testing.T, layout Layout) string {
	t.Helper()
	root, err := KimiWakeRoot(layout)
	if err != nil {
		t.Fatal(err)
	}
	return readOrAbsent(t, kimiwake.PreferencePath(root))
}

func kimiSkillPath(t *testing.T, layout Layout) string {
	t.Helper()
	files, err := (kimiClient{}).Files(layout)
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(filepath.Dir(files[0]), "skills", "iris", "SKILL.md")
}

// kimiServer is Iris's entry in Kimi's MCP file.
func kimiServer(t *testing.T, path string) map[string]any {
	t.Helper()
	var doc map[string]any
	raw := readOrAbsent(t, path)
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("invalid MCP JSON: %s", raw)
	}
	servers, _ := doc["mcpServers"].(map[string]any)
	server, _ := servers["iris"].(map[string]any)
	return server
}

// The preference is the only record of the choice. Integration without a flag keeps it, --wake and
// --no-wake change it, the MCP arguments carry none, and the skill is there only while wake is on.
// Removal takes all of it back.
func TestKimiWakePreferenceSurvivesReintegrationAndExplicitChanges(t *testing.T) {
	layout := testLayout(t)
	customHome := filepath.Join(layout.Home, "custom-kimi")
	layout.Getenv = env(map[string]string{"KIMI_CODE_HOME": customHome})
	client := kimiClient{}
	files, err := client.Files(layout)
	if err != nil {
		t.Fatal(err)
	}
	skill := kimiSkillPath(t, layout)
	assertKimiWake(t, layout, false, false)
	if err := client.Install(layout, nil); err != nil {
		t.Fatal(err)
	}
	assertKimiWake(t, layout, true, false)
	server := kimiServer(t, files[1])
	if !reflect.DeepEqual(server["args"], []any{"mcp", "--client", "kimi"}) || server["env"].(map[string]any)["KIMI_CODE_HOME"] != customHome {
		t.Fatalf("fresh MCP entry: %+v", server)
	}
	if got := kimiPreference(t, layout); got != "default\n" {
		t.Fatalf("fresh preference %q", got)
	}
	if got := readOrAbsent(t, skill); got != "absent" {
		t.Fatalf("the skill came without wake: %q", got)
	}
	server["startupTimeoutMs"] = 12345
	server["env"].(map[string]any)["USER_SETTING"] = "retained"
	encoded, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{"iris": server}})
	if err := os.WriteFile(files[1], encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, step := range []struct {
		name    string
		install func() error
		want    string
	}{
		{"--no-wake", func() error { return InstallKimiWake(layout, false) }, kimiwake.PreferenceOff},
		{"reintegration after --no-wake", func() error { return client.Install(layout, nil) }, kimiwake.PreferenceOff},
		{"--wake", func() error { return InstallKimiWake(layout, true) }, kimiwake.PreferenceOn},
		{"reintegration after --wake", func() error { return client.Install(layout, nil) }, kimiwake.PreferenceOn},
		{"--no-wake again", func() error { return InstallKimiWake(layout, false) }, kimiwake.PreferenceOff},
	} {
		if err := step.install(); err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
		on := step.want == kimiwake.PreferenceOn
		assertKimiWake(t, layout, true, on)
		wantSkill := "absent"
		if on {
			wantSkill = kimiSkill
		}
		if got := kimiPreference(t, layout); got != step.want+"\n" {
			t.Fatalf("%s: preference %q", step.name, got)
		}
		if got := readOrAbsent(t, skill); got != wantSkill {
			t.Fatalf("%s: skill %q", step.name, got)
		}
		server := kimiServer(t, files[1])
		if !reflect.DeepEqual(server["args"], []any{"mcp", "--client", "kimi"}) ||
			server["startupTimeoutMs"] != float64(12345) || server["env"].(map[string]any)["USER_SETTING"] != "retained" {
			t.Fatalf("%s: MCP entry %+v", step.name, server)
		}
		if drift, err := layout.Drift(client); err != nil || len(drift) != 0 {
			t.Fatalf("%s: drift %v, %v", step.name, drift, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(customHome, "skills")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("turning wake off left the skills directory Iris made: %v", err)
	}
	if err := client.Remove(layout, nil); err != nil {
		t.Fatal(err)
	}
	assertKimiWake(t, layout, false, false)
	root, err := KimiWakeRoot(layout)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("removal left the wake state directory Iris made: %v", err)
	}
	if got := readOrAbsent(t, files[1]); got != "absent" {
		t.Fatalf("removal left the MCP file: %s", got)
	}
}

// An integration from before ADR 030 recorded wake in Iris's MCP arguments. Without a flag a
// recorded preference stays, and otherwise only an old --wake or --wake=true becomes on
// ([decided 20261113-1]). Afterwards the arguments name no wake at all.
func TestKimiWakeMigratesTheChoiceInOldMCPArguments(t *testing.T) {
	on, off, def := kimiwake.PreferenceOn, kimiwake.PreferenceOff, kimiwake.PreferenceDefault
	bare := []any{"mcp", "--client", "kimi"}
	wake := []any{"mcp", "--client", "kimi", "--wake"}
	for _, tc := range []struct {
		name    string
		marker  string
		args    []any
		command string
		want    string
	}{
		{"fresh", "", nil, "", def},
		{"bare", "", bare, "", def},
		{"old off", "", []any{"mcp", "--client", "kimi", "--wake=false"}, "", def},
		{"old on", "", wake, "", on},
		{"old on spelled out", "", []any{"mcp", "--client", "kimi", "--wake=true"}, "", on},
		{"another program's arguments", "", wake, filepath.Join("opt", "other", "server"), def},
		{"recorded off stays", "off\n", wake, "", off},
		{"recorded on stays", "on\n", bare, "", on},
		{"recorded default stays", "default\n", wake, "", def},
		{"a file a removal emptied", "\n", wake, "", on},
	} {
		t.Run(tc.name, func(t *testing.T) {
			layout := testLayout(t)
			files, err := (kimiClient{}).Files(layout)
			if err != nil {
				t.Fatal(err)
			}
			if tc.args != nil {
				command := tc.command
				if command == "" {
					command = layout.Iris()
				}
				raw, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{"iris": map[string]any{"command": command, "args": tc.args}}})
				seedFile(t, files[1], string(raw))
			}
			if tc.marker != "" {
				root, err := KimiWakeRoot(layout)
				if err != nil {
					t.Fatal(err)
				}
				seedFile(t, kimiwake.PreferencePath(root), tc.marker)
			}
			if err := (kimiClient{}).Install(layout, nil); err != nil {
				t.Fatal(err)
			}
			if got := kimiPreference(t, layout); got != tc.want+"\n" {
				t.Fatalf("preference %q, want %q", got, tc.want)
			}
			server := kimiServer(t, files[1])
			if server["command"] != layout.Iris() || !reflect.DeepEqual(server["args"], bare) {
				t.Fatalf("migrated MCP entry %+v", server)
			}
			wantSkill := "absent"
			if tc.want == on {
				wantSkill = kimiSkill
			}
			if got := readOrAbsent(t, kimiSkillPath(t, layout)); got != wantSkill {
				t.Fatalf("skill %q", got)
			}
		})
	}
}

func TestKimiLegacyWakeReadsOnlyAValidKimiInvocation(t *testing.T) {
	const iris = "/opt/iris-build"
	wake := []any{"mcp", "--client", "kimi", "--wake"}
	for _, tc := range []struct {
		name   string
		server map[string]any
		want   bool
	}{
		{"empty", nil, false},
		{"other command", map[string]any{"command": "other", "args": wake}, false},
		{"no command", map[string]any{"args": wake}, false},
		{"Windows Iris", map[string]any{"command": `C:\tools\iris.exe`, "args": wake}, true},
		{"this Iris by its path", map[string]any{"command": iris, "args": wake}, true},
		{"non-string arg", map[string]any{"command": "iris", "args": []any{"mcp", 1}}, false},
		{"no args", map[string]any{"command": "iris"}, false},
		{"another subcommand", map[string]any{"command": "iris", "args": []any{"watch", "--client", "kimi", "--wake"}}, false},
		{"bad flag value", map[string]any{"command": "iris", "args": []any{"mcp", "--client", "kimi", "--wake=bad"}}, false},
		{"positional", map[string]any{"command": "iris", "args": append(append([]any{}, wake...), "extra")}, false},
		{"another client", map[string]any{"command": "iris", "args": []any{"mcp", "--client", "codex", "--wake"}}, false},
		{"off", map[string]any{"command": "iris", "args": []any{"mcp", "--client", "kimi", "--wake=false"}}, false},
		{"bare", map[string]any{"command": "iris", "args": []any{"mcp", "--client", "kimi"}}, false},
		{"on", map[string]any{"command": "iris", "args": wake}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc := map[string]any{}
			if tc.server != nil {
				doc["mcpServers"] = map[string]any{"iris": tc.server}
			}
			if got := kimiLegacyWake(doc, iris); got != tc.want {
				t.Fatalf("read %v, want %v", got, tc.want)
			}
		})
	}
}

func TestKimiWakeConvertsRemoteMCPToStdio(t *testing.T) {
	doc := map[string]any{"mcpServers": map[string]any{"iris": map[string]any{
		"url": "https://example.invalid/mcp", "transport": "http", "headers": map[string]any{"Authorization": "old"},
		"bearerTokenEnvVar": "OLD_TOKEN", "startupTimeoutMs": 12345,
		"env": map[string]any{"USER_SETTING": "retained"},
	}}}
	kimiMCPConfig(doc, "/opt/iris", "/home/person/.kimi-code")
	server := doc["mcpServers"].(map[string]any)["iris"].(map[string]any)
	for _, key := range []string{"url", "transport", "headers", "bearerTokenEnvVar"} {
		if _, present := server[key]; present {
			t.Fatalf("remote transport field %q survived stdio conversion: %+v", key, server)
		}
	}
	if server["command"] != "/opt/iris" || server["startupTimeoutMs"] != 12345 ||
		!reflect.DeepEqual(server["args"], []any{"mcp", "--client", "kimi"}) {
		t.Fatalf("remote entry was not converted without losing its settings: %+v", server)
	}
	if !reflect.DeepEqual(server["env"], map[string]any{"USER_SETTING": "retained", "KIMI_CODE_HOME": "/home/person/.kimi-code"}) {
		t.Fatalf("MCP environment was not retained: %+v", server["env"])
	}
}

func TestKimiInstallStoresAbsoluteCustomHome(t *testing.T) {
	layout := testLayout(t)
	home := filepath.Join(layout.Home, "custom", "kimi")
	t.Chdir(layout.Home)
	relative := filepath.Join("custom", "kimi")
	layout.Getenv = env(map[string]string{"KIMI_CODE_HOME": relative})
	client := kimiClient{}
	if err := client.Install(layout, nil); err != nil {
		t.Fatal(err)
	}
	files, err := client.Files(layout)
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(files[0]) || filepath.Dir(files[0]) != home {
		t.Fatalf("Kimi files did not use the absolute custom home: %v", files)
	}
	if got := kimiServer(t, files[1])["env"].(map[string]any)["KIMI_CODE_HOME"]; got != home {
		t.Fatalf("stored Kimi home = %q, want %q", got, home)
	}
}

// A preference that is not default, on or off is refused rather than guessed, before any file is
// written. An explicit choice replaces it.
func TestKimiWakeRefusesABrokenPreference(t *testing.T) {
	layout := testLayout(t)
	files, err := (kimiClient{}).Files(layout)
	if err != nil {
		t.Fatal(err)
	}
	root, err := KimiWakeRoot(layout)
	if err != nil {
		t.Fatal(err)
	}
	seedFile(t, kimiwake.PreferencePath(root), "maybe\n")
	if _, _, err := KimiWake(layout); !errors.Is(err, kimiwake.ErrPreference) {
		t.Fatalf("broken preference read: %v", err)
	}
	if err := (kimiClient{}).Install(layout, nil); !errors.Is(err, ErrBroken) {
		t.Fatalf("broken preference install: %v", err)
	}
	if got := readOrAbsent(t, files[0]); got != "absent" {
		t.Fatalf("a broken preference let the hooks be written: %s", got)
	}
	if err := InstallKimiWake(layout, false); err != nil {
		t.Fatal(err)
	}
	if got := kimiPreference(t, layout); got != "off\n" {
		t.Fatalf("explicit choice over a broken preference: %q", got)
	}

	unreadable := testLayout(t)
	root, err = KimiWakeRoot(unreadable)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(kimiwake.PreferencePath(root), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, _, err := KimiWake(unreadable); err == nil || errors.Is(err, kimiwake.ErrPreference) {
		t.Fatalf("unreadable preference read: %v", err)
	}
	if err := (kimiClient{}).Install(unreadable, nil); !errors.Is(err, ErrBroken) {
		t.Fatalf("unreadable preference install: %v", err)
	}
	// An explicit --wake does not read the old choice, but it cannot record the new one either,
	// and the skill never tells the model about a wake that is not on.
	if err := InstallKimiWake(unreadable, true); err == nil {
		t.Fatal("wake turned on without a preference")
	}
	if got := readOrAbsent(t, kimiSkillPath(t, unreadable)); got != "absent" {
		t.Fatalf("the skill came with an unrecorded wake: %q", got)
	}
}

func TestKimiInstallRejectsBrokenMCPBeforeWriting(t *testing.T) {
	layout := testLayout(t)
	files, err := (kimiClient{}).Files(layout)
	if err != nil {
		t.Fatal(err)
	}
	seedFile(t, files[0], "# user config\n")
	seedFile(t, files[1], "{")
	before := readFiles(t, files)
	if err := (kimiClient{}).Install(layout, nil); !errors.Is(err, ErrBroken) {
		t.Fatalf("broken MCP install error: %v", err)
	}
	if after := readFiles(t, files); !reflect.DeepEqual(before, after) {
		t.Fatal("broken MCP file was changed")
	}
	if got := kimiPreference(t, layout); got != "absent" {
		t.Fatalf("broken MCP let the preference be written: %q", got)
	}
}

func TestKimiInstallRefusesAnUnreadableMCPPath(t *testing.T) {
	layout := testLayout(t)
	files, err := (kimiClient{}).Files(layout)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(files[1], 0o700); err != nil {
		t.Fatal(err)
	}
	if err := (kimiClient{}).Install(layout, nil); err == nil {
		t.Fatal("directory MCP path was accepted by install")
	}
	if got := readOrAbsent(t, files[0]); got != "absent" {
		t.Fatalf("failed MCP read wrote hooks: %s", got)
	}
	assertKimiWake(t, layout, false, false)
}

// The preference lives in the OS account's pinned directory, so nothing about Kimi's wake works
// without it.
func TestKimiWakeNeedsThePinnedDirectory(t *testing.T) {
	layout := testLayout(t)
	layout.PinnedConfigDir = func() (string, error) { return "", errors.New("no account directory") }
	if _, _, err := KimiWake(layout); err == nil {
		t.Fatal("read a preference without the pinned directory")
	}
	if err := (kimiClient{}).Install(layout, nil); err == nil {
		t.Fatal("installed without the pinned directory")
	}
	if err := InstallKimiWake(layout, false); err == nil {
		t.Fatal("turned wake off without the pinned directory")
	}
	if err := (kimiClient{}).Remove(layout, nil); err == nil {
		t.Fatal("removal did not report that no watcher could be stopped")
	}
	files, err := (kimiClient{}).Files(layout)
	if err != nil {
		t.Fatal(err)
	}
	if got := readOrAbsent(t, files[0]); got != "absent" {
		t.Fatalf("wrote hooks without the pinned directory: %s", got)
	}
}

// The watcher reads only the pinned configuration, so wake turns on only where that is the
// configuration in use. Integration without wake, and turning it off, work anywhere.
func TestKimiWakeOnNeedsThePinnedConfiguration(t *testing.T) {
	layout := testLayout(t)
	layout.Getenv = env(map[string]string{"IRIS_TOKEN_FILE": filepath.Join(layout.Home, "elsewhere", "hub.token")})
	files, err := (kimiClient{}).Files(layout)
	if err != nil {
		t.Fatal(err)
	}
	err = InstallKimiWake(layout, true)
	if err == nil || !strings.Contains(err.Error(), "kimi wake requires the default Iris configuration directory") {
		t.Fatalf("wake on with another configuration: %v", err)
	}
	if readOrAbsent(t, files[0]) != "absent" || kimiPreference(t, layout) != "absent" {
		t.Fatal("a refused --wake wrote files")
	}
	if err := (kimiClient{}).Install(layout, nil); err != nil {
		t.Fatal(err)
	}
	if got := kimiPreference(t, layout); got != "default\n" {
		t.Fatalf("plain integration with another configuration: %q", got)
	}
	if err := InstallKimiWake(layout, false); err != nil {
		t.Fatal(err)
	}
	if got := kimiPreference(t, layout); got != "off\n" {
		t.Fatalf("--no-wake with another configuration: %q", got)
	}

	unresolved := homeless(t)
	unresolved.Getenv = env(map[string]string{"KIMI_CODE_HOME": filepath.Join(layout.Home, "kimi")})
	unresolved.PinnedConfigDir = layout.PinnedConfigDir
	if err := InstallKimiWake(unresolved, true); err == nil {
		t.Fatal("wake on where no configuration directory resolves")
	}
}

// Iris adds its skill to text a person wrote and takes back only its own text. A skill a person
// changed is no longer Iris's, so it stays.
func TestKimiSkillKeepsTextAPersonWrote(t *testing.T) {
	layout := testLayout(t)
	skill := kimiSkillPath(t, layout)
	seedFile(t, skill, "# mine")
	for range 2 {
		if err := InstallKimiWake(layout, true); err != nil {
			t.Fatal(err)
		}
		if got := readOrAbsent(t, skill); got != "# mine\n"+kimiSkill {
			t.Fatalf("skill beside a person's text: %q", got)
		}
	}
	if err := InstallKimiWake(layout, false); err != nil {
		t.Fatal(err)
	}
	if got := readOrAbsent(t, skill); got != "# mine" {
		t.Fatalf("turning wake off did not give the person's text back: %q", got)
	}

	changed := testLayout(t)
	skill = kimiSkillPath(t, changed)
	if err := InstallKimiWake(changed, true); err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(kimiSkill, "exactly one replacement", "one replacement", 1)
	if edited == kimiSkill {
		t.Fatal("the edit changed nothing")
	}
	if err := os.WriteFile(skill, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := InstallKimiWake(changed, false); err != nil {
		t.Fatal(err)
	}
	if err := (kimiClient{}).Remove(changed, nil); err != nil {
		t.Fatal(err)
	}
	if got := readOrAbsent(t, skill); got != edited {
		t.Fatalf("a changed skill was not kept: %q", got)
	}
}

// With wake off, a drift check reports a skill that integration would take back, and changes
// nothing: not the skill and not the manifest that lets a later run take it back.
func TestKimiDriftReportsASkillWakeOffWouldTakeBack(t *testing.T) {
	layout := testLayout(t)
	client := kimiClient{}
	skill := kimiSkillPath(t, layout)
	if err := InstallKimiWake(layout, true); err != nil {
		t.Fatal(err)
	}
	root, err := KimiWakeRoot(layout)
	if err != nil {
		t.Fatal(err)
	}
	// The preference turned off by hand: the next integration takes the skill back.
	if err := os.WriteFile(kimiwake.PreferencePath(root), []byte("off\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		body  string
		drift bool
	}{
		{"Iris's skill", kimiSkill, true},
		{"emptied, so integration deletes it", "\n", true},
		{"changed by a person", "# mine\n", false},
	} {
		if err := os.WriteFile(skill, []byte(tc.body), 0o644); err != nil {
			t.Fatal(err)
		}
		drift, err := layout.Drift(client)
		if err != nil || slices.Contains(drift, skill) != tc.drift {
			t.Fatalf("%s: drift %v, %v", tc.name, drift, err)
		}
		if got := readOrAbsent(t, skill); got != tc.body {
			t.Fatalf("%s: the drift check changed the skill to %q", tc.name, got)
		}
	}
	if err := os.Remove(skill); err != nil {
		t.Fatal(err)
	}
	if drift, err := layout.Drift(client); err != nil || len(drift) != 0 {
		t.Fatalf("a skill already gone: drift %v, %v", drift, err)
	}
	if err := os.Mkdir(skill, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := layout.Drift(client); err == nil {
		t.Fatal("a skill that cannot be read was not reported")
	}
	if err := os.Remove(skill); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(skill, []byte(kimiSkill), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := client.Install(layout, nil); err != nil {
		t.Fatal(err)
	}
	if got := readOrAbsent(t, skill); got != "absent" {
		t.Fatalf("integration after the drift checks kept the skill: %q", got)
	}
}

// startKimiWatcher starts a stand-in watcher and records it, as a watcher records itself, as the
// live owner of session's record. The channel closes when it has ended.
func startKimiWatcher(t *testing.T, layout Layout, session string) <-chan struct{} {
	t.Helper()
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "-test.run=^$")
	cmd.Env = append(os.Environ(), kimiWatcherEnv+"=1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-done
	})
	child, ok := proc.Lookup(cmd.Process.Pid)
	if !ok {
		t.Fatal("the stand-in watcher has no process")
	}
	self, ok := proc.Lookup(os.Getpid())
	if !ok {
		t.Fatal("this test has no process")
	}
	root, err := KimiWakeRoot(layout)
	if err != nil {
		t.Fatal(err)
	}
	key := kimiwake.Key{PID: self.PID, StartMS: self.StartMS, Session: session}
	attempt := kimiwake.Attempt{PID: child.PID, StartMS: child.StartMS, ID: session}
	if owned, err := (&kimiwake.Store{Root: root}).Own(key, attempt, time.Second); err != nil || !owned {
		t.Fatalf("recording the stand-in watcher: %v %v", owned, err)
	}
	return done
}

func waitStopped(t *testing.T, done <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s left its watcher running", what)
	}
}

// Turning wake off and removing the integration stop the watchers a record shows alive, by their
// pid and start time, instead of leaving them to notice at their next check.
func TestKimiWakeOffAndRemovalStopTheLiveWatchers(t *testing.T) {
	layout := testLayout(t)
	if err := InstallKimiWake(layout, true); err != nil {
		t.Fatal(err)
	}
	done := startKimiWatcher(t, layout, "session_off")
	if err := InstallKimiWake(layout, false); err != nil {
		t.Fatal(err)
	}
	waitStopped(t, done, "--no-wake")
	done = startKimiWatcher(t, layout, "session_removed")
	if err := (kimiClient{}).Remove(layout, nil); err != nil {
		t.Fatal(err)
	}
	waitStopped(t, done, "removal")
}
