package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type launchRun struct {
	calls []string
	fail  string
}

func (f *launchRun) Run(_ context.Context, name string, args ...string) (string, error) {
	call := name + " " + strings.Join(args, " ")
	f.calls = append(f.calls, call)
	if f.fail != "" && strings.Contains(call, f.fail) {
		return "", errors.New("exit status 1")
	}
	return "state = running", nil
}

func launchManager(home string, run Runner) launchdManager {
	return newManager(home, "", []string{"/opt/iris", "serve"}, run).(launchdManager)
}

func TestLaunchdLifecycle(t *testing.T) {
	ctx := context.Background()
	run := &launchRun{}
	m := launchManager(t.TempDir(), run)
	if err := m.Install(ctx); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(m.plist)
	if err != nil || !strings.Contains(string(body), Marker) {
		t.Fatalf("%s %v", body, err)
	}
	st, err := m.Status(ctx)
	if err != nil || !st.Installed || !st.Active || st.Detail == "" {
		t.Fatalf("%+v %v", st, err)
	}
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := m.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if err := m.Uninstall(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(m.plist); !os.IsNotExist(err) {
		t.Fatal("plist remains")
	}
	if err := m.Uninstall(ctx); err != nil {
		t.Fatalf("nothing installed: %v", err)
	}
}

func TestLaunchdHost(t *testing.T) {
	m := Host("/Users/a", "", []string{"/opt/iris"}).(launchdManager)
	if m.plist != filepath.Join("/Users/a", "Library", "LaunchAgents", "iris.plist") {
		t.Fatal(m.plist)
	}
	if _, ok := m.run.(ExecRunner); !ok {
		t.Fatalf("%T", m.run)
	}
	if want := "gui/"; !strings.HasPrefix(m.domain(), want) {
		t.Fatal(m.domain())
	}
}

func TestLaunchdRefusesAForeignPlist(t *testing.T) {
	ctx := context.Background()
	m := launchManager(t.TempDir(), &launchRun{})
	if err := os.MkdirAll(filepath.Dir(m.plist), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m.plist, []byte("<plist/>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := m.Install(ctx); err == nil {
		t.Fatal("overwrote a foreign plist")
	}
	if err := m.Uninstall(ctx); err == nil {
		t.Fatal("removed a foreign plist")
	}
}

func TestLaunchdReportsFilesystemFailures(t *testing.T) {
	ctx := context.Background()
	blocked := t.TempDir()
	if err := os.WriteFile(filepath.Join(blocked, "Library"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	m := launchManager(blocked, &launchRun{})
	if err := m.Install(ctx); err == nil {
		t.Fatal("plist path runs through a file")
	}
	if err := m.Uninstall(ctx); err == nil {
		t.Fatal("unreadable plist")
	}
	dangling := t.TempDir()
	if err := os.Mkdir(filepath.Join(dangling, "Library"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dangling, "gone"), filepath.Join(dangling, "Library", "LaunchAgents")); err != nil {
		t.Fatal(err)
	}
	if err := launchManager(dangling, &launchRun{}).Install(ctx); err == nil {
		t.Fatal("plist directory cannot be created")
	}
	link := launchManager(t.TempDir(), &launchRun{})
	if err := os.MkdirAll(filepath.Dir(link.plist), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(filepath.Dir(link.plist), "gone", "plist"), link.plist); err != nil {
		t.Fatal(err)
	}
	if err := link.Install(ctx); err == nil {
		t.Fatal("plist cannot be written")
	}
}

func TestLaunchdStartBootstrapsOnlyWhenInactive(t *testing.T) {
	ctx := context.Background()
	run := &launchRun{fail: "print"}
	m := launchManager(t.TempDir(), run)
	if st, err := m.Status(ctx); err != nil || st.Installed || st.Active {
		t.Fatalf("%+v %v", st, err)
	}
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if got := run.calls[len(run.calls)-1]; !strings.HasPrefix(got, "launchctl bootstrap ") {
		t.Fatal(got)
	}
}
