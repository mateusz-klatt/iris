package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

func TestRegistryOwnValue(t *testing.T) {
	v := registryValue{runKey, fmt.Sprintf("Iris-test-%d-%d", os.Getpid(), time.Now().UnixNano())}
	t.Cleanup(func() {
		if err := v.Remove(); err != nil {
			t.Error(err)
		}
	})
	if err := v.Write(`"C:\Program Files\Iris\iris.exe" service start`); err != nil {
		t.Fatal(err)
	}
	got, err := v.Read()
	if err != nil || !strings.Contains(got, "service start") {
		t.Fatalf("%q %v", got, err)
	}
	if err := v.Remove(); err != nil {
		t.Fatal(err)
	}
	got, err = v.Read()
	if err != nil || got != "" {
		t.Fatalf("%q %v", got, err)
	}
}
func TestDetachedHubHelper(t *testing.T) {
	if os.Getenv("IRIS_SERVICE_TEST_CHILD") != "1" {
		return
	}
	fmt.Println("detached child")
	if addr := os.Getenv("IRIS_SERVICE_TEST_ADDR"); addr != "" {
		_ = http.ListenAndServe(addr, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	}
	time.Sleep(time.Minute)
	os.Exit(0)
}

// helperHub describes a real detached child of this test binary.
func helperHub(t *testing.T, dir, stem, url string) detachedHub {
	t.Helper()
	t.Setenv("IRIS_SERVICE_TEST_CHILD", "1")
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return detachedHub{argv: []string{bin, "-test.run=^TestDetachedHubHelper$"}, dir: dir, stem: stem, url: url}
}

// helperProcess runs the child directly and never leaves it behind.
func helperProcess(t *testing.T) *exec.Cmd {
	t.Helper()
	p := helperHub(t, "", "", "")
	cmd := exec.Command(p.argv[0], p.argv[1:]...)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return cmd
}

func writePID(t *testing.T, p detachedHub, id processIdentity) {
	t.Helper()
	raw, err := json.Marshal(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.path(".pid"), raw, 0600); err != nil {
		t.Fatal(err)
	}
}

func freeAddress(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

func testKey(t *testing.T) string {
	t.Helper()
	key := fmt.Sprintf(`Software\Iris-test-%d-%d`, os.Getpid(), time.Now().UnixNano())
	t.Cleanup(func() {
		if err := registry.DeleteKey(registry.CURRENT_USER, key); err != nil && !errors.Is(err, registry.ErrNotExist) {
			t.Error(err)
		}
	})
	return key
}

func TestRegistryMissingKeyAndValue(t *testing.T) {
	v := registryValue{testKey(t), "Iris"}
	if got, err := v.Read(); got != "" || err != nil {
		t.Fatalf("missing key: %q %v", got, err)
	}
	if err := v.Remove(); err != nil {
		t.Fatalf("remove from missing key: %v", err)
	}
	if err := v.Write("iris service start"); err != nil {
		t.Fatal(err)
	}
	if got, err := v.Read(); got != "iris service start" || err != nil {
		t.Fatalf("%q %v", got, err)
	}
	if err := v.Remove(); err != nil {
		t.Fatal(err)
	}
	if got, err := v.Read(); got != "" || err != nil {
		t.Fatalf("missing value: %q %v", got, err)
	}
	if err := v.Remove(); err != nil {
		t.Fatalf("remove missing value: %v", err)
	}
}

func TestRegistryReportsRealFailures(t *testing.T) {
	v := registryValue{testKey(t), "Iris"}
	if err := v.Write("x"); err != nil {
		t.Fatal(err)
	}
	k, err := registry.OpenKey(registry.CURRENT_USER, v.key, registry.SET_VALUE)
	if err != nil {
		t.Fatal(err)
	}
	defer k.Close()
	if err := k.SetDWordValue(v.name, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := v.Read(); err == nil {
		t.Fatal("a value of the wrong type read as absent")
	}
	long := registryValue{strings.Repeat("x", 300), "Iris"}
	if err := long.Write("x"); err == nil {
		t.Fatal("wrote under a key name the registry rejects")
	}
}

func TestHostUsesTheGivenDatabaseAndAddress(t *testing.T) {
	db := filepath.Join(`C:\Data Dir`, "other.db")
	m := Host("", "", []string{`C:\Program Files\Iris\iris.exe`, "serve", "--listen", "127.0.0.1:9", "--db", db}).(autostartManager)
	if m.value != (registryValue{runKey, "Iris"}) {
		t.Fatalf("%+v", m.value)
	}
	want := `"C:\Program Files\Iris\iris.exe" service start --listen 127.0.0.1:9 --db "` + db + `"`
	if m.command != want {
		t.Fatalf("%s\nwant %s", m.command, want)
	}
	hub := m.process.(detachedHub)
	if hub.dir != `C:\Data Dir` || hub.stem != "other.db.hub" || hub.url != "http://127.0.0.1:9/v1/status" {
		t.Fatalf("%+v", hub)
	}
	if _, ok := m.legacy.(scheduledTask); !ok {
		t.Fatalf("%T", m.legacy)
	}
}

func TestHostDefaultsKeepTheOriginalFileNames(t *testing.T) {
	m := Host("", "", []string{"iris", "serve", "--db"}).(autostartManager)
	hub := m.process.(detachedHub)
	if hub.stem != "hub" || !strings.HasSuffix(hub.url, "/v1/status") {
		t.Fatalf("%+v", hub)
	}
	if hubStem(filepath.Join("dir", "hub")) == "hub" {
		t.Fatal("a database named hub shares files with hub.db")
	}
}

func TestTwoHubsInOneDirectoryKeepSeparateIdentities(t *testing.T) {
	dir := t.TempDir()
	first := helperHub(t, dir, hubStem("first.db"), "http://127.0.0.1:1/v1/status")
	second := helperHub(t, dir, hubStem("second.db"), "http://127.0.0.1:1/v1/status")
	ctx := context.Background()
	for _, p := range []detachedHub{first, second} {
		if err := p.Start(ctx); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = p.Stop(ctx) })
	}
	h, err := second.openRecorded()
	if err != nil || h == 0 {
		t.Fatalf("second hub was not recorded: %v %v", h, err)
	}
	defer windows.CloseHandle(h)
	if err := first.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if state, err := windows.WaitForSingleObject(h, 0); err != nil || state != uint32(windows.WAIT_TIMEOUT) {
		t.Fatalf("stopping one hub stopped the other: %v %v", state, err)
	}
}

func TestRealAutostartLifecycle(t *testing.T) {
	addr := freeAddress(t)
	t.Setenv("IRIS_SERVICE_TEST_ADDR", addr)
	dir := t.TempDir()
	hub := helperHub(t, dir, "hub", "http://"+addr+"/v1/status")
	task := &recordingRunner{}
	m := autostartManager{value: registryValue{testKey(t), "Iris"}, command: "iris service start", process: hub, legacy: scheduledTask{task}}
	ctx := context.Background()
	t.Cleanup(func() { _ = hub.Stop(ctx) })
	if err := m.Install(ctx); err != nil {
		t.Fatal(err)
	}
	st, err := m.Status(ctx)
	if err != nil || !st.Installed || !st.Active || st.Detail != "iris service start" {
		t.Fatalf("%+v %v", st, err)
	}
	raw, err := os.ReadFile(hub.path(".pid"))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if again, _ := os.ReadFile(hub.path(".pid")); string(again) != string(raw) {
		t.Fatal("start replaced a running hub")
	}
	if err := m.Uninstall(ctx); err != nil {
		t.Fatal(err)
	}
	st, err = m.Status(ctx)
	if err != nil || st.Installed || st.Active {
		t.Fatalf("%+v %v", st, err)
	}
	if _, err := os.Stat(hub.path(".pid")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("identity survived: %v", err)
	}
}

func TestResponds(t *testing.T) {
	code := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code) }))
	defer srv.Close()
	ctx := context.Background()
	if !(detachedHub{url: srv.URL}).Responds(ctx) {
		t.Fatal("healthy hub")
	}
	code = http.StatusInternalServerError
	if (detachedHub{url: srv.URL}).Responds(ctx) {
		t.Fatal("a failing hub counted as up")
	}
	if (detachedHub{url: "http://%zz"}).Responds(ctx) {
		t.Fatal("unparseable address")
	}
	if (detachedHub{url: "http://127.0.0.1:1/"}).Responds(ctx) {
		t.Fatal("closed port")
	}
}

func TestStartLeavesARespondingHubAlone(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	p := detachedHub{argv: []string{"does-not-exist"}, dir: t.TempDir(), stem: "hub", url: srv.URL}
	if err := p.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p.path(".pid")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("started a second hub: %v", err)
	}
}

func TestLockedRefusesConcurrentAndBrokenState(t *testing.T) {
	dir := t.TempDir()
	p := detachedHub{dir: dir, stem: "hub"}
	ran := false
	err := p.locked(func() error {
		return p.locked(func() error { ran = true; return nil })
	})
	if err == nil || ran || !strings.Contains(err.Error(), "already in progress") {
		t.Fatalf("%v ran=%v", err, ran)
	}
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0600); err != nil {
		t.Fatal(err)
	}
	blocked := detachedHub{dir: filepath.Join(file, "below"), stem: "hub"}
	if err := blocked.locked(func() error { ran = true; return nil }); err == nil || ran {
		t.Fatalf("directory below a file: %v", err)
	}
	directory := detachedHub{dir: t.TempDir(), stem: "hub"}
	if err := os.Mkdir(directory.path("-process.lock"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := directory.locked(func() error { ran = true; return nil }); err == nil || ran {
		t.Fatalf("lock path is a directory: %v", err)
	}
}

func TestStartReportsWhatStopsIt(t *testing.T) {
	ctx := context.Background()
	closed := "http://127.0.0.1:1/v1/status"
	p := detachedHub{argv: []string{"does-not-exist"}, dir: t.TempDir(), stem: "hub", url: closed}
	if err := p.Start(ctx); err == nil {
		t.Fatal("started a missing binary")
	}
	if _, err := os.Stat(p.path(".pid")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("recorded a process that never ran: %v", err)
	}
	unlogged := detachedHub{argv: []string{"does-not-exist"}, dir: t.TempDir(), stem: "hub", url: closed}
	if err := os.Mkdir(unlogged.path(".log"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := unlogged.Start(ctx); err == nil || !strings.Contains(err.Error(), "hub.log") {
		t.Fatalf("started without a log: %v", err)
	}
	corrupt := detachedHub{dir: t.TempDir(), stem: "hub", url: closed}
	if err := os.WriteFile(corrupt.path(".pid"), []byte("not json"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := corrupt.Start(ctx); err == nil {
		t.Fatal("ignored a corrupt identity")
	}
	if err := corrupt.Stop(ctx); err == nil {
		t.Fatal("removed a corrupt identity without reporting it")
	}
}

func TestOpenRecordedRejectsWhatIsNotTheHub(t *testing.T) {
	p := detachedHub{dir: t.TempDir(), stem: "hub"}
	if h, err := p.openRecorded(); h != 0 || err != nil {
		t.Fatalf("no file: %v %v", h, err)
	}
	unreadable := detachedHub{dir: t.TempDir(), stem: "hub"}
	if err := os.Mkdir(unreadable.path(".pid"), 0700); err != nil {
		t.Fatal(err)
	}
	if h, err := unreadable.openRecorded(); h != 0 || err == nil {
		t.Fatalf("unreadable identity: %v %v", h, err)
	}
	writePID(t, p, processIdentity{})
	if h, err := p.openRecorded(); h != 0 || err == nil {
		t.Fatalf("empty identity: %v %v", h, err)
	}
	writePID(t, p, processIdentity{PID: 0xFFFFFFF0, Created: 1})
	if h, err := p.openRecorded(); h != 0 || err != nil {
		t.Fatalf("no such process: %v %v", h, err)
	}
	// The System process refuses this access. An elevated runner may open it instead,
	// and then the creation time still rejects it.
	writePID(t, p, processIdentity{PID: 4, Created: 1})
	if h, err := p.openRecorded(); h != 0 || (err != nil && !errors.Is(err, windows.ERROR_ACCESS_DENIED)) {
		t.Fatalf("protected process: %v %v", h, err)
	}
	cmd := helperProcess(t)
	created, err := processCreated(cmd.Process)
	if err != nil {
		t.Fatal(err)
	}
	writePID(t, p, processIdentity{uint32(cmd.Process.Pid), created})
	h, err := p.openRecorded()
	if h == 0 || err != nil {
		t.Fatalf("running hub: %v %v", h, err)
	}
	windows.CloseHandle(h)
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	if h, err := p.openRecorded(); h != 0 || err != nil {
		t.Fatalf("exited hub: %v %v", h, err)
	}
}

func TestStaleIdentityIsReplacedByANewHub(t *testing.T) {
	p := helperHub(t, t.TempDir(), "hub", "http://127.0.0.1:1/v1/status")
	writePID(t, p, processIdentity{PID: 0xFFFFFFF0, Created: 1})
	ctx := context.Background()
	if err := p.Start(ctx); err != nil {
		t.Fatal(err)
	}
	h, err := p.openRecorded()
	if err != nil || h == 0 {
		t.Fatalf("%v %v", h, err)
	}
	windows.CloseHandle(h)
	if err := p.Stop(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestAnUnrecordedHubDoesNotKeepRunning(t *testing.T) {
	wait := func(cmd *exec.Cmd) {
		t.Helper()
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("hub kept running without an identity")
		}
	}
	broken := errors.New("no identity")
	p := detachedHub{dir: t.TempDir(), stem: "hub"}
	cmd := helperProcess(t)
	if err := p.record(cmd.Process, func(*os.Process) (uint64, error) { return 0, broken }); !errors.Is(err, broken) {
		t.Fatal(err)
	}
	wait(cmd)
	if err := os.Mkdir(p.path(".pid"), 0700); err != nil {
		t.Fatal(err)
	}
	cmd = helperProcess(t)
	if err := p.record(cmd.Process, processCreated); err == nil {
		t.Fatal("wrote an identity into a directory")
	}
	wait(cmd)
}

func TestProcessCreatedNeedsALiveHandle(t *testing.T) {
	cmd := helperProcess(t)
	created, err := processCreated(cmd.Process)
	if err != nil || created == 0 {
		t.Fatalf("%d %v", created, err)
	}
	released, err := os.FindProcess(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	if err := released.Release(); err != nil {
		t.Fatal(err)
	}
	if _, err := processCreated(released); err == nil {
		t.Fatal("read a released process")
	}
}

func TestTerminateJudgesByTheWait(t *testing.T) {
	cmd := helperProcess(t)
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(cmd.Process.Pid))
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(h)
	if err := terminate(h, 1); err == nil || !strings.Contains(err.Error(), "did not stop") {
		t.Fatalf("a refused termination counted as stopped: %v", err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := terminate(h, 5000); err != nil {
		t.Fatalf("an exited process is stopped: %v", err)
	}
}

func TestForgetKeepsTheIdentityOfARunningHub(t *testing.T) {
	p := detachedHub{dir: t.TempDir(), stem: "hub"}
	writePID(t, p, processIdentity{PID: 7, Created: 7})
	stuck := errors.New("hub did not stop")
	if err := p.forget(stuck); !errors.Is(err, stuck) {
		t.Fatal(err)
	}
	if _, err := os.Stat(p.path(".pid")); err != nil {
		t.Fatalf("identity lost: %v", err)
	}
	path, err := windows.UTF16PtrFromString(p.path(".pid"))
	if err != nil {
		t.Fatal(err)
	}
	held, err := windows.CreateFile(path, windows.GENERIC_READ, windows.FILE_SHARE_READ, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.forget(nil); err == nil {
		t.Fatal("removed a file that another process holds")
	}
	windows.CloseHandle(held)
	if err := p.forget(nil); err != nil {
		t.Fatal(err)
	}
	if err := p.forget(nil); err != nil {
		t.Fatalf("second stop: %v", err)
	}
}
func TestDetachedHubIdentity(t *testing.T) {
	t.Setenv("IRIS_SERVICE_TEST_CHILD", "1")
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	p := detachedHub{argv: []string{bin, "-test.run=^TestDetachedHubHelper$"}, dir: t.TempDir(), stem: "hub", url: "http://127.0.0.1:1/v1/status"}
	ctx := context.Background()
	if err := p.Start(ctx); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(p.dir, "hub.pid")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var id processIdentity
	if err := json.Unmarshal(raw, &id); err != nil {
		t.Fatal(err)
	}
	// Keep the original identity available even if a later assertion fails.
	t.Cleanup(func() {
		_ = os.WriteFile(path, raw, 0600)
		if err := p.Stop(ctx); err != nil {
			t.Error(err)
		}
	})
	h, err := p.openRecorded()
	if err != nil || h == 0 {
		t.Fatalf("identity: %v %v", h, err)
	}
	defer windows.CloseHandle(h)
	if err := p.Start(ctx); err != nil {
		t.Fatal(err)
	}
	again, _ := os.ReadFile(path)
	if string(again) != string(raw) {
		t.Fatal("duplicate start replaced identity")
	}
	id.Created++
	wrong, _ := json.Marshal(id)
	if err := os.WriteFile(path, wrong, 0600); err != nil {
		t.Fatal(err)
	}
	if err := p.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if state, err := windows.WaitForSingleObject(h, 0); err != nil || state != uint32(windows.WAIT_TIMEOUT) {
		t.Fatalf("stopped mismatched process: %v %v", state, err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := p.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if state, err := windows.WaitForSingleObject(h, 0); err != nil || state != windows.WAIT_OBJECT_0 {
		t.Fatalf("did not stop: %v %v", state, err)
	}
	if _, err := os.Stat(filepath.Join(p.dir, "hub.log")); err != nil {
		t.Fatal(err)
	}
}
