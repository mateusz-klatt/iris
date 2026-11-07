package doctor

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/mateusz-klatt/iris/internal/integrate"
)

// noPlugins lets Claude's installer run without the plugin CLI.
type noPlugins struct{}

func (noPlugins) MarketplaceAdd(string) error    { return nil }
func (noPlugins) Install(string) error           { return nil }
func (noPlugins) Uninstall(string) error         { return nil }
func (noPlugins) MarketplaceRemove(string) error { return nil }

// Every command form iris integrate installs on this system, whether a shell line, a guard,
// cmd, PowerShell, Codex's commandWindows or an argv list, must carry the real iris hook's
// stderr back to doctor. A form that ate it would make every client look like a stand-in.
// This builds iris from this tree, so CI runs it on each operating system.
func TestTheRealHookAnswersTheCanaryThroughEveryInstalledForm(t *testing.T) {
	if testing.Short() {
		t.Skip("builds iris")
	}
	home := t.TempDir()
	iris := filepath.Join(home, "bin", "iris")
	if runtime.GOOS == "windows" {
		iris += ".exe"
	}
	build := exec.Command("go", "build", "-o", iris, "github.com/mateusz-klatt/iris/cmd/iris")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build iris: %v: %s", err, out)
	}
	// The hook fails open fast against a hub that is not there; the receipt does not need one.
	t.Setenv("IRIS_HUB_URL", "http://127.0.0.1:9")
	t.Setenv("IRIS_TOKEN_FILE", filepath.Join(home, "hub.token"))
	layout := integrate.Layout{
		Home:     home,
		StateDir: filepath.Join(home, "state"),
		Getenv:   func(string) string { return "" },
		LookPath: func(name string) (string, error) {
			if name == "iris" {
				return iris, nil
			}
			return "", exec.ErrNotFound
		},
		GOOS: runtime.GOOS,
	}
	for _, c := range integrate.All() {
		if err := c.Install(layout, noPlugins{}); err != nil {
			t.Fatalf("%s: %v", c.Name(), err)
		}
	}
	for _, c := range integrate.All() {
		row := doctorRow(t, layout, c.Name())
		if row.Status == StatusSilent || strings.Contains(row.Detail, "not from Iris") {
			t.Errorf("%s: %+v", c.Name(), row)
		}
	}
}
