//go:build linux || darwin

package proc

import (
	"errors"
	"os"
	"testing"
)

func TestWatchRefusesInitAndMissingProcesses(t *testing.T) {
	if _, err := watch(1); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := watch(0x7FFFFFF0); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}

func TestWatchParentSeesThisTestRunner(t *testing.T) {
	w, err := WatchParent()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if !w.Alive() {
		t.Fatal("the parent of this test is running")
	}
}

func TestAliveNoticesAReplacedParent(t *testing.T) {
	w, err := WatchParent()
	if err != nil {
		t.Fatal(err)
	}
	reused := &ParentWatch{pid: w.pid, start: w.start + 1}
	if reused.Alive() {
		t.Fatal("same pid, another start time")
	}
	other := &ParentWatch{pid: os.Getpid(), start: w.start}
	if other.Alive() {
		t.Fatal("this process is not the parent")
	}
}

func TestLookupRefusesWhatDoesNotExist(t *testing.T) {
	if _, ok := lookupOS(0); ok {
		t.Fatal("pid 0")
	}
	if _, ok := lookupOS(0x7FFFFFF0); ok {
		t.Fatal("no such process")
	}
}
