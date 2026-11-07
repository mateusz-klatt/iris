package service

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
)

type memoryValue struct {
	text string
	err  error
}

func (v *memoryValue) Read() (string, error) { return v.text, v.err }
func (v *memoryValue) Write(s string) error {
	if v.err != nil {
		return v.err
	}
	v.text = s
	return nil
}
func (v *memoryValue) Remove() error { v.text = ""; return v.err }

type memoryTask struct {
	removed int
	err     error
}

func (t *memoryTask) Remove(context.Context) error { t.removed++; return t.err }

type memoryProcess struct {
	up            bool
	starts, stops int
	err           error
}

func (p *memoryProcess) Responds(context.Context) bool { return p.up }
func (p *memoryProcess) Start(context.Context) error   { p.starts++; p.up = p.err == nil; return p.err }
func (p *memoryProcess) Stop(context.Context) error    { p.stops++; p.up = false; return p.err }
func TestAutostartLifecycle(t *testing.T) {
	ctx := context.Background()
	value := &memoryValue{}
	process := &memoryProcess{}
	m := autostartManager{value, `"C:\Program Files\Iris\iris.exe" service start --db hub.db`, process, &memoryTask{}}
	if err := m.Install(ctx); err != nil {
		t.Fatal(err)
	}
	st, err := m.Status(ctx)
	if err != nil || !st.Installed || !st.Active || st.Detail != m.command {
		t.Fatalf("%+v %v", st, err)
	}
	if err := m.Start(ctx); err != nil || process.starts != 1 {
		t.Fatalf("duplicate start: %d %v", process.starts, err)
	}
	if err := m.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if err := m.Start(ctx); err != nil || process.starts != 2 {
		t.Fatalf("restart: %d %v", process.starts, err)
	}
	if err := m.Uninstall(ctx); err != nil {
		t.Fatal(err)
	}
	st, err = m.Status(ctx)
	if err != nil || st.Installed || st.Active || process.stops != 2 {
		t.Fatalf("%+v %v", st, err)
	}
}
func TestAutostartFailure(t *testing.T) {
	ctx := context.Background()
	broken := errors.New("denied")
	value := &memoryValue{err: broken}
	process := &memoryProcess{}
	m := autostartManager{value, "iris service start", process, &memoryTask{}}
	if !errors.Is(m.Install(ctx), broken) || process.starts != 0 {
		t.Fatal("started after registry failure")
	}
	value.err = nil
	process.err = broken
	if !errors.Is(m.Install(ctx), broken) || value.text == "" {
		t.Fatal("lost autostart after spawn failure")
	}
	if !errors.Is(m.Uninstall(ctx), broken) || value.text != "" {
		t.Fatal("failed to remove value after stop failure")
	}
}

type latePhase struct {
	memoryProcess
	polls int
}

// Responds is false for the start check and the first poll, as for a hub opening SQLite.
func (p *latePhase) Responds(context.Context) bool { p.polls++; return p.polls > 2 }

type silentProcess struct{ memoryProcess }

func (*silentProcess) Responds(context.Context) bool { return false }

func TestInstallWaitsForReadiness(t *testing.T) {
	task := &memoryTask{}
	process := &latePhase{}
	m := autostartManager{&memoryValue{}, "iris service start", process, task}
	if err := m.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	if process.polls != 3 || task.removed != 1 {
		t.Fatalf("polls %d, task removals %d", process.polls, task.removed)
	}
}

func TestInstallGivesUpWhenTheHubStaysSilent(t *testing.T) {
	task := &memoryTask{}
	value := &memoryValue{}
	m := autostartManager{value, "iris service start", &silentProcess{}, task}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := m.Install(ctx)
	if err == nil || !strings.Contains(err.Error(), "hub log") {
		t.Fatal(err)
	}
	if value.text == "" || task.removed != 0 {
		t.Fatalf("value %q, task removals %d", value.text, task.removed)
	}
}

func TestOldTaskThatCannotBeRemovedIsReported(t *testing.T) {
	denied := errors.New("denied")
	m := autostartManager{&memoryValue{}, "iris service start", &memoryProcess{}, &memoryTask{err: denied}}
	err := m.Install(context.Background())
	if !errors.Is(err, denied) || !strings.Contains(err.Error(), "hub is running") {
		t.Fatal(err)
	}
	if !errors.Is(m.Uninstall(context.Background()), denied) {
		t.Fatal("uninstall hid the task")
	}
}

func TestStatusReportsARegistryFailure(t *testing.T) {
	denied := errors.New("denied")
	m := autostartManager{&memoryValue{err: denied}, "", &memoryProcess{}, &memoryTask{}}
	if _, err := m.Status(context.Background()); !errors.Is(err, denied) {
		t.Fatal(err)
	}
}

type recordingRunner struct {
	calls  [][]string
	query  string
	failAt int // 1-based call that fails, 0 for none
}

func (r *recordingRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	r.calls = append(r.calls, append([]string{name}, args...))
	if len(r.calls) == r.failAt {
		return "", errors.New("exit status 1")
	}
	return r.query, nil
}

func TestScheduledTaskRemovesOnlyTheOldIrisTask(t *testing.T) {
	ctx := context.Background()
	query := []string{"schtasks", "/Query", "/TN", "Iris", "/XML"}
	remove := []string{"schtasks", "/Delete", "/F", "/TN", "Iris"}
	// Shape measured from schtasks on Windows 11. Text around it is localized.
	ours := "<Command>C:\\tools\\iris.exe</Command>\n<Arguments>serve --listen 127.0.0.1:7878 --token-file C:\\t</Arguments>"
	absent := &recordingRunner{failAt: 1}
	if err := (scheduledTask{absent}).Remove(ctx); err != nil || !reflect.DeepEqual(absent.calls, [][]string{query}) {
		t.Fatalf("absent: %v %v", absent.calls, err)
	}
	foreign := &recordingRunner{query: "<Command>C:\\backup\\iris.exe</Command><Arguments>--nightly</Arguments>"}
	if err := (scheduledTask{foreign}).Remove(ctx); err != nil || !reflect.DeepEqual(foreign.calls, [][]string{query}) {
		t.Fatalf("someone else's task: %v %v", foreign.calls, err)
	}
	old := &recordingRunner{query: ours}
	if err := (scheduledTask{old}).Remove(ctx); err != nil || !reflect.DeepEqual(old.calls, [][]string{query, remove}) {
		t.Fatalf("old task: %v %v", old.calls, err)
	}
	refused := &recordingRunner{query: ours, failAt: 2}
	if err := (scheduledTask{refused}).Remove(ctx); err == nil {
		t.Fatal("hid a failed delete")
	}
}

func TestRunnerHelper(t *testing.T) {
	switch os.Getenv("IRIS_RUNNER_TEST") {
	case "":
		return
	case "speak":
		_, _ = os.Stdout.WriteString("  said  \n")
		os.Exit(0)
	case "complain":
		_, _ = os.Stderr.WriteString("refused\n")
		os.Exit(3)
	default:
		os.Exit(4)
	}
}

func TestExecRunner(t *testing.T) {
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	run := func(mode string) (string, error) {
		t.Setenv("IRIS_RUNNER_TEST", mode)
		return ExecRunner{}.Run(ctx, bin, "-test.run=^TestRunnerHelper$")
	}
	if out, err := run("speak"); out != "said" || err != nil {
		t.Fatalf("%q %v", out, err)
	}
	if out, err := run("complain"); out != "refused" || err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("%q %v", out, err)
	}
	if out, err := run("silent"); out != "" || err == nil {
		t.Fatalf("%q %v", out, err)
	}
}
