package proc

import (
	"errors"
	"os"
	"slices"
	"testing"
)

// Args and ReadsTerminal answer only for the process they were asked about. An identity that
// does not match, before or after the read, is not found, and a failed read is its own error.
func TestIdentifiedChecksTheProcessBeforeAndAfterTheRead(t *testing.T) {
	denied := errors.New("denied")
	live := func(int) (Snapshot, bool) { return Snapshot{PID: 10, StartMS: 100}, true }
	gone := func(int) (Snapshot, bool) { return Snapshot{}, false }
	reused := func(int) (Snapshot, bool) { return Snapshot{PID: 10, StartMS: 99}, true }
	read := func(int) (string, error) { return "fact", nil }
	for name, tc := range map[string]struct {
		pid   int
		start int64
		get   func(int) (Snapshot, bool)
		read  func(int) (string, error)
		want  string
		err   error
	}{
		"no pid":     {0, 100, live, read, "", ErrNotFound},
		"no start":   {10, 0, live, read, "", ErrNotFound},
		"gone":       {10, 100, gone, read, "", ErrNotFound},
		"reused":     {10, 100, reused, read, "", ErrNotFound},
		"read fails": {10, 100, live, func(int) (string, error) { return "", denied }, "", denied},
		"read":       {10, 100, live, read, "fact", nil},
	} {
		got, err := identified(tc.pid, tc.start, tc.get, tc.read)
		if got != tc.want || !errors.Is(err, tc.err) {
			t.Fatalf("%s: %q %v", name, got, err)
		}
	}
	// The process ended and its pid was reused while it was read.
	calls := 0
	flip := func(int) (Snapshot, bool) {
		calls++
		if calls > 1 {
			return reused(10)
		}
		return live(10)
	}
	if got, err := identified(10, 100, flip, read); got != "" || !errors.Is(err, ErrNotFound) {
		t.Fatalf("reused during the read: %q %v", got, err)
	}
}

// Args reads the arguments a live process was started with, here this test's own, and refuses a
// start time that is not the process's.
func TestArgsReadsThisProcess(t *testing.T) {
	self, ok := Lookup(os.Getpid())
	if !ok {
		t.Fatal("self lookup")
	}
	args, err := Args(self.PID, self.StartMS)
	if err != nil || !slices.Equal(args, os.Args) {
		t.Fatalf("Args = %q, %v; os.Args %q", args, err, os.Args)
	}
	if _, err := Args(self.PID, self.StartMS+1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another start time: %v", err)
	}
	if _, err := ReadsTerminal(self.PID, self.StartMS+1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another start time: %v", err)
	}
}
