//go:build linux

package hook

import (
	"os"
	"testing"
)

func TestReadProcArgs(t *testing.T) {
	if args, err := dispatchCursorArgs(os.Getpid(), 0); err != nil || len(args) == 0 {
		t.Fatalf("dispatch: %v %v", args, err)
	}
	if _, err := readProcArgs(0, 0); err == nil {
		t.Fatal("pid 0 had arguments")
	}
	if _, err := readProcArgs(1<<30, 0); err == nil {
		t.Fatal("a missing process had arguments")
	}
	args, err := readProcArgs(os.Getpid(), 0)
	if err != nil || len(args) == 0 {
		t.Fatalf("this process: %v %v", args, err)
	}
	if _, err := argsFromCmdline(nil); err == nil {
		t.Fatal("an empty command line was accepted")
	}
	got, err := argsFromCmdline([]byte("cursor-agent\x00-p\x00hi\x00"))
	if err != nil || len(got) != 3 || got[1] != "-p" {
		t.Fatalf("%v %v", got, err)
	}
	prev := readCursorArgs
	readCursorArgs = nil
	t.Cleanup(func() { readCursorArgs = prev })
	if _, err := dispatchCursorArgs(1, 0); err == nil {
		t.Fatal("a missing reader returned arguments")
	}
}
