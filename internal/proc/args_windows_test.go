//go:build windows

package proc

import (
	"errors"
	"os"
	"slices"
	"testing"

	"golang.org/x/sys/windows"
)

func TestArgsOfAProcessThatIsGone(t *testing.T) {
	if _, err := argsOS(0x7FFFFFF0); err == nil {
		t.Fatal("read the command line of no process")
	}
}

// A query that fails is its error, and a process with an empty command line has no arguments.
func TestArgsReportsAFailedOrEmptyCommandLine(t *testing.T) {
	defer func(q func(windows.Handle, []byte) error) { queryCommandLine = q }(queryCommandLine)
	queryCommandLine = func(windows.Handle, []byte) error { return windows.ERROR_ACCESS_DENIED }
	if _, err := argsOS(os.Getpid()); !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		t.Fatalf("a refused query: %v", err)
	}
	queryCommandLine = func(_ windows.Handle, buf []byte) error {
		clear(buf)
		return nil
	}
	if _, err := argsOS(os.Getpid()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an empty command line: %v", err)
	}
}

// A doubled quote is split differently by Rust than by the C runtime, so it is refused rather than
// risk hiding a print flag. Ordinary quoting splits as Go does.
func TestSplitCommandLineRefusesADoubledQuote(t *testing.T) {
	for _, line := range []string{`grok.exe --system-prompt "a"" b" --single=PONG`, `agy.exe -p ""`, ""} {
		if args, err := splitCommandLine(line); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%s: %q %v", line, args, err)
		}
	}
	args, err := splitCommandLine(`agy.exe -p "reply with exactly PONG" --model m`)
	if err != nil || !slices.Equal(args, []string{"agy.exe", "-p", "reply with exactly PONG", "--model", "m"}) {
		t.Fatalf("%q %v", args, err)
	}
}

func TestReadsTerminalOSOfAProcessThatIsGone(t *testing.T) {
	if _, err := readsTerminalOS(0x7FFFFFF0); err == nil {
		t.Fatal("read the standard input of no process")
	}
}
