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
	print string
}

func (f *launchRun) output() string {
	if f.print != "" {
		return f.print
	}
	return printRunning
}

func (f *launchRun) Run(_ context.Context, name string, args ...string) (string, error) {
	call := name + " " + strings.Join(args, " ")
	f.calls = append(f.calls, call)
	if f.fail != "" && strings.Contains(call, f.fail) {
		return "", errors.New("exit status 1")
	}
	return f.output(), nil
}

// Captured from launchctl print on macOS. The nested "state = active" lines belong to the job's
// endpoints and event sources, so they say nothing about the process.
const (
	printRunning = "gui/501/iris = {\n\tactive count = 1\n\ttype = LaunchAgent\n\tstate = running\n\n\tprogram = /opt/iris\n\tpid = 23019\n\tlast exit code = (never exited)\n\tendpoints = {\n\t\tstate = active\n\t}\n}"
	printCrashed = "gui/501/iris = {\n\tactive count = 0\n\ttype = LaunchAgent\n\tstate = spawn scheduled\n\n\tprogram = /opt/iris\n\truns = 2\n\tlast exit code = 1\n\tendpoints = {\n\t\tstate = active\n\t}\n}"
)

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

func writePlist(t *testing.T, m launchdManager) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(m.plist), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m.plist, LaunchdPlist(m.argv), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLaunchdStartBootstrapsOnlyWhenNotLoaded(t *testing.T) {
	ctx := context.Background()
	run := &launchRun{fail: "print"}
	m := launchManager(t.TempDir(), run)
	writePlist(t, m)
	if st, err := m.Status(ctx); err != nil || !st.Installed || st.Active {
		t.Fatalf("%+v %v", st, err)
	}
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if got := run.calls[len(run.calls)-1]; !strings.HasPrefix(got, "launchctl bootstrap ") {
		t.Fatal(got)
	}
}

func TestLaunchdStartNamesAMissingInstall(t *testing.T) {
	run := &launchRun{fail: "print"}
	m := launchManager(t.TempDir(), run)
	err := m.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "not installed") {
		t.Fatalf("launchctl reports this only as an I/O error: %v", err)
	}
	if len(run.calls) != 0 {
		t.Fatalf("%v", run.calls)
	}
}

func TestLaunchdStartLeavesALoadedJobToKeepAlive(t *testing.T) {
	run := &launchRun{print: printCrashed}
	m := launchManager(t.TempDir(), run)
	writePlist(t, m)
	// A second bootstrap of a loaded label fails, and launchd is already restarting this one.
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, call := range run.calls {
		if strings.Contains(call, "bootstrap") {
			t.Fatal(call)
		}
	}
}

func TestLaunchdStatusReadsTheJobStateNotTheLoad(t *testing.T) {
	ctx := context.Background()
	m := launchManager(t.TempDir(), &launchRun{print: printCrashed})
	writePlist(t, m)
	st, err := m.Status(ctx)
	if err != nil || !st.Installed || st.Active || !strings.Contains(st.Detail, "last exit code = 1") {
		t.Fatalf("a hub that keeps exiting is not active: %+v %v", st, err)
	}
}

func TestLaunchdInstallReplacesALoadedJob(t *testing.T) {
	run := &launchRun{}
	m := launchManager(t.TempDir(), run)
	if err := m.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"launchctl bootout " + m.target(),
		"launchctl bootstrap " + m.domain() + " " + m.plist,
	}
	if strings.Join(run.calls, "|") != strings.Join(want, "|") {
		t.Fatalf("%v", run.calls)
	}
}

func TestLaunchdInstallReportsAFailedBootstrap(t *testing.T) {
	m := launchManager(t.TempDir(), &launchRun{fail: "bootstrap"})
	if err := m.Install(context.Background()); err == nil {
		t.Fatal("the job did not load")
	}
}

func TestLaunchdStopIsANoOpWhenNotLoaded(t *testing.T) {
	run := &launchRun{fail: "print"}
	m := launchManager(t.TempDir(), run)
	if err := m.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, call := range run.calls {
		if strings.Contains(call, "bootout") {
			t.Fatal(call)
		}
	}
}

func TestLaunchdStopReportsAFailedBootout(t *testing.T) {
	m := launchManager(t.TempDir(), &launchRun{fail: "bootout"})
	if err := m.Stop(context.Background()); err == nil {
		t.Fatal("the job is still loaded")
	}
}
