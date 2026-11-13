package integrate

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCopilotIntegrationInstallsAndRemovesItsExtension(t *testing.T) {
	home := t.TempDir()
	copilotHome := filepath.Join(home, "copilot home")
	iris := filepath.Join(home, "tools", "iris with \"quotes\"")
	l := Layout{
		Home:     home,
		StateDir: filepath.Join(home, "state"),
		Getenv: env(map[string]string{
			"COPILOT_HOME": copilotHome,
		}),
		LookPath: func(name string) (string, error) {
			if name == "iris" {
				return iris, nil
			}
			return "", os.ErrNotExist
		},
		GOOS: "linux",
	}
	client, ok := ByName(Copilot)
	if !ok {
		t.Fatal("Copilot integration is not registered")
	}
	files, err := client.Files(l)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 4 ||
		files[2] != filepath.Join(copilotHome, "extensions", "iris", "watcher.mjs") ||
		files[3] != filepath.Join(copilotHome, "extensions", "iris", "extension.mjs") {
		t.Fatalf("unexpected Copilot files: %v", files)
	}
	if err := client.Install(l, nil); err != nil {
		t.Fatal(err)
	}
	extension, err := os.ReadFile(files[3])
	if err != nil {
		t.Fatal(err)
	}
	quoted, _ := json.Marshal(iris)
	if !strings.Contains(string(extension), string(quoted)) ||
		strings.Contains(string(extension), "__IRIS_EXECUTABLE__") {
		t.Fatalf("extension does not contain the escaped Iris executable path:\n%s", extension)
	}
	watcher, err := os.ReadFile(files[2])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(watcher), `source: "automation-iris"`) ||
		!strings.Contains(string(watcher), `mode: "enqueue"`) {
		t.Fatalf("unexpected watcher:\n%s", watcher)
	}
	if err := client.Remove(l, nil); err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if _, err := os.Stat(file); !os.IsNotExist(err) {
			t.Errorf("Copilot file remains after remove: %s (%v)", file, err)
		}
	}
}

func TestCopilotInstallRefusesToReplaceAnExistingExtension(t *testing.T) {
	l := testLayout(t)
	client, _ := ByName(Copilot)
	files, err := client.Files(l)
	if err != nil {
		t.Fatal(err)
	}
	seedFile(t, files[3], "export const userExtension = true;\n")
	if err := client.Install(l, nil); !errors.Is(err, ErrBroken) {
		t.Fatal(err)
	}
	if got := string(mustRead(t, files[3])); got != "export const userExtension = true;\n" {
		t.Fatalf("existing extension changed: %q", got)
	}
	for _, file := range files[:3] {
		if _, err := os.Stat(file); !os.IsNotExist(err) {
			t.Errorf("partial integration wrote %s before rejecting the extension collision", file)
		}
	}
}

func TestCopilotInstallReportsAnUnreadableExtensionPath(t *testing.T) {
	l := testLayout(t)
	client, _ := ByName(Copilot)
	files, err := client.Files(l)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(files[2]), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(files[2], 0o700); err != nil {
		t.Fatal(err)
	}
	if err := client.Install(l, nil); !errors.Is(err, ErrBroken) {
		t.Fatal(err)
	}
	for _, file := range files[:2] {
		if _, err := os.Stat(file); !os.IsNotExist(err) {
			t.Errorf("partial integration wrote %s before reporting the extension path", file)
		}
	}
}

func TestCopilotInstallReportsAnExtensionWriteError(t *testing.T) {
	l := testLayout(t)
	client, _ := ByName(Copilot)
	files, err := client.Files(l)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Install(l, nil); err != nil {
		t.Fatal(err)
	}
	for _, file := range files[2:] {
		if err := os.Remove(file); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(manifestPath(l.StateDir), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := client.Install(l, nil); err == nil || !strings.Contains(err.Error(), "manifest is unreadable") {
		t.Fatal(err)
	}
}

func TestCopilotInstallReportsAnUnreadableManifestBeforeAdoptingItsFiles(t *testing.T) {
	l := testLayout(t)
	client, _ := ByName(Copilot)
	if err := client.Install(l, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath(l.StateDir), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := client.Install(l, nil); err == nil || !strings.Contains(err.Error(), "manifest is unreadable") {
		t.Fatal(err)
	}
}

// What a release of Iris wrote is Iris's to replace, whatever its content or the iris path it
// named, so an upgrade or a moved binary reintegrates instead of being refused.
func TestCopilotInstallReplacesFilesAnOlderIrisWrote(t *testing.T) {
	l := testLayout(t)
	client, _ := ByName(Copilot)
	files, err := client.Files(l)
	if err != nil {
		t.Fatal(err)
	}
	seedFile(t, files[2], string(ownedCopilotFile([]byte("// an older watcher\n"))))
	seedFile(t, files[3], string(ownedCopilotFile([]byte("// an older extension\n"))))
	if err := client.Install(l, nil); err != nil {
		t.Fatal(err)
	}
	if got := string(mustRead(t, files[2])); got != string(ownedCopilotFile([]byte(copilotWatcher))) {
		t.Fatalf("older watcher was not replaced:\n%s", got)
	}
	moved := filepath.Join(l.Home, "moved", "iris")
	l.LookPath = func(string) (string, error) { return moved, nil }
	if err := client.Install(l, nil); err != nil {
		t.Fatal(err)
	}
	quoted, _ := json.Marshal(moved)
	got := mustRead(t, files[3])
	if !strings.Contains(string(got), string(quoted)) || !irisCopilotFile(got) {
		t.Fatalf("extension does not name the moved iris:\n%s", got)
	}
}

// A recorded extension file that no longer matches is somebody's now, emptied included. Removal
// keeps it and its record, fails naming it, and still takes out everything else; once the file is
// moved aside, the same command finishes.
func TestCopilotRemoveKeepsAnExtensionSomebodyChanged(t *testing.T) {
	for name, change := range map[string]func(string) string{
		"edited":          func(s string) string { return s + "// kept\n" },
		"emptied":         func(string) string { return "" },
		"an empty object": func(string) string { return "{}\n" },
	} {
		t.Run(name, func(t *testing.T) {
			l := testLayout(t)
			client, _ := ByName(Copilot)
			files, err := client.Files(l)
			if err != nil {
				t.Fatal(err)
			}
			if err := client.Install(l, nil); err != nil {
				t.Fatal(err)
			}
			changed := change(string(mustRead(t, files[3])))
			seedFile(t, files[3], changed)
			if err := client.Install(l, nil); !errors.Is(err, ErrBroken) {
				t.Fatal(err)
			}
			if err := client.Remove(l, nil); !errors.Is(err, errChangedExtension) || !strings.Contains(err.Error(), files[3]) {
				t.Fatalf("remove: %v", err)
			}
			if got := string(mustRead(t, files[3])); got != changed {
				t.Fatalf("changed extension was not kept:\n%s", got)
			}
			for _, file := range files[:3] {
				if _, err := os.Stat(file); !os.IsNotExist(err) {
					t.Errorf("%s remains after remove: %v", file, err)
				}
			}
			if m, err := loadManifest(l.StateDir); err != nil || len(m.Files) != 1 || m.Files[files[3]].Client != Copilot {
				t.Fatalf("manifest after remove: %v, %v", m.Files, err)
			}
			aside := filepath.Join(l.Home, "extension.mjs")
			if err := os.Rename(files[3], aside); err != nil {
				t.Fatal(err)
			}
			if err := client.Remove(l, nil); err != nil {
				t.Fatal(err)
			}
			if m, err := loadManifest(l.StateDir); err != nil || len(m.Files) != 0 {
				t.Fatalf("manifest after the retry: %v, %v", m.Files, err)
			}
			if got := string(mustRead(t, aside)); got != changed {
				t.Fatalf("the file moved aside changed:\n%s", got)
			}
		})
	}
}

// The extension files prove that Iris wrote them, so removal deletes them with no manifest to
// record them.
func TestCopilotRemoveDeletesItsExtensionAfterTheManifestWasLost(t *testing.T) {
	l := testLayout(t)
	client, _ := ByName(Copilot)
	files, err := client.Files(l)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Install(l, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(manifestPath(l.StateDir)); err != nil {
		t.Fatal(err)
	}
	if err := client.Remove(l, nil); err != nil {
		t.Fatal(err)
	}
	for _, file := range files[2:] {
		if _, err := os.Stat(file); !os.IsNotExist(err) {
			t.Errorf("%s remains after remove: %v", file, err)
		}
	}
}

// With no manifest, Iris cannot tell a changed extension file from one that was never its own.
// Removal deletes the file that still matches and keeps the changed one without failing and
// without recording it.
func TestCopilotRemoveWithoutAManifestKeepsAChangedExtensionWithoutAMessage(t *testing.T) {
	l := testLayout(t)
	client, _ := ByName(Copilot)
	files, err := client.Files(l)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Install(l, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(manifestPath(l.StateDir)); err != nil {
		t.Fatal(err)
	}
	changed := string(mustRead(t, files[3])) + "// kept\n"
	seedFile(t, files[3], changed)
	if err := client.Remove(l, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(files[2]); !os.IsNotExist(err) {
		t.Errorf("unchanged watcher remains after remove: %v", err)
	}
	if got := string(mustRead(t, files[3])); got != changed {
		t.Fatalf("changed extension was not kept:\n%s", got)
	}
	if m, err := loadManifest(l.StateDir); err != nil || len(m.Files) != 0 {
		t.Fatalf("manifest after remove: %v, %v", m.Files, err)
	}
}

func TestCopilotRemoveReportsAnUnreadableManifestAndDeletesNothing(t *testing.T) {
	l := testLayout(t)
	client, _ := ByName(Copilot)
	files, err := client.Files(l)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Install(l, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath(l.StateDir), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := client.Remove(l, nil); err == nil || !strings.Contains(err.Error(), "manifest is unreadable") {
		t.Fatal(err)
	}
	for _, file := range files {
		if _, err := os.Stat(file); err != nil {
			t.Errorf("%s: %v", file, err)
		}
	}
}

// Remove must not depend on the iris that is first on the PATH now: an upgrade or a moved binary
// would leave the extension behind. Files written before a manifest existed are removed as well.
func TestCopilotRemoveDeletesFilesWrittenForAnotherIris(t *testing.T) {
	l := testLayout(t)
	client, _ := ByName(Copilot)
	files, err := client.Files(l)
	if err != nil {
		t.Fatal(err)
	}
	seedFile(t, files[2], string(ownedCopilotFile([]byte("// an older watcher\n"))))
	seedFile(t, files[3], string(ownedCopilotFile([]byte("// an older extension\n"))))
	if err := client.Install(l, nil); err != nil {
		t.Fatal(err)
	}
	l.LookPath = func(string) (string, error) { return filepath.Join(l.Home, "moved", "iris"), nil }
	if err := client.Remove(l, nil); err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if _, err := os.Stat(file); !os.IsNotExist(err) {
			t.Errorf("Copilot file remains after remove: %s (%v)", file, err)
		}
	}
}
