//go:build darwin

package proc

import (
	"errors"
	"io"
	"os"
	"slices"
	"syscall"
	"testing"
)

// kern.procargs2 holds the argument count, the executable path and its padding, the arguments and
// then the environment. Only the counted arguments are read, and a buffer cut short gives none.
func TestProcargsArgsReadsTheLayout(t *testing.T) {
	for name, tc := range map[string]struct {
		raw  string
		want []string
	}{
		"padded":     {"\x02\x00\x00\x00/opt/agy\x00\x00\x00\x00agy\x00-p\x00PATH=/bin\x00", []string{"agy", "-p"}},
		"one":        {"\x01\x00\x00\x00/bin/sh\x00sh\x00", []string{"sh"}},
		"empty":      {"\x03\x00\x00\x00/opt/agy\x00agy\x00-p\x00\x00", []string{"agy", "-p", ""}},
		"cut":        {"\x02\x00\x00\x00/bin/sh\x00sh\x00", nil},
		"unended":    {"\x01\x00\x00\x00/bin/sh\x00sh", nil},
		"no count":   {"\x00\x00\x00\x00/bin/sh\x00", nil},
		"past data":  {"\xff\x00\x00\x00/bin/sh\x00sh\x00", nil},
		"cut path":   {"\x01\x00\x00\x00/bin", nil},
		"too short":  {"\x01\x00", nil},
		"no content": {"", nil},
	} {
		got, err := procargsArgs([]byte(tc.raw))
		if tc.want == nil {
			if !errors.Is(err, ErrNotFound) {
				t.Fatalf("%s: %q %v", name, got, err)
			}
			continue
		}
		if err != nil || !slices.Equal(got, tc.want) {
			t.Fatalf("%s: %q %v", name, got, err)
		}
	}
}

// The kernel refuses another user's arguments, as launchd's are.
func TestArgsOSRefusesAnotherUsersProcess(t *testing.T) {
	if _, err := argsOS(1); err == nil {
		t.Fatal("read launchd's arguments")
	}
}

type closer func() error

func (c closer) Close() error { return c() }

// macOS gives no cheap view of another process's input, so the check is whether this process has a
// controlling terminal, which a hook shares with the client that started it. The answer matches
// an independent open of /dev/tty, and a terminal that opens or not decides it.
func TestReadsTerminalOSOpensTheControllingTerminal(t *testing.T) {
	f, err := os.OpenFile("/dev/tty", os.O_RDONLY|syscall.O_NOCTTY, 0)
	if err == nil {
		_ = f.Close()
	}
	if got, readErr := readsTerminalOS(os.Getpid()); readErr != nil || got != (err == nil) {
		t.Fatalf("this process: %v %v, /dev/tty %v", got, readErr, err)
	}
	defer func(open func() (io.Closer, error)) { openTerminal = open }(openTerminal)
	openTerminal = func() (io.Closer, error) { return nil, syscall.ENXIO }
	if got, err := readsTerminalOS(0); err != nil || got {
		t.Fatalf("no terminal: %v %v", got, err)
	}
	closed := false
	openTerminal = func() (io.Closer, error) { return closer(func() error { closed = true; return nil }), nil }
	if got, err := readsTerminalOS(0); err != nil || !got || !closed {
		t.Fatalf("a terminal: %v %v, closed %v", got, err, closed)
	}
}
