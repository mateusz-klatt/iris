package doctor

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/integrate"
	"github.com/mateusz-klatt/iris/internal/kimiwake"
	"github.com/mateusz-klatt/iris/internal/proc"
)

func kimiRoot(t *testing.T, root string, err error) {
	t.Helper()
	old := kimiWakeRoot
	t.Cleanup(func() { kimiWakeRoot = old })
	kimiWakeRoot = func(integrate.Layout) (string, error) { return root, err }
}

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	raw, _ := json.Marshal(v)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// The preference line says what is configured, in every state the file can be in.
func TestKimiWakeReportNamesThePreference(t *testing.T) {
	for _, tc := range []struct {
		file string
		want string
	}{
		{"", "not integrated, so wake is off"},
		{"default\n", "preference default: off until Kimi's native acceptance (ADR 030)"},
		{"off\n", "preference off"},
		{"on\n", "preference on: a user prompt of an interactive session asks the model"},
		{"maybe\n", "preference unreadable, so wake is off"},
	} {
		root := t.TempDir()
		if tc.file != "" {
			if err := os.WriteFile(kimiwake.PreferencePath(root), []byte(tc.file), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		kimiRoot(t, root, nil)
		if lines := kimiWakeReport(integrate.Layout{}, time.Now()); len(lines) != 1 || !strings.HasPrefix(lines[0], tc.want) {
			t.Fatalf("%q: %q", tc.file, lines)
		}
	}
	kimiRoot(t, "", errors.New("no account directory"))
	if lines := kimiWakeReport(integrate.Layout{}, time.Now()); len(lines) != 1 || !strings.Contains(lines[0], "could not be found") {
		t.Fatalf("no root: %q", lines)
	}
}

// Each session's record gets one line: a running watcher, a latch and its age, a request not
// answered yet, no watcher, or a record that cannot be read. A Kimi process that has ended says so.
func TestKimiWakeReportListsEachSession(t *testing.T) {
	self, ok := proc.Lookup(os.Getpid())
	if !ok {
		t.Fatal("this process")
	}
	root := t.TempDir()
	if err := os.WriteFile(kimiwake.PreferencePath(root), []byte("on\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.UnixMilli(1_800_000_000_000)
	watchers := filepath.Join(root, "watchers")
	record := func(pid int, start int64, session string, r kimiwake.Record) {
		r.Version = 1
		writeJSON(t, filepath.Join(watchers, fmt.Sprintf("%d-%d-%s.json", pid, start, session)), r)
	}
	record(self.PID, self.StartMS, "session_a", kimiwake.Record{Attempt: &kimiwake.Attempt{PID: self.PID, StartMS: self.StartMS, ID: "w"}})
	record(self.PID, self.StartMS, "session_b", kimiwake.Record{Latch: &kimiwake.Latch{State: kimiwake.LatchFailed, AtMS: now.Add(-90 * time.Second).UnixMilli()}})
	record(self.PID, self.StartMS, "session_c", kimiwake.Record{ArmRequestedMS: now.Add(-5 * time.Second).UnixMilli()})
	record(self.PID, self.StartMS, "session_d", kimiwake.Record{})
	record(7, 8, "session_e", kimiwake.Record{})
	if err := os.WriteFile(filepath.Join(watchers, fmt.Sprintf("%d-%d-session_f.json", self.PID, self.StartMS)), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	kimiRoot(t, root, nil)
	lines := kimiWakeReport(integrate.Layout{}, now)
	head := fmt.Sprintf("session %%s of kimi pid %d", self.PID)
	want := []string{
		lines[0],
		fmt.Sprintf(head, "session_a") + fmt.Sprintf(": watcher pid %d running", self.PID),
		fmt.Sprintf(head, "session_b") + ": latched failed 1m30s ago; SessionStart or a watcher started by hand clears it",
		fmt.Sprintf(head, "session_c") + ": watcher requested 5s ago, not started yet",
		fmt.Sprintf(head, "session_d") + ": no watcher",
		fmt.Sprintf(head, "session_f") + ": record unreadable",
		"session session_e of kimi pid 7, which has ended: no watcher",
	}
	if len(lines) != len(want) {
		t.Fatalf("%q", lines)
	}
	for _, w := range want {
		found := false
		for _, l := range lines {
			found = found || l == w
		}
		if !found {
			t.Fatalf("missing %q in %q", w, lines)
		}
	}
	if text := (Result{KimiWake: lines}).Format(); !strings.Contains(text, "kimi wake: "+lines[1]+"\n") {
		t.Fatalf("rendered %q", text)
	}

	if err := os.RemoveAll(watchers); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(watchers, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if lines := kimiWakeReport(integrate.Layout{}, now); len(lines) != 2 || lines[1] != "watcher records unreadable" {
		t.Fatalf("broken records: %q", lines)
	}
}
