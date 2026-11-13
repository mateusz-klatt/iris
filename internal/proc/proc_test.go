package proc

import (
	"os"
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
