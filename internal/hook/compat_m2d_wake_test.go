//go:build compat

package hook

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// The overlay puts only the test driver in the historical package. All worker
// code and wire types come from the checkout being built.
func compatBuildWakeDriver(t *testing.T, root string, ref compatHistoricalRef) string {
	t.Helper()
	return compatBuildPackageDriver(t, root, ref, "internal/wake", "compat_m2d_driver_test.go")
}

func compatBuildPackageDriver(t *testing.T, root string, ref compatHistoricalRef, pkg, driver string) string {
	t.Helper()
	source := filepath.Join(pkg, driver)
	dir := t.TempDir()
	bin := filepath.Join(dir, filepath.Base(pkg)+".test")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	args := []string{"test", "-c", "-tags", "compat", "-modcacherw", "-buildvcs=false", "-o", bin}
	buildDir := root
	if ref.commit != "" {
		wt, remove := compatHistoricalWorktree(t, root, ref)
		defer remove()
		buildDir = wt
		overlay, err := json.Marshal(map[string]map[string]string{
			"Replace": {filepath.Join(wt, source): filepath.Join(root, source)},
		})
		if err != nil {
			t.Fatal(err)
		}
		file := filepath.Join(dir, "overlay.json")
		if err := os.WriteFile(file, overlay, 0o600); err != nil {
			t.Fatal(err)
		}
		args = append(args, "-overlay", file)
	}
	cmd := exec.Command("go", append(args, "./"+pkg)...)
	cmd.Dir = buildDir
	cmd.Env = compatGoEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build %s driver %s: %v\n%s", pkg, ref.name, err, out)
	}
	return bin
}

func compatWakeConfig(t *testing.T, hub *liveCompatHub, proxyURL string, opted bool) string {
	t.Helper()
	dir := t.TempDir()
	token, err := os.ReadFile(hub.tokenFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "hub.token"), token, 0o600); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(map[string]any{"hub_url": proxyURL, "session_succession": opted})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "client.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

type compatWakeDriver struct {
	stdin  io.WriteCloser
	done   chan struct{}
	err    error
	output bytes.Buffer
}

func compatStartWakeDriver(t *testing.T, bin, dir, cwd, client, session string) *compatWakeDriver {
	t.Helper()
	d := &compatWakeDriver{done: make(chan struct{})}
	cmd := exec.Command(bin, "-test.run=^TestCompatM2dWakeDriver$", "-test.v")
	cmd.Dir = cwd
	cmd.Env = append(cmd.Environ(), "IRIS_COMPAT_WAKE_DIR="+dir,
		"IRIS_COMPAT_WAKE_CLIENT="+client, "IRIS_COMPAT_WAKE_SESSION="+session)
	cmd.Stdout, cmd.Stderr = &d.output, &d.output
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	d.stdin = stdin
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		t.Fatal(err)
	}
	go func() {
		d.err = cmd.Wait()
		close(d.done)
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-d.done
	})
	return d
}

func (d *compatWakeDriver) wait(t *testing.T) string {
	t.Helper()
	select {
	case <-d.done:
	case <-time.After(5 * time.Second):
		t.Fatal("wake driver did not exit")
	}
	out := d.output.String()
	if d.err != nil || !strings.Contains(out, "--- PASS: TestCompatM2dWakeDriver") {
		t.Fatalf("wake driver: %v\n%s", d.err, out)
	}
	return out
}

func (d *compatWakeDriver) stop(t *testing.T) string {
	t.Helper()
	_ = d.stdin.Close()
	return d.wait(t)
}
