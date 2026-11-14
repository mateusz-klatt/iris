//go:build windows

package proc

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The test binary's helper modes. Their processes have no standard handles, so they answer
// through files.
const (
	envReport = "IRIS_PROC_REPORT" // write this process's desktop and SYSTEMROOT, then wait
	envLaunch = "IRIS_PROC_LAUNCH" // start two probes from this process's desktop, then exit
	envParent = "IRIS_PROC_PARENT" // "pid:startMS" of the parent a launcher starts its probe under
	envProbe  = "IRIS_PROC_PROBE"  // wait for <dir>/go, then load user32 and record the result
)

func TestDetachedHelperModes(t *testing.T) {
	switch {
	case os.Getenv(envReport) != "":
		_ = os.WriteFile(os.Getenv(envReport), []byte(readDesktopInfo()+"\n"+os.Getenv("SYSTEMROOT")), 0o600)
		time.Sleep(time.Minute)
	case os.Getenv(envLaunch) != "":
		launch(os.Getenv(envLaunch))
	case os.Getenv(envProbe) != "":
		dir := os.Getenv(envProbe)
		for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			if _, err := os.Stat(filepath.Join(dir, "go")); err == nil {
				break
			}
		}
		result := "user32 ok"
		if err := syscall.NewLazyDLL("user32.dll").Load(); err != nil {
			result = "unexpected: " + err.Error()
			if errors.Is(err, windows.ERROR_DLL_INIT_FAILED) {
				result = "user32 init failed"
			}
		}
		_ = os.WriteFile(filepath.Join(dir, "probe"), []byte(result), 0o600)
	default:
		return
	}
	os.Exit(0)
}

// launch starts one probe through StartDetached and one as a plain child, which keeps this
// process's desktop, and records whether each started.
func launch(dir string) {
	pid, start, _ := strings.Cut(os.Getenv(envParent), ":")
	parentPID, _ := strconv.Atoi(pid)
	parentStart, _ := strconv.ParseInt(start, 10, 64)
	result := "moved started"
	d, err := StartDetached(os.Args[0], []string{"-test.run=^TestDetachedHelperModes$"}, []string{envProbe + "=" + filepath.Join(dir, "moved")}, parentPID, parentStart)
	if err != nil {
		result = "moved: " + err.Error()
	} else {
		_ = os.WriteFile(filepath.Join(dir, "moved", "pid"), []byte(strconv.Itoa(d.PID)), 0o600)
		d.Release()
	}
	plain := exec.Command(os.Args[0], "-test.run=^TestDetachedHelperModes$")
	plain.Env = []string{envProbe + "=" + filepath.Join(dir, "kept"), "SYSTEMROOT=" + os.Getenv("SYSTEMROOT")}
	if err := plain.Start(); err != nil {
		result += "; kept: " + err.Error()
	} else {
		result += "; kept started"
		_ = os.WriteFile(filepath.Join(dir, "kept", "pid"), []byte(strconv.Itoa(plain.Process.Pid)), 0o600)
		_ = plain.Process.Release()
	}
	_ = os.WriteFile(filepath.Join(dir, "launched"), []byte(result), 0o600)
}

func waitFile(t *testing.T, path string) string {
	t.Helper()
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if b, err := os.ReadFile(path); err == nil && len(b) > 0 {
			return string(b)
		}
	}
	t.Fatalf("%s was never written", path)
	return ""
}

// windowStation names this process's window station. Unlike the code under test, the test may
// connect to it.
func windowStation(t *testing.T) string {
	t.Helper()
	user32 := syscall.NewLazyDLL("user32.dll")
	station, _, _ := user32.NewProc("GetProcessWindowStation").Call()
	buf := make([]uint16, 256)
	var n uint32
	if ok, _, err := user32.NewProc("GetUserObjectInformationW").Call(station, 2, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)*2), uintptr(unsafe.Pointer(&n))); ok == 0 {
		t.Fatalf("window station name: %v", err)
	}
	return windows.UTF16ToString(buf)
}

func self(t *testing.T) Snapshot {
	t.Helper()
	s, ok := Lookup(os.Getpid())
	if !ok {
		t.Fatal("self lookup")
	}
	return s
}

func replaceDesktopInfo(t *testing.T, info string) {
	t.Helper()
	orig := desktopInfo
	t.Cleanup(func() { desktopInfo = orig })
	desktopInfo = func() string { return info }
}

// The process is created under the given parent, not this test, and receives SYSTEMROOT although
// the given environment omits it. Started from a desktop other than Default, it runs on its
// window station's Default desktop; started from Default, it keeps the caller's desktop.
func TestStartDetachedRunsUnderTheParentOnItsTargetDesktop(t *testing.T) {
	station := windowStation(t)
	for _, tc := range []struct {
		from, want string
		release    bool
	}{
		{from: station + `\iris-command-desktop`, want: station + `\Default`},
		{from: station + `\Default`, want: readDesktopInfo(), release: true},
	} {
		replaceDesktopInfo(t, tc.from)
		parent := parentChild(t)
		snap, ok := Lookup(parent.Process.Pid)
		if !ok {
			t.Fatal("parent lookup")
		}
		report := filepath.Join(t.TempDir(), "report")
		d, err := StartDetached(os.Args[0], []string{"-test.run=^TestDetachedHelperModes$"}, []string{envReport + "=" + report}, snap.PID, snap.StartMS)
		if err != nil {
			t.Fatalf("StartDetached from %q = %v", tc.from, err)
		}
		got := waitFile(t, report)
		child, ok := Lookup(d.PID)
		if tc.release {
			// Release leaves the process running; it is stopped by its identity instead.
			d.Release()
			if _, still := Lookup(d.PID); !still {
				t.Fatal("Release stopped the process")
			}
			_ = Terminate(child.PID, child.StartMS)
		} else {
			d.Kill()
		}
		if !ok || child.PPID != snap.PID {
			t.Fatalf("from %q: child parent = %d (found %v), want %d rather than this test (%d)", tc.from, child.PPID, ok, snap.PID, os.Getpid())
		}
		desktop, root, _ := strings.Cut(got, "\n")
		if !strings.EqualFold(desktop, tc.want) {
			t.Fatalf("from %q: child desktop = %q, want %q", tc.from, desktop, tc.want)
		}
		if root == "" || root != os.Getenv("SYSTEMROOT") {
			t.Fatalf("child SYSTEMROOT = %q, want %q", root, os.Getenv("SYSTEMROOT"))
		}
	}
}

// The regression this placement fixes: agy closes a command's desktop when the command ends, and
// a process that inherited it cannot load user32 afterwards. A launcher runs on a fresh desktop,
// starts one probe through StartDetached and one as a plain child, and exits; then the desktop is
// closed. Only the plain probe, which kept the closed desktop, fails.
func TestStartDetachedOutlivesTheDesktopItWasStartedFrom(t *testing.T) {
	station := windowStation(t)
	user32 := syscall.NewLazyDLL("user32.dll")
	name := fmt.Sprintf("iris-proc-test-%d", os.Getpid())
	desk, _, err := user32.NewProc("CreateDesktopW").Call(uintptr(unsafe.Pointer(&utf16.Encode([]rune(name + "\x00"))[0])), 0, 0, 0, windows.GENERIC_ALL, 0)
	if desk == 0 {
		t.Fatalf("CreateDesktopW: %v", err)
	}
	closeDesktop := user32.NewProc("CloseDesktop")
	closed := false
	defer func() {
		if !closed {
			_, _, _ = closeDesktop.Call(desk)
		}
	}()

	dir := t.TempDir()
	for _, sub := range []string{"moved", "kept"} {
		if err := os.Mkdir(filepath.Join(dir, sub), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	me := self(t)
	env, err := environmentBlock(append(os.Environ(), envLaunch+"="+dir, fmt.Sprintf("%s=%d:%d", envParent, me.PID, me.StartMS)))
	if err != nil {
		t.Fatal(err)
	}
	cmdLine, _ := windows.UTF16PtrFromString(windows.ComposeCommandLine([]string{os.Args[0], "-test.run=^TestDetachedHelperModes$"}))
	si := &windows.StartupInfo{Flags: windows.STARTF_USESTDHANDLES}
	si.Cb = uint32(unsafe.Sizeof(*si))
	si.Desktop = &utf16.Encode([]rune(station + `\` + name + "\x00"))[0]
	var pi windows.ProcessInformation
	if err := windows.CreateProcess(nil, cmdLine, nil, nil, false, windows.CREATE_UNICODE_ENVIRONMENT|windows.DETACHED_PROCESS, &env[0], nil, si, &pi); err != nil {
		t.Fatalf("start the launcher on %s: %v", name, err)
	}
	_ = windows.CloseHandle(pi.Thread)
	defer func() { _ = windows.CloseHandle(pi.Process) }()
	if status, err := windows.WaitForSingleObject(pi.Process, 30000); err != nil || status != windows.WAIT_OBJECT_0 {
		_ = windows.TerminateProcess(pi.Process, 1)
		t.Fatalf("launcher did not exit: status=%d, err=%v", status, err)
	}
	if got := waitFile(t, filepath.Join(dir, "launched")); got != "moved started; kept started" {
		t.Fatalf("launcher: %s", got)
	}
	probes := map[string]windows.Handle{}
	for _, sub := range []string{"moved", "kept"} {
		pid, err := strconv.Atoi(waitFile(t, filepath.Join(dir, sub, "pid")))
		if err != nil {
			t.Fatal(err)
		}
		h, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.PROCESS_TERMINATE, false, uint32(pid))
		if err != nil {
			t.Fatalf("open %s probe: %v", sub, err)
		}
		probes[sub] = h
		t.Cleanup(func() {
			_ = windows.TerminateProcess(h, 1)
			_ = windows.CloseHandle(h)
		})
	}

	if ok, _, err := closeDesktop.Call(desk); ok == 0 {
		t.Fatalf("CloseDesktop: %v", err)
	}
	closed = true
	for _, sub := range []string{"moved", "kept"} {
		if err := os.WriteFile(filepath.Join(dir, sub, "go"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if got := waitFile(t, filepath.Join(dir, "moved", "probe")); got != "user32 ok" {
		t.Fatalf("a process from StartDetached could not load user32 after its launcher's desktop closed: %s", got)
	}
	// The plain probe must finish, either reporting DLL initialization failure or exiting with
	// its status. Missing output alone is not evidence: it could mean the probe never ran.
	if status, err := windows.WaitForSingleObject(probes["kept"], 30000); err != nil || status != windows.WAIT_OBJECT_0 {
		t.Fatalf("plain probe did not exit: status=%d, err=%v", status, err)
	}
	var code uint32
	if err := windows.GetExitCodeProcess(probes["kept"], &code); err != nil {
		t.Fatal(err)
	}
	if code != 0xC0000142 {
		got, err := os.ReadFile(filepath.Join(dir, "kept", "probe"))
		if code != 0 || err != nil || string(got) != "user32 init failed" {
			t.Fatalf("plain probe: exit=%#x, output=%q, err=%v; want DLL initialization failure", code, got, err)
		}
	}
}

// Every refusal happens before a process could start.
func TestStartDetachedRefusesBeforeAnythingStarts(t *testing.T) {
	me := self(t)
	origCreate := createProcess
	t.Cleanup(func() { createProcess = origCreate })
	createProcess = func(*uint16, *uint16, *windows.SecurityAttributes, *windows.SecurityAttributes, bool, uint32, *uint16, *uint16, *windows.StartupInfo, *windows.ProcessInformation) error {
		t.Error("a process was created despite a refusal")
		return errors.New("refused")
	}
	exe := os.Args[0]
	expect := func(label string, want error, err error) {
		t.Helper()
		if err == nil || (want != nil && !errors.Is(err, want)) {
			t.Errorf("%s: err = %v, want %v", label, err, want)
		}
	}

	replaceDesktopInfo(t, `WinSta0\`)
	_, err := StartDetached(exe, nil, nil, me.PID, me.StartMS)
	expect("malformed desktop", errDesktop, err)
	replaceDesktopInfo(t, "")

	_, err = StartDetached(exe+"\x00", nil, nil, me.PID, me.StartMS)
	expect("NUL in the executable", nil, err)
	_, err = StartDetached(exe, []string{"a\x00b"}, nil, me.PID, me.StartMS)
	expect("NUL in an argument", nil, err)
	_, err = StartDetached(exe, nil, []string{"A=1\x00"}, me.PID, me.StartMS)
	expect("NUL in the environment", nil, err)
	_, err = StartDetached(exe, nil, []string{""}, me.PID, me.StartMS)
	expect("empty environment entry", nil, err)

	_, err = StartDetached(exe, nil, nil, me.PID, me.StartMS+1)
	expect("another start time", ErrNotFound, err)
	_, err = StartDetached(exe, nil, nil, 0x7FFFFFF0, 100)
	expect("a pid that does not exist", nil, err)

	exited := exec.Command(exe, "-test.run=^$")
	if err := exited.Start(); err != nil {
		t.Fatal(err)
	}
	h, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(exited.Process.Pid))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = windows.CloseHandle(h) }()
	var created, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &created, &exit, &kernel, &user); err != nil {
		t.Fatal(err)
	}
	if _, err := windows.WaitForSingleObject(h, windows.INFINITE); err != nil {
		t.Fatal(err)
	}
	_, err = StartDetached(exe, nil, nil, exited.Process.Pid, created.Nanoseconds()/1e6)
	expect("an exited parent whose object a handle keeps", ErrNotFound, err)
	_ = exited.Wait()

	origOpen := openProcess
	t.Cleanup(func() { openProcess = origOpen })
	openProcess = func(uint32, bool, uint32) (windows.Handle, error) { return 0, windows.ERROR_ACCESS_DENIED }
	_, err = StartDetached(exe, nil, nil, me.PID, me.StartMS)
	expect("a parent that refuses to be opened", windows.ERROR_ACCESS_DENIED, err)
	openProcess = origOpen

	origList, origUpdate := newAttributeList, updateAttributeList
	t.Cleanup(func() { newAttributeList, updateAttributeList = origList, origUpdate })
	newAttributeList = func(uint32) (*windows.ProcThreadAttributeListContainer, error) {
		return nil, windows.ERROR_NOT_ENOUGH_MEMORY
	}
	_, err = StartDetached(exe, nil, nil, me.PID, me.StartMS)
	expect("no attribute list", windows.ERROR_NOT_ENOUGH_MEMORY, err)
	newAttributeList = origList
	updateAttributeList = func(*windows.ProcThreadAttributeListContainer, uintptr, unsafe.Pointer, uintptr) error {
		return windows.ERROR_INVALID_PARAMETER
	}
	_, err = StartDetached(exe, nil, nil, me.PID, me.StartMS)
	expect("a parent that cannot be named", windows.ERROR_INVALID_PARAMETER, err)
}

// A program that Windows cannot start is an error, not a process.
func TestStartDetachedReportsAProgramThatCannotStart(t *testing.T) {
	me := self(t)
	replaceDesktopInfo(t, "")
	if _, err := StartDetached(filepath.Join(t.TempDir(), "missing.exe"), nil, nil, me.PID, me.StartMS); err == nil {
		t.Fatal("started a program that does not exist")
	}
}

func TestTargetDesktop(t *testing.T) {
	for _, tc := range []struct {
		info, want string
		refused    bool
	}{
		{info: `WinSta0\Default`},
		{info: `winsta0\default`},
		{info: `Service-0x0-3e7$\Default`},
		{info: ""},
		{info: "Default"},
		{info: `WinSta0\exebox-3IMKI5K6IRVFMZCP76RN2SZEYF`, want: `WinSta0\Default`},
		{info: `Service-0x0-3e7$\exebox-1`, want: `Service-0x0-3e7$\Default`},
		// agy 1.5.8 names its command desktops alone; the window station is the inherited one.
		{info: "exebox-W7DZUNVZMBP3XSFQELATKMSQNY", want: "Default"},
		{info: "exe\x00box", refused: true},
		{info: `WinSta0\`, refused: true},
		{info: `\exebox-1`, refused: true},
		{info: `WinSta0\a\b`, refused: true},
		{info: "WinSta0\\exe\x00box", refused: true},
		{info: "Win\x00Sta0\\exebox", refused: true},
	} {
		got, err := targetDesktop(tc.info)
		if tc.refused {
			if !errors.Is(err, errDesktop) {
				t.Errorf("targetDesktop(%q) = %q, %v; want a refusal", tc.info, got, err)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("targetDesktop(%q) = %q, %v; want %q", tc.info, got, err, tc.want)
		}
	}
	// This process's own desktop is one that a detached start can place.
	if _, err := targetDesktop(readDesktopInfo()); err != nil {
		t.Fatalf("this process's desktop %q: %v", readDesktopInfo(), err)
	}
}

func TestEnvironmentBlock(t *testing.T) {
	decode := func(block []uint16) []string {
		var out []string
		for _, s := range strings.Split(string(utf16.Decode(block)), "\x00") {
			if s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	block, err := environmentBlock([]string{"Path=a", "B=1", "PATH=b", "systemroot=C:\\X"})
	if err != nil {
		t.Fatal(err)
	}
	if got := decode(block); strings.Join(got, "|") != `B=1|PATH=b|systemroot=C:\X` {
		t.Fatalf("names are sorted without case, the later entry wins and SYSTEMROOT is kept: %q", got)
	}
	if block[len(block)-1] != 0 || block[len(block)-2] != 0 {
		t.Fatal("block does not end with two NULs")
	}
	block, err = environmentBlock(nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := decode(block); len(got) != 1 || got[0] != "SYSTEMROOT="+os.Getenv("SYSTEMROOT") {
		t.Fatalf("a missing SYSTEMROOT is added: %q", got)
	}
	if _, err := environmentBlock([]string{"A=1\x00B=2"}); err == nil {
		t.Fatal("accepted a NUL inside an entry")
	}
	block, err = environmentBlock([]string{`=D:=D:\work`, `=C:=C:\old`, `=c:=C:\new`, "Z=", "systemroot=C:\\X"})
	if err != nil {
		t.Fatal(err)
	}
	if got := decode(block); strings.Join(got, "|") != `=c:=C:\new|=D:=D:\work|systemroot=C:\X|Z=` {
		t.Fatalf("drive-directory names stay distinct, compare without case and sort before normal names: %q", got)
	}
	for _, kv := range []string{"", "NO_SEPARATOR", "=", "=C:", "==value"} {
		if _, err := environmentBlock([]string{"A=1", kv, "B=2"}); err == nil {
			t.Errorf("accepted malformed entry %q", kv)
		}
	}
}

// A console program started without a console still runs, and its output still arrives through
// the pipe set on the command.
func TestNoConsoleRunsAConsoleProgramWithoutAConsole(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestConsoleProbeHelper$")
	cmd.Env = append(os.Environ(), "IRIS_PROC_CONSOLE_PROBE=1")
	var out strings.Builder
	cmd.Stdout = &out
	NoConsole(cmd)
	if err := cmd.Run(); err != nil {
		t.Fatalf("console program without a console: %v", err)
	}
	if !strings.Contains(out.String(), "console=none") {
		t.Fatalf("probe output = %q", out.String())
	}
}

func TestConsoleProbeHelper(t *testing.T) {
	if os.Getenv("IRIS_PROC_CONSOLE_PROBE") != "1" {
		return
	}
	hwnd, _, _ := syscall.NewLazyDLL("kernel32.dll").NewProc("GetConsoleWindow").Call()
	state := "console=attached"
	if hwnd == 0 {
		state = "console=none"
	}
	_, _ = os.Stdout.WriteString(state + "\n")
	os.Exit(0)
}
