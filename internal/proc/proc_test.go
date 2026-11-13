package proc

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestNearestFrom(t *testing.T) {
	tree := map[int]Snapshot{
		10: {PID: 10, PPID: 9, Name: "iris"},
		9:  {PID: 9, PPID: 8, Name: "/bin/bash"},
		8:  {PID: 8, PPID: 7, Name: "python3"},
		7:  {PID: 7, PPID: 2, Name: "grok"},
		2:  {PID: 2, PPID: 1, Name: "grok"},
		6:  {PID: 6, PPID: 5, Name: "iris"},
		5:  {PID: 5, PPID: 4, Name: `C:\Windows\System32\cmd.exe`},
		4:  {PID: 4, PPID: 1, Name: `C:\Tools\cursor-agent.exe`},
		3:  {PID: 3, PPID: 1, Name: "claude"},
	}
	get := func(pid int) (Snapshot, bool) {
		s, ok := tree[pid]
		return s, ok
	}
	got, ok := NearestFrom(10, get)
	if !ok || got.PID != 7 || got.Name != "grok" {
		t.Fatalf("past a shell and a helper = %+v ok=%v", got, ok)
	}
	got, ok = NearestFrom(7, get)
	if !ok || got.PID != 2 {
		t.Fatalf("stop at the first client = %+v", got)
	}
	got, ok = NearestFrom(6, get)
	if !ok || got.PID != 4 {
		t.Fatalf("windows shell = %+v", got)
	}
	got, ok = NearestFrom(3, get)
	if ok {
		t.Fatalf("init is not an ancestor worth recording, got %+v", got)
	}

	onlyHelper := map[int]Snapshot{
		4: {PID: 4, PPID: 3, Name: "iris"},
		3: {PID: 3, PPID: 1, Name: "python3"},
	}
	got, ok = NearestFrom(4, func(pid int) (Snapshot, bool) {
		s, ok := onlyHelper[pid]
		return s, ok
	})
	if !ok || got.Name != "python3" {
		t.Fatalf("fallback nearest non-shell = %+v", got)
	}
}

func TestLookupLive(t *testing.T) {
	got, ok := Lookup(os.Getpid())
	if !ok || got.PID != os.Getpid() || got.Name == "" || got.StartMS <= 0 {
		t.Fatalf("current process = %+v, ok %t", got, ok)
	}
}

func TestNearestLive(t *testing.T) {
	a, err := Nearest()
	if err != nil {
		t.Fatal(err)
	}
	if a.PID <= 1 {
		t.Fatalf("pid %d", a.PID)
	}
	if runtime.GOOS == "linux" && a.StartMS <= 0 {
		t.Fatalf("start %d", a.StartMS)
	}
}

func TestExecutableAndTerminateValidation(t *testing.T) {
	self, ok := Lookup(os.Getpid())
	if !ok {
		t.Fatal("self lookup")
	}
	exe, err := Executable(self.PID, self.StartMS)
	if err != nil || !filepath.IsAbs(exe) {
		t.Fatalf("Executable(self) = %q, %v", exe, err)
	}
	dir := t.TempDir()
	reg := filepath.Join(dir, "agy")
	if err := os.WriteFile(reg, []byte("bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Executable(0, 100); !errors.Is(err, ErrNotFound) {
		t.Fatalf("pid 0: %v", err)
	}
	if _, err := Executable(10, 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("start 0: %v", err)
	}
	if _, err := executableFrom(10, 100, func(int) (Snapshot, bool) { return Snapshot{}, false }, nil, os.Stat); !errors.Is(err, ErrNotFound) {
		t.Fatal("missing snapshot")
	}
	if _, err := executableFrom(10, 100, func(int) (Snapshot, bool) { return Snapshot{PID: 10, StartMS: 99}, true }, nil, os.Stat); !errors.Is(err, ErrNotFound) {
		t.Fatal("mismatched startMS")
	}
	live := func(int) (Snapshot, bool) { return Snapshot{PID: 10, StartMS: 100}, true }
	for name, readExe := range map[string]func(int, Snapshot) (string, error){
		"error":    func(int, Snapshot) (string, error) { return "", errors.New("denied") },
		"empty":    func(int, Snapshot) (string, error) { return "", nil },
		"deleted":  func(int, Snapshot) (string, error) { return reg + " (deleted)", nil },
		"relative": func(int, Snapshot) (string, error) { return "agy", nil },
		"dir":      func(int, Snapshot) (string, error) { return dir, nil },
		"missing":  func(int, Snapshot) (string, error) { return filepath.Join(dir, "missing"), nil },
	} {
		if _, err := executableFrom(10, 100, live, readExe, os.Stat); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	calls := 0
	reused := func(int) (Snapshot, bool) {
		calls++
		if calls > 1 {
			return Snapshot{PID: 10, StartMS: 200}, true
		}
		return Snapshot{PID: 10, StartMS: 100}, true
	}
	if _, err := executableFrom(10, 100, reused, func(int, Snapshot) (string, error) { return reg, nil }, os.Stat); !errors.Is(err, ErrNotFound) {
		t.Fatalf("PID reuse after stat must fail: %v", err)
	}
	if got, err := executableFrom(10, 100, live, func(int, Snapshot) (string, error) { return reg, nil }, os.Stat); err != nil || got != reg {
		t.Fatalf("valid regular file = %q, %v", got, err)
	}
	if err := Terminate(0, 100); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Terminate pid 0: %v", err)
	}
	if err := Terminate(self.PID, self.StartMS+1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Terminate mismatched startMS: %v", err)
	}
}
