package kimiwake

import (
	"os"
	"path/filepath"
	"testing"
)

// Records lists every watcher record for a report, in file-name order, with whether its watcher
// and its Kimi process run. A record that cannot be read is listed as unreadable, a file that is
// not a record is skipped, and a state directory that does not exist has none.
func TestRecordsListsEveryWatcherRecord(t *testing.T) {
	if entries, err := (&Store{Root: filepath.Join(t.TempDir(), "none")}).Records(); err != nil || entries != nil {
		t.Fatalf("no state: %v %v", entries, err)
	}
	s, _, procs := newStore(t)
	live, latched, broken := keyFor("session_a"), keyFor("session_b"), keyFor("session_c")
	ended := Key{PID: 7, StartMS: 8, Session: "session_d"}
	put(t, s, live, Record{Version: recordVersion, Attempt: &Attempt{PID: 9001, StartMS: 9002, ID: "w"}})
	put(t, s, latched, Record{Version: recordVersion, Latch: &Latch{State: LatchFailed, AtMS: 5}})
	put(t, s, ended, Record{Version: recordVersion, ArmRequestedMS: 6})
	procs.set(9001, 9002, true)
	for name, body := range map[string]string{s.recordPath(broken): "{", filepath.Join(s.Root, watchersName, "notes.txt"): "x"} {
		if err := os.WriteFile(name, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := s.Records()
	if err != nil || len(entries) != 4 {
		t.Fatalf("%+v %v", entries, err)
	}
	byKey := map[Key]Entry{}
	for _, e := range entries {
		byKey[e.Key] = e
	}
	if e := byKey[live]; !e.Watching || !e.KimiAlive || e.Unreadable != nil {
		t.Fatalf("live: %+v", e)
	}
	if e := byKey[latched]; e.Watching || e.Record.Latch == nil || e.Record.Latch.State != LatchFailed {
		t.Fatalf("latched: %+v", e)
	}
	if e := byKey[broken]; e.Unreadable == nil || e.Watching {
		t.Fatalf("broken: %+v", e)
	}
	if e := byKey[ended]; e.KimiAlive || e.Record.ArmRequestedMS != 6 {
		t.Fatalf("ended: %+v", e)
	}

	blocked := &Store{Root: t.TempDir()}
	if err := os.WriteFile(filepath.Join(blocked.Root, watchersName), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := blocked.Records(); err == nil {
		t.Fatal("a watchers path that is not a directory was read")
	}
}
