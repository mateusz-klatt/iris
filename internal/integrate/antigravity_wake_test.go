package integrate

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mateusz-klatt/iris/internal/config"
)

func TestAntigravityWakeOptInNoCommandAllowRuleAndRemove(t *testing.T) {
	l := testLayout(t)
	c, _ := ByName(Antigravity)

	// Fresh plain Install leaves wake off, writes no marker, has zero drift, and writes no command(...) rule.
	if err := c.Install(l, nil); err != nil {
		t.Fatal(err)
	}
	enabled, err := AntigravityWake(l)
	if err != nil || enabled {
		t.Fatalf("fresh install: enabled=%v err=%v", enabled, err)
	}
	configDir, err := l.IrisConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	markerPath := filepath.Join(configDir, antigravityWakeFile)
	if _, err := os.Stat(markerPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("plain install wrote wake marker: %v", err)
	}
	if drifted, err := l.Drift(c); err != nil || len(drifted) != 0 {
		t.Fatalf("drift = %v, %v", drifted, err)
	}

	// Opt-in --wake writes "on\n", survives plain Install, and still never writes command(...) to settings.json.
	if err := InstallAntigravityWake(l, true); err != nil {
		t.Fatal(err)
	}
	if enabled, err = AntigravityWake(l); err != nil || !enabled {
		t.Fatalf("--wake: enabled=%v err=%v", enabled, err)
	}
	settingsPath, err := AgySettings(l)
	if err != nil {
		t.Fatal(err)
	}
	settingsText := string(mustRead(t, settingsPath))
	if strings.Contains(settingsText, "command(") || !strings.Contains(settingsText, AgyAllowRule) {
		t.Fatalf("unexpected settings.json:\n%s", settingsText)
	}
	if err := c.Install(l, nil); err != nil {
		t.Fatal(err)
	}
	if enabled, err = AntigravityWake(l); err != nil || !enabled {
		t.Fatalf("plain reinstall cleared on: enabled=%v err=%v", enabled, err)
	}

	// --no-wake writes "off\n" and survives plain Install.
	if err := InstallAntigravityWake(l, false); err != nil {
		t.Fatal(err)
	}
	if enabled, err = AntigravityWake(l); err != nil || enabled {
		t.Fatalf("--no-wake: enabled=%v err=%v", enabled, err)
	}
	if err := c.Install(l, nil); err != nil {
		t.Fatal(err)
	}
	if enabled, err = AntigravityWake(l); err != nil || enabled {
		t.Fatalf("plain reinstall cleared off: enabled=%v err=%v", enabled, err)
	}

	// Remove deletes the wake marker alongside hooks, MCP entry and mcp(iris/*).
	if err := InstallAntigravityWake(l, true); err != nil {
		t.Fatal(err)
	}
	if err := c.Remove(l, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(markerPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Remove kept wake marker: %v", err)
	}
}

func TestAntigravityConfigDirVariantsAndErrors(t *testing.T) {
	home := t.TempDir()
	// IRIS_TOKEN_FILE wins.
	tokLayout := Layout{Home: home, Getenv: func(k string) string {
		if k == "IRIS_TOKEN_FILE" {
			return filepath.Join(home, "custom-cfg", "hub.token")
		}
		return ""
	}}
	if got, err := tokLayout.IrisConfigDir(); err != nil || got != filepath.Join(home, "custom-cfg") {
		t.Fatalf("IRIS_TOKEN_FILE = %q, %v", got, err)
	}
	// Windows with APPDATA vs fallback under Home.
	winEnv := Layout{Home: home, GOOS: "windows", Getenv: func(k string) string {
		if k == "APPDATA" {
			return filepath.Join(home, "WinAppData")
		}
		return ""
	}}
	if got, err := winEnv.IrisConfigDir(); err != nil || got != filepath.Join(home, "WinAppData", "iris") {
		t.Fatalf("windows APPDATA = %q, %v", got, err)
	}
	winFallback := Layout{Home: home, GOOS: "windows", Getenv: func(string) string { return "" }}
	if got, err := winFallback.IrisConfigDir(); err != nil || got != filepath.Join(home, "AppData", "Roaming", "iris") {
		t.Fatalf("windows fallback = %q, %v", got, err)
	}
	// macOS.
	macLayout := Layout{Home: home, GOOS: "darwin", Getenv: func(string) string { return "" }}
	if got, err := macLayout.IrisConfigDir(); err != nil || got != filepath.Join(home, "Library", "Application Support", "iris") {
		t.Fatalf("darwin = %q, %v", got, err)
	}
	// Linux XDG_CONFIG_HOME.
	xdgLayout := Layout{Home: home, GOOS: "linux", Getenv: func(k string) string {
		if k == "XDG_CONFIG_HOME" {
			return filepath.Join(home, "xdg")
		}
		return ""
	}}
	if got, err := xdgLayout.IrisConfigDir(); err != nil || got != filepath.Join(home, "xdg", "iris") {
		t.Fatalf("XDG = %q, %v", got, err)
	}

	// Homeless / broken files / unreadable marker errors.
	nowhere := homeless(t)
	if err := InstallAntigravityWake(nowhere, true); err == nil {
		t.Fatal("InstallAntigravityWake without home succeeded")
	}
	if _, err := AntigravityWake(nowhere); err == nil {
		t.Fatal("AntigravityWake without home succeeded")
	}

	brokenInstall := testLayout(t)
	files, _ := (antigravityClient{}).Files(brokenInstall)
	if err := os.MkdirAll(files[0], 0o700); err != nil {
		t.Fatal(err)
	}
	if err := InstallAntigravityWake(brokenInstall, true); err == nil {
		t.Fatal("broken hooks.json install succeeded")
	}

	badMarker := testLayout(t)
	cfgDir, _ := badMarker.IrisConfigDir()
	if err := os.MkdirAll(filepath.Join(cfgDir, antigravityWakeFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := InstallAntigravityWake(badMarker, true); err == nil {
		t.Fatal("directory at wake marker path succeeded on update")
	}
	if _, err := AntigravityWake(badMarker); err == nil {
		t.Fatal("directory at wake marker path succeeded on read")
	}
}

func TestAntigravityConfigDirSplitAndPinnedFallback(t *testing.T) {
	c, _ := ByName(Antigravity)

	// 1. Default PinnedConfigDir (nil seam -> hubclient.PinnedConfigDir) refuses --wake when Layout.Home is a temp dir, naming both paths without writing files.
	tempHome := t.TempDir()
	defaultPinned := Layout{
		Home:     tempHome,
		StateDir: filepath.Join(tempHome, "state"),
		Getenv:   func(string) string { return "" },
		GOOS:     "linux",
	}
	wantEnvCfg := filepath.Join(tempHome, ".config", "iris")
	err := InstallAntigravityWake(defaultPinned, true)
	if err == nil || !strings.Contains(err.Error(), wantEnvCfg) {
		t.Fatalf("expected split-dir refusal naming %q, got %v", wantEnvCfg, err)
	}
	if _, statErr := os.Stat(wantEnvCfg); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("refused --wake created %s: %v", wantEnvCfg, statErr)
	}

	// 2. PinnedConfigDir error fails closed on --wake, --no-wake and --remove.
	pinErrLayout := testLayout(t)
	pinErrLayout.PinnedConfigDir = func() (string, error) { return "", errors.New("pinned lookup failed") }
	if err := InstallAntigravityWake(pinErrLayout, true); err == nil || !strings.Contains(err.Error(), "pinned lookup failed") {
		t.Fatalf("--wake pinnedErr = %v", err)
	}
	if err := InstallAntigravityWake(pinErrLayout, false); err == nil || !strings.Contains(err.Error(), "pinned lookup failed") {
		t.Fatalf("--no-wake pinnedErr = %v", err)
	}
	if err := c.Remove(pinErrLayout, nil); err == nil || !strings.Contains(err.Error(), "pinned lookup failed") {
		t.Fatalf("--remove pinnedErr = %v", err)
	}

	// 3. Windows case-insensitive config dir equality succeeds on --wake.
	winHome := t.TempDir()
	winCfg := filepath.Join(winHome, "AppData", "Roaming", "iris")
	winLayout := testLayout(t)
	winLayout.Home = winHome
	winLayout.StateDir = filepath.Join(winHome, "state")
	winLayout.GOOS = "windows"
	winLayout.PinnedConfigDir = func() (string, error) { return strings.ToUpper(winCfg), nil }
	if err := InstallAntigravityWake(winLayout, true); err != nil {
		t.Fatalf("windows case-insensitive --wake failed: %v", err)
	}

	// 4. Split directories: --wake refuses naming both paths; --no-wake and --remove both write "off\n" to pinnedDir idempotently.
	pinnedDir := filepath.Join(t.TempDir(), "pinned-iris")
	if err := os.MkdirAll(pinnedDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pinnedDir, antigravityWakeFile), []byte("on\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	splitLayout := testLayout(t)
	splitCfg, _ := splitLayout.IrisConfigDir()
	splitLayout.PinnedConfigDir = func() (string, error) { return pinnedDir, nil }

	err = InstallAntigravityWake(splitLayout, true)
	if err == nil || !strings.Contains(err.Error(), pinnedDir) || !strings.Contains(err.Error(), splitCfg) {
		t.Fatalf("split --wake error must name both %q and %q, got %v", pinnedDir, splitCfg, err)
	}

	for i := 0; i < 2; i++ {
		if err := InstallAntigravityWake(splitLayout, false); err != nil {
			t.Fatalf("split --no-wake pass %d: %v", i, err)
		}
	}
	if got := string(mustRead(t, filepath.Join(pinnedDir, antigravityWakeFile))); got != "off\n" {
		t.Fatalf("pinned marker after split --no-wake = %q", got)
	}
	if got := string(mustRead(t, filepath.Join(splitCfg, antigravityWakeFile))); got != "off\n" {
		t.Fatalf("env marker after split --no-wake = %q", got)
	}

	// Re-seed pinned marker to "on\n" and verify split --remove also writes "off\n" to pinnedDir idempotently.
	if err := os.WriteFile(filepath.Join(pinnedDir, antigravityWakeFile), []byte("on\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := c.Remove(splitLayout, nil); err != nil {
			t.Fatalf("split --remove pass %d: %v", i, err)
		}
	}
	if got := string(mustRead(t, filepath.Join(pinnedDir, antigravityWakeFile))); got != "off\n" {
		t.Fatalf("pinned marker after split --remove = %q", got)
	}

	// 5. Same configDir with a separate StateDir (where unwire did not track antigravity-wake) still turns "on" to "off\n" on Remove.
	samePinned := testLayout(t)
	sameCfg, _ := samePinned.IrisConfigDir()
	if err := os.MkdirAll(sameCfg, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sameCfg, antigravityWakeFile), []byte("on\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := c.Remove(samePinned, nil); err != nil {
		t.Fatalf("same-dir untracked marker Remove: %v", err)
	}
	if got := string(mustRead(t, filepath.Join(sameCfg, antigravityWakeFile))); got != "off\n" {
		t.Fatalf("same-dir untracked marker after Remove = %q", got)
	}

	// 6. Homeless --no-wake and --remove still disable pinnedDir while returning the IrisConfigDir error on --no-wake;
	// broken Install on split --no-wake and non-regular pinned marker path also return errors cleanly.
	if err := os.WriteFile(filepath.Join(pinnedDir, antigravityWakeFile), []byte("on\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	homelessPinned := homeless(t)
	homelessPinned.PinnedConfigDir = func() (string, error) { return pinnedDir, nil }
	if err := InstallAntigravityWake(homelessPinned, false); err == nil {
		t.Fatal("homeless --no-wake succeeded")
	}
	if got := string(mustRead(t, filepath.Join(pinnedDir, antigravityWakeFile))); got != "off\n" {
		t.Fatalf("homeless --no-wake did not disable pinned marker: %q", got)
	}
	if err := os.WriteFile(filepath.Join(pinnedDir, antigravityWakeFile), []byte("on\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := c.Remove(homelessPinned, nil); err == nil {
		t.Fatal("homeless Remove succeeded without state dir")
	}
	if got := string(mustRead(t, filepath.Join(pinnedDir, antigravityWakeFile))); got != "off\n" {
		t.Fatalf("homeless Remove did not disable pinned marker: %q", got)
	}
	if dirs, err := AntigravityConfigDirs(homelessPinned); err == nil || len(dirs) != 1 || dirs[0] != pinnedDir {
		t.Fatalf("AntigravityConfigDirs(homelessPinned) = %v, %v", dirs, err)
	}
	if dirs, err := AntigravityConfigDirs(splitLayout); err != nil || len(dirs) != 2 {
		t.Fatalf("AntigravityConfigDirs(splitLayout) = %v, %v", dirs, err)
	}
	if dirs, err := AntigravityConfigDirs(samePinned); err != nil || len(dirs) != 1 {
		t.Fatalf("AntigravityConfigDirs(samePinned) = %v, %v", dirs, err)
	}

	brokenNoWake := testLayout(t)
	brokenNoWake.PinnedConfigDir = func() (string, error) { return pinnedDir, nil }
	brokenFiles, _ := c.Files(brokenNoWake)
	if err := os.MkdirAll(brokenFiles[0], 0o700); err != nil {
		t.Fatal(err)
	}
	if err := InstallAntigravityWake(brokenNoWake, false); err == nil {
		t.Fatal("broken hooks.json --no-wake succeeded")
	}

	badPinnedDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(badPinnedDir, antigravityWakeFile), 0o700); err != nil {
		t.Fatal(err)
	}
	badPinnedLayout := testLayout(t)
	badPinnedLayout.PinnedConfigDir = func() (string, error) { return badPinnedDir, nil }
	if err := InstallAntigravityWake(badPinnedLayout, false); err == nil {
		t.Fatal("directory at pinned marker path succeeded on --no-wake")
	}
}

func TestAntigravityNilPinnedInjectionIsolatesRemoveAndNoWake(t *testing.T) {
	c, _ := ByName(Antigravity)
	isolatedPinned, err := config.PinnedDir()
	if err != nil || !strings.HasPrefix(filepath.Clean(isolatedPinned), filepath.Clean(os.TempDir())) {
		t.Fatalf("TestMain PinnedDir = %q, %v (temp %q)", isolatedPinned, err, os.TempDir())
	}
	if err := os.MkdirAll(isolatedPinned, 0o700); err != nil {
		t.Fatal(err)
	}

	outsideAccount := filepath.Join(t.TempDir(), "outside-account-iris")
	if err := os.MkdirAll(outsideAccount, 0o700); err != nil {
		t.Fatal(err)
	}
	outsideMarker := filepath.Join(outsideAccount, antigravityWakeFile)
	if err := os.WriteFile(outsideMarker, []byte("on\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	beforeInfo, err := os.Stat(outsideMarker)
	if err != nil {
		t.Fatal(err)
	}

	seedIsolatedOn := func() {
		t.Helper()
		if err := os.WriteFile(filepath.Join(isolatedPinned, antigravityWakeFile), []byte("on\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	assertIsolatedOffAndOutsideUntouched := func(step string) {
		t.Helper()
		if got := string(mustRead(t, filepath.Join(isolatedPinned, antigravityWakeFile))); got != "off\n" {
			t.Fatalf("%s: isolated marker = %q, want off", step, got)
		}
		afterInfo, statErr := os.Stat(outsideMarker)
		if statErr != nil || !afterInfo.ModTime().Equal(beforeInfo.ModTime()) || string(mustRead(t, outsideMarker)) != "on\n" {
			t.Fatalf("%s: outside account marker changed (%v, mtime %v -> %v)", step, statErr, beforeInfo.ModTime(), afterInfo.ModTime())
		}
	}

	// 1. Homeless Layout with nil PinnedConfigDir on --no-wake and Remove touches only TestMain's isolatedPinned.
	seedIsolatedOn()
	if err := InstallAntigravityWake(homeless(t), false); err == nil {
		t.Fatal("homeless --no-wake succeeded")
	}
	assertIsolatedOffAndOutsideUntouched("homeless --no-wake")

	seedIsolatedOn()
	if err := c.Remove(homeless(t), nil); err == nil {
		t.Fatal("homeless Remove succeeded")
	}
	assertIsolatedOffAndOutsideUntouched("homeless Remove")

	// 2. Normal temp-Home Layout with nil PinnedConfigDir on --no-wake and Remove touches only tempHome and isolatedPinned.
	tempHome := t.TempDir()
	nilPinnedLayout := Layout{
		Home:     tempHome,
		StateDir: filepath.Join(tempHome, "state"),
		Getenv:   func(string) string { return "" },
		GOOS:     "linux",
		LookPath: func(name string) (string, error) {
			if name == "iris" {
				return "/usr/local/bin/iris", nil
			}
			return "", errors.New("not found")
		},
	}
	seedIsolatedOn()
	if err := InstallAntigravityWake(nilPinnedLayout, false); err != nil {
		t.Fatalf("nilPinnedLayout --no-wake: %v", err)
	}
	assertIsolatedOffAndOutsideUntouched("nilPinnedLayout --no-wake")

	seedIsolatedOn()
	if err := c.Remove(nilPinnedLayout, nil); err != nil {
		t.Fatalf("nilPinnedLayout Remove: %v", err)
	}
	assertIsolatedOffAndOutsideUntouched("nilPinnedLayout Remove")
}
