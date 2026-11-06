package service

import (
	"context"
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
