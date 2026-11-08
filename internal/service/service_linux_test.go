package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fakeRun struct{ calls []string }

func (f *fakeRun) Run(_ context.Context, name string, args ...string) (string, error) {
	f.calls = append(f.calls, name+" "+strings.Join(args, " "))
	if name == "systemctl" && len(args) >= 3 && args[2] == "is-active" {
		return "active", nil
	}
	return "", nil
}

func TestSystemdInstallDoesNotCallTheHost(t *testing.T) {
	home := t.TempDir()
	run := &fakeRun{}
	mgr := newManager(home, "", []string{"/opt/iris", "serve"}, run)
	if err := mgr.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	unit := filepath.Join(home, ".config", "systemd", "user", "iris.service")
	body, err := os.ReadFile(unit)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "ExecStart=/opt/iris serve") || !strings.Contains(string(body), Marker) {
		t.Fatal(string(body))
	}
	joined := strings.Join(run.calls, "\n")
	if !strings.Contains(joined, "systemctl --user daemon-reload") || !strings.Contains(joined, "systemctl --user enable --now iris.service") {
		t.Fatal(joined)
	}
	st, err := mgr.Status(context.Background())
	if err != nil || !st.Installed || !st.Active {
		t.Fatalf("%+v %v", st, err)
	}
	if err := mgr.Uninstall(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(unit); !os.IsNotExist(err) {
		t.Fatal("unit remains")
	}
}

func TestSystemdRefusesForeignUnit(t *testing.T) {
	home := t.TempDir()
	unit := filepath.Join(home, ".config", "systemd", "user", "iris.service")
	if err := os.MkdirAll(filepath.Dir(unit), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unit, []byte("[Service]\nExecStart=/bin/true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mgr := newManager(home, "", []string{"/opt/iris", "serve"}, &fakeRun{})
	if err := mgr.Install(context.Background()); err == nil {
		t.Fatal("expected refusal")
	}
	body, _ := os.ReadFile(unit)
	if strings.Contains(string(body), Marker) {
		t.Fatal("overwrote foreign unit")
	}
}

// failingRun fails the calls whose text contains a key, with the given output.
type failingRun struct {
	fail map[string]string
	out  string
}

func (f failingRun) Run(_ context.Context, name string, args ...string) (string, error) {
	call := name + " " + strings.Join(args, " ")
	for part, out := range f.fail {
		if strings.Contains(call, part) {
			return out, errors.New("exit status 1")
		}
	}
	return f.out, nil
}

func installed(t *testing.T, home string, run Runner) (Manager, string) {
	t.Helper()
	mgr := newManager(home, "", []string{"/opt/iris", "serve"}, &fakeRun{})
	if err := mgr.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	return newManager(home, "", []string{"/opt/iris", "serve"}, run), filepath.Join(home, ".config", "systemd", "user", "iris.service")
}

func TestHostUsesTheGivenConfigDirectory(t *testing.T) {
	m := Host("/home/a", "/etc/xdg", []string{"/opt/iris"}).(systemdManager)
	if m.unit != filepath.Join("/etc/xdg", "systemd", "user", "iris.service") {
		t.Fatal(m.unit)
	}
	if _, ok := m.run.(ExecRunner); !ok {
		t.Fatalf("%T", m.run)
	}
}

func TestSystemdInstallStopsAtTheFirstFailure(t *testing.T) {
	ctx := context.Background()
	for _, step := range []string{"daemon-reload", "enable"} {
		run := failingRun{fail: map[string]string{step: ""}}
		if err := newManager(t.TempDir(), "", []string{"/opt/iris"}, run).Install(ctx); err == nil {
			t.Fatalf("%s failed silently", step)
		}
	}
}

func TestSystemdInstallReportsFilesystemFailures(t *testing.T) {
	ctx := context.Background()
	argv := []string{"/opt/iris"}
	blocked := t.TempDir()
	if err := os.WriteFile(filepath.Join(blocked, ".config"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := newManager(blocked, "", argv, &fakeRun{}).Install(ctx); err == nil {
		t.Fatal("unit path runs through a file")
	}
	dangling := t.TempDir()
	if err := os.Mkdir(filepath.Join(dangling, ".config"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dangling, "gone"), filepath.Join(dangling, ".config", "systemd")); err != nil {
		t.Fatal(err)
	}
	if err := newManager(dangling, "", argv, &fakeRun{}).Install(ctx); err == nil {
		t.Fatal("unit directory cannot be created")
	}
	link := t.TempDir()
	dir := filepath.Join(link, ".config", "systemd", "user")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(link, "gone", "unit"), filepath.Join(dir, "iris.service")); err != nil {
		t.Fatal(err)
	}
	if err := newManager(link, "", argv, &fakeRun{}).Install(ctx); err == nil {
		t.Fatal("unit file cannot be written")
	}
}

func TestSystemdUninstall(t *testing.T) {
	ctx := context.Background()
	argv := []string{"/opt/iris"}
	if err := newManager(t.TempDir(), "", argv, &fakeRun{}).Uninstall(ctx); err != nil {
		t.Fatalf("nothing installed: %v", err)
	}
	blocked := t.TempDir()
	if err := os.WriteFile(filepath.Join(blocked, ".config"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := newManager(blocked, "", argv, &fakeRun{}).Uninstall(ctx); err == nil {
		t.Fatal("unreadable unit")
	}
	foreign := t.TempDir()
	unit := filepath.Join(foreign, ".config", "systemd", "user", "iris.service")
	if err := os.MkdirAll(filepath.Dir(unit), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unit, []byte("[Service]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := newManager(foreign, "", argv, &fakeRun{}).Uninstall(ctx); err == nil {
		t.Fatal("removed a foreign unit")
	}
	if _, err := os.Stat(unit); err != nil {
		t.Fatal(err)
	}
	mgr, unit := installed(t, t.TempDir(), failingRun{fail: map[string]string{"disable": "not loaded", "daemon-reload": ""}})
	if err := mgr.Uninstall(ctx); err == nil {
		t.Fatal("daemon-reload failed silently")
	}
	if _, err := os.Stat(unit); !os.IsNotExist(err) {
		t.Fatal("a failed disable must not keep the unit")
	}
}

func TestSystemdUninstallReportsAUnitThatCannotBeRemoved(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	mgr, unit := installed(t, t.TempDir(), &fakeRun{})
	dir := filepath.Dir(unit)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if err := mgr.Uninstall(context.Background()); err == nil {
		t.Fatal("unit remains")
	}
}

func TestSystemdStatus(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name   string
		run    Runner
		active bool
		detail string
	}{
		{"active", failingRun{out: "active"}, true, "active"},
		{"no output", failingRun{}, true, ""},
		{"activating", failingRun{out: "activating"}, false, "activating"},
		{"inactive", failingRun{fail: map[string]string{"is-active": "inactive"}}, false, "inactive"},
		{"no systemd", failingRun{fail: map[string]string{"is-active": ""}}, false, "exit status 1"},
	} {
		t.Run(c.name, func(t *testing.T) {
			mgr, _ := installed(t, t.TempDir(), c.run)
			st, err := mgr.Status(ctx)
			if err != nil || !st.Installed || st.Active != c.active || st.Detail != c.detail {
				t.Fatalf("%+v %v", st, err)
			}
		})
	}
	st, err := newManager(t.TempDir(), "", nil, failingRun{out: "active"}).Status(ctx)
	if err != nil || st.Installed || st.Active {
		t.Fatalf("a unit that is gone is not active: %+v %v", st, err)
	}
}

func TestSystemdStartAndStop(t *testing.T) {
	ctx := context.Background()
	run := &fakeRun{}
	mgr := newManager(t.TempDir(), "", nil, run)
	if err := mgr.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(run.calls, "\n"); got != "systemctl --user start iris.service\nsystemctl --user stop iris.service" {
		t.Fatal(got)
	}
}
