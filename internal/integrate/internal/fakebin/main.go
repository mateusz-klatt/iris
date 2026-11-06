// Command fakebin is a stand-in for iris and the client binaries in tests.
// IRIS_FAKE_STDOUT is written to stdout, IRIS_FAKE_EXIT is the status, and
// IRIS_FAKE_REQUIRE_CLOSED_STDIN=1 fails unless stdin is already at EOF.
package main

import (
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

func main() {
	if os.Getenv("IRIS_FAKE_REQUIRE_CLOSED_STDIN") == "1" && !stdinClosed() {
		os.Exit(1)
	}
	if s := os.Getenv("IRIS_FAKE_STDOUT"); s != "" {
		if !strings.HasSuffix(s, "\n") {
			s += "\n"
		}
		_, _ = io.WriteString(os.Stdout, s)
	}
	code := 0
	if s := os.Getenv("IRIS_FAKE_EXIT"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil {
			os.Exit(2)
		}
		code = n
	}
	os.Exit(code)
}

func stdinClosed() bool {
	got := make(chan int, 1)
	go func() {
		var buf [1]byte
		n, _ := os.Stdin.Read(buf[:])
		got <- n
	}()
	select {
	case n := <-got:
		return n == 0
	case <-time.After(300 * time.Millisecond):
		return false
	}
}
