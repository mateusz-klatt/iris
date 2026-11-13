//go:build linux

package proc

import (
	"encoding/binary"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestParseStat(t *testing.T) {
	s := "1055815 (python3) R 1055804 1055804 1055804 0 -1 4194304 974 0 0 0 1 0 0 0 20 0 1 0 2375890 24555520 2498 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0"
	state, ppid, ticks, err := parseStat(s)
	if err != nil || state != "R" || ppid != 1055804 || ticks != 2375890 {
		t.Fatalf("state=%s ppid=%d ticks=%d err=%v", state, ppid, ticks, err)
	}
	// comm itself contains a parenthesis and spaces.
	s = "12 (a) b) S 99 1 1 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 50 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0"
	state, ppid, ticks, err = parseStat(s)
	if err != nil || state != "S" || ppid != 99 || ticks != 50 {
		t.Fatalf("comm with paren: state=%s ppid=%d ticks=%d err=%v", state, ppid, ticks, err)
	}
}

func TestParseStatRefusesDamage(t *testing.T) {
	for _, s := range []string{
		"no parenthesis",
		"1 (name)",
		"1 (name) S 2 3",
		"1 (name) S x 1 1 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 50 0",
		"1 (name) S 2 1 1 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 x 0",
	} {
		if _, _, _, err := parseStat(s); err == nil {
			t.Errorf("%q", s)
		}
	}
}

func TestStartFrom(t *testing.T) {
	stat := "12 (a) S 99 1 1 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 250 0"
	ok := func() (int64, int64, error) { return 1000, 100, nil }
	if got, err := startFrom(stat, ok); got != 1002500 || err != nil {
		t.Fatalf("%d %v", got, err)
	}
	if _, err := startFrom("damaged", ok); err == nil {
		t.Fatal("damaged stat")
	}
	broken := errors.New("no clock")
	if _, err := startFrom(stat, func() (int64, int64, error) { return 0, 0, broken }); !errors.Is(err, broken) {
		t.Fatal(err)
	}
}

func TestBootTime(t *testing.T) {
	read := func(text string, err error) func(string) ([]byte, error) {
		return func(path string) ([]byte, error) {
			if path != "/proc/stat" {
				t.Fatal(path)
			}
			return []byte(text), err
		}
	}
	if got, err := bootTime(read("cpu 1 2\nbtime 1700000000\nprocesses 3\n", nil)); got != 1700000000 || err != nil {
		t.Fatalf("%d %v", got, err)
	}
	if _, err := bootTime(read("cpu 1 2\n", nil)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if _, err := bootTime(read("btime soon\n", nil)); err == nil {
		t.Fatal("damaged line")
	}
	broken := errors.New("unreadable")
	if _, err := bootTime(read("", broken)); !errors.Is(err, broken) {
		t.Fatal(err)
	}
}

func auxv(words ...uint64) []byte {
	var out []byte
	for _, w := range words {
		if strconv.IntSize == 64 {
			out = binary.NativeEndian.AppendUint64(out, w)
		} else {
			out = binary.NativeEndian.AppendUint32(out, uint32(w))
		}
	}
	return out
}

func TestClkTck(t *testing.T) {
	read := func(raw []byte, err error) func(string) ([]byte, error) {
		return func(string) ([]byte, error) { return raw, err }
	}
	for name, c := range map[string]struct {
		raw  []byte
		err  error
		want int64
	}{
		"present":    {auxv(6, 4096, 17, 250, 0, 0), nil, 250},
		"zero value": {auxv(17, 0, 0, 0), nil, 100},
		"terminated": {auxv(6, 4096, 0, 0, 17, 250), nil, 100},
		"truncated":  {auxv(6, 4096), nil, 100},
		"unreadable": {nil, errors.New("denied"), 100},
	} {
		if got := clkTck(read(c.raw, c.err)); got != c.want {
			t.Errorf("%s: %d", name, got)
		}
	}
}

func TestAuxWordReadsBothWidths(t *testing.T) {
	if got := auxWord(binary.NativeEndian.AppendUint64(nil, 1<<40)); got != 1<<40 {
		t.Fatal(got)
	}
	if got := auxWord(binary.NativeEndian.AppendUint32(nil, 7)); got != 7 {
		t.Fatal(got)
	}
}

func TestStatusValue(t *testing.T) {
	status := []byte("Name:\tiris\nPPid:\t12\n")
	if got := statusValue(status, "PPid:"); got != "12" {
		t.Fatal(got)
	}
	if got := statusValue(status, "Missing:"); got != "" {
		t.Fatal(got)
	}
}

func TestLookupReadsThisProcess(t *testing.T) {
	got, ok := lookupOS(os.Getpid())
	if !ok || got.PPID != os.Getppid() || got.StartMS <= 0 || !strings.Contains(got.Name, "proc.test") {
		t.Fatalf("%+v %v", got, ok)
	}
}

func TestLookupOfAProcessWithoutACommandLine(t *testing.T) {
	if got := argv0(0x7FFFFFF0); got != "" {
		t.Fatal(got)
	}
	if _, _, err := statEvidence(0x7FFFFFF0); err == nil {
		t.Fatal("no such process")
	}
}

func TestLookupRejectsAZombie(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Wait() }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		state, _, err := statEvidence(cmd.Process.Pid)
		if err == nil && state == "Z" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("child did not become a zombie: state %q, error %v", state, err)
		}
		time.Sleep(time.Millisecond)
	}
	if got, ok := Lookup(cmd.Process.Pid); ok {
		t.Fatalf("zombie was accepted as live: %+v", got)
	}
}
