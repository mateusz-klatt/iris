package proc

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestLookupReadsThisProcess(t *testing.T) {
	before := time.Now().UnixMilli()
	got, ok := lookupOS(os.Getpid())
	if !ok || got.PID != os.Getpid() || got.PPID != os.Getppid() {
		t.Fatalf("%+v %v", got, ok)
	}
	if want := filepath.Base(os.Args[0]); got.Name != want {
		t.Fatalf("%q, want %q", got.Name, want)
	}
	if got.StartMS <= 0 || got.StartMS > before {
		t.Fatalf("started at %d, now %d", got.StartMS, before)
	}
}

func TestLookupKeepsTheKernelNameWhenArgumentsAreRefused(t *testing.T) {
	got, ok := lookupOS(1)
	if !ok || got.Name != "launchd" || got.PPID != 0 {
		t.Fatalf("%+v %v", got, ok)
	}
}

// startNamed runs sleep with a chosen argv[0], which is how a client that retitles itself appears.
func startNamed(t *testing.T, argv0 string) int {
	t.Helper()
	cmd := &exec.Cmd{Path: "/bin/sleep", Args: []string{argv0, "30"}}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return cmd.Process.Pid
}

func TestLookupNamesAClientFromItsFirstArgument(t *testing.T) {
	for argv0, want := range map[string]string{
		"claude":              "claude",
		"/opt/bin/Claude":     "claude",
		"not-a-client":        "sleep",
		"-zsh":                "sleep",
		"/usr/local/bin/node": "sleep",
	} {
		pid := startNamed(t, argv0)
		got, ok := lookupOS(pid)
		if !ok || got.Name != want {
			t.Fatalf("argv[0] %q: %+v %v, want %q", argv0, got, ok, want)
		}
	}
}

func TestFirstArgumentReadsTheProcargsLayout(t *testing.T) {
	for name, tc := range map[string]struct {
		raw  string
		exe  string
		want string
	}{
		"padded":    {"\x02\x00\x00\x00/path/2.1.319\x00\x00\x00\x00claude\x00--flag\x00", "/path/2.1.319", "claude"},
		"unpadded":  {"\x01\x00\x00\x00/bin/sh\x00sh\x00", "/bin/sh", "sh"},
		"no end":    {"\x01\x00\x00\x00/bin/sh\x00sh", "/bin/sh", "sh"},
		"path only": {"\x00\x00\x00\x00/bin/sh\x00", "/bin/sh", ""},
		"cut path":  {"\x01\x00\x00\x00/bin", "", ""},
		"short":     {"\x01\x00", "", ""},
		"empty":     {"", "", ""},
	} {
		if got := executablePath([]byte(tc.raw)); got != tc.exe {
			t.Fatalf("%s exe: %q, want %q", name, got, tc.exe)
		}
		if got := firstArgument([]byte(tc.raw)); got != tc.want {
			t.Fatalf("%s: %q, want %q", name, got, tc.want)
		}
	}
}

func TestExecutableOSReadsThisProcessAndRefusesKernelProcesses(t *testing.T) {
	if got, err := executableOS(os.Getpid(), Snapshot{}); err != nil || got == "" {
		t.Fatalf("got %q, %v", got, err)
	}
	if _, err := executableOS(1, Snapshot{}); err == nil {
		t.Fatal("expected kernel refusal for launchd")
	}
}
