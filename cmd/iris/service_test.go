package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mateusz-klatt/iris/internal/service"
)

func TestUserServiceDoesNotNeedIrisOnPath(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	mgr, err := userService("127.0.0.1:1", filepath.Join(t.TempDir(), "hub.token"))
	if err != nil || mgr == nil {
		t.Fatalf("%v %v", mgr, err)
	}
}

func TestServiceBinary(t *testing.T) {
	missing := func(string) (string, error) { return "", errors.New("not on PATH") }
	self := func() (string, error) { return "/running/iris", nil }

	found := func(name string) (string, error) {
		if name != "iris" {
			t.Fatal(name)
		}
		return "iris-on-path", nil
	}
	want, err := filepath.Abs("iris-on-path")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := serviceBinary(found, self); got != want || err != nil {
		t.Fatalf("PATH wins: %q %v", got, err)
	}
	if got, err := serviceBinary(missing, self); got != "/running/iris" || err != nil {
		t.Fatalf("a logon without iris on PATH: %q %v", got, err)
	}
	broken := errors.New("no executable path")
	if _, err := serviceBinary(missing, func() (string, error) { return "", broken }); !errors.Is(err, broken) {
		t.Fatal(err)
	}
}

// serviceSandbox gives each test its own configuration directory and records the
// command line the manager was built with.
func serviceSandbox(t *testing.T) *[]string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	t.Setenv("AppData", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	orig, origAbs := hostService, absPath
	t.Cleanup(func() { hostService, absPath = orig, origAbs })
	var argv []string
	hostService = func(_, _ string, got []string) service.Manager {
		argv = got
		return scriptedService{}
	}
	return &argv
}

func flagValue(argv []string, name string) string {
	for i, arg := range argv {
		if arg == name && i+1 < len(argv) {
			return argv[i+1]
		}
	}
	return ""
}

func TestLifecycleCommandsFindTheInstalledHub(t *testing.T) {
	argv := serviceSandbox(t)
	dir := t.TempDir()
	db, token := filepath.Join(dir, "custom.db"), filepath.Join(dir, "custom.token")
	capture(t, func() {
		if code := runService([]string{"install", "--listen", "127.0.0.1:9999", "--db", db, "--token-file", token}); code != 0 {
			t.Fatal(code)
		}
	})
	for _, verb := range []string{"stop", "start", "status", "uninstall"} {
		*argv = nil
		capture(t, func() {
			if code := runService([]string{verb}); code != 0 {
				t.Fatal(verb, code)
			}
		})
		if flagValue(*argv, "--listen") != "127.0.0.1:9999" || flagValue(*argv, "--db") != db || flagValue(*argv, "--token-file") != token {
			t.Fatalf("%s rebuilt other settings: %v", verb, *argv)
		}
	}
	// Uninstalling forgets them, so a later install starts clean.
	saved, err := loadServiceSettings()
	if err != nil || saved != (serviceSettings{}) {
		t.Fatalf("%+v %v", saved, err)
	}
}

func TestAFlagOverridesTheInstalledSettings(t *testing.T) {
	argv := serviceSandbox(t)
	if err := saveServiceSettings(serviceSettings{Listen: "127.0.0.1:1"}); err != nil {
		t.Fatal(err)
	}
	capture(t, func() {
		if code := runService([]string{"status", "--listen", "127.0.0.1:2"}); code != 0 {
			t.Fatal(code)
		}
	})
	if got := flagValue(*argv, "--listen"); got != "127.0.0.1:2" {
		t.Fatal(got)
	}
}

func TestRelativePathsBecomeAbsolute(t *testing.T) {
	argv := serviceSandbox(t)
	t.Chdir(t.TempDir())
	capture(t, func() {
		if code := runService([]string{"install", "--db", "hub.db", "--token-file", "hub.token"}); code != 0 {
			t.Fatal(code)
		}
	})
	for _, name := range []string{"--db", "--token-file"} {
		if got := flagValue(*argv, name); !filepath.IsAbs(got) {
			t.Fatalf("%s saved as %q", name, got)
		}
	}
	saved, err := loadServiceSettings()
	if err != nil || !filepath.IsAbs(saved.Database) || !filepath.IsAbs(saved.TokenFile) {
		t.Fatalf("%+v %v", saved, err)
	}
}

func TestADefaultInstallStaysOnTheDefaults(t *testing.T) {
	serviceSandbox(t)
	if err := installUserService(context.Background(), "", "", ""); err != nil {
		t.Fatal(err)
	}
	saved, err := loadServiceSettings()
	if err != nil || saved != (serviceSettings{}) {
		t.Fatalf("%+v %v", saved, err)
	}
}

func TestAFailedInstallRecordsNothing(t *testing.T) {
	serviceSandbox(t)
	hostService = func(string, string, []string) service.Manager {
		return scriptedService{err: errors.New("unit failed")}
	}
	capture(t, func() {
		if code := runService([]string{"install", "--listen", "127.0.0.1:1"}); code != 1 {
			t.Fatal(code)
		}
	})
	saved, err := loadServiceSettings()
	if err != nil || saved != (serviceSettings{}) {
		t.Fatalf("%+v %v", saved, err)
	}
}

func TestUnreadableSettingsStopTheCommand(t *testing.T) {
	serviceSandbox(t)
	path, err := serviceSettingsPath()
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, errText := capture(t, func() {
		if code := runService([]string{"uninstall"}); code != 1 {
			t.Fatal(code)
		}
	})
	if !strings.Contains(errText, "service.json") {
		t.Fatal(errText)
	}
	// A directory there can neither be read nor replaced.
	if err = os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(path, "kept"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err = loadServiceSettings(); err == nil {
		t.Fatal("read a directory")
	}
	if err = saveServiceSettings(serviceSettings{}); err == nil {
		t.Fatal("wrote over a directory")
	}
	if err = removeServiceSettings(); err == nil {
		t.Fatal("removed a directory with something in it")
	}
}

func TestAnInstallThatCannotRecordItsSettingsFails(t *testing.T) {
	serviceSandbox(t)
	path, err := serviceSettingsPath()
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(filepath.Join(path, "kept"), 0o700); err != nil {
		t.Fatal(err)
	}
	capture(t, func() {
		if code := runService([]string{"install"}); code != 1 {
			t.Fatal(code)
		}
	})
}

func TestUninstallReportsSettingsItCannotRemove(t *testing.T) {
	serviceSandbox(t)
	if err := saveServiceSettings(serviceSettings{}); err != nil {
		t.Fatal(err)
	}
	path, err := serviceSettingsPath()
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, []byte("{}"), 0o400); err != nil {
		t.Fatal(err)
	}
	// Replace the file with a directory holding something, which Remove refuses,
	// after the read has already succeeded.
	orig := hostService
	hostService = func(home, xdg string, argv []string) service.Manager {
		_ = os.Remove(path)
		_ = os.MkdirAll(filepath.Join(path, "kept"), 0o700)
		return orig(home, xdg, argv)
	}
	capture(t, func() {
		if code := runService([]string{"uninstall"}); code != 1 {
			t.Fatal(code)
		}
	})
}

func TestSettingsWithoutAConfigDirectory(t *testing.T) {
	serviceSandbox(t)
	for _, key := range []string{"HOME", "USERPROFILE", "AppData", "XDG_CONFIG_HOME"} {
		t.Setenv(key, "")
	}
	if _, err := loadServiceSettings(); err == nil {
		t.Fatal("load")
	}
	if err := saveServiceSettings(serviceSettings{}); err == nil {
		t.Fatal("save")
	}
	if err := removeServiceSettings(); err == nil {
		t.Fatal("remove")
	}
}

func TestSettingsDirectoryThatCannotBeCreated(t *testing.T) {
	serviceSandbox(t)
	file := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", file)
	t.Setenv("AppData", file)
	t.Setenv("HOME", file)
	if err := saveServiceSettings(serviceSettings{}); err == nil {
		t.Fatal("no error")
	}
}

func TestPathsThatCannotBeResolved(t *testing.T) {
	serviceSandbox(t)
	failing := func(name string) func(string) (string, error) {
		return func(p string) (string, error) {
			if strings.Contains(p, name) {
				return "", errors.New("cwd gone")
			}
			return p, nil
		}
	}
	for _, name := range []string{"hub.token", "hub.db"} {
		absPath = failing(name)
		if _, err := userServiceDB("", "", ""); err == nil {
			t.Fatal(name)
		}
	}
	if err := installUserService(context.Background(), "", "", ""); err == nil {
		t.Fatal("install built a manager it could not")
	}
}

func TestAbsoluteKeepsAnEmptyPath(t *testing.T) {
	if got := absolute(""); got != "" {
		t.Fatal(got)
	}
	if first("", "b") != "b" || first("a", "b") != "a" {
		t.Fatal("first")
	}
}

func TestUninstallWithoutSettingsIsFine(t *testing.T) {
	serviceSandbox(t)
	if err := removeServiceSettings(); err != nil {
		t.Fatal(err)
	}
}
