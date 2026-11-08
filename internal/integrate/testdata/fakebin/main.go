// Command fakebin is a stand-in for iris and the client binaries in tests.
// IRIS_FAKE_STDOUT is written to stdout, IRIS_FAKE_EXIT is the status, and
// IRIS_FAKE_REQUIRE_CLOSED_STDIN=1 fails unless stdin is already at EOF. Like iris hook, it
// answers doctor's canary nonce on stderr, unless IRIS_FAKE_NO_RECEIPT=1 makes it a stand-in
// that never reaches Iris.
package main

import (
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

func main() {
	if want, ok := os.LookupEnv("IRIS_FAKE_STDIN"); ok {
		raw, err := io.ReadAll(os.Stdin)
		if err != nil || string(raw) != want {
			os.Exit(4)
		}
		if receipt := os.Getenv("IRIS_FAKE_STDIN_RECEIPT"); receipt != "" {
			if err := os.WriteFile(receipt, raw, 0600); err != nil {
				os.Exit(5)
			}
		}
	}
	if os.Getenv("IRIS_FAKE_REQUIRE_CLOSED_STDIN") == "1" && !stdinClosed() {
		os.Exit(1)
	}
	if s := os.Getenv("IRIS_FAKE_STDOUT"); s != "" {
		if !strings.HasSuffix(s, "\n") {
			s += "\n"
		}
		_, _ = io.WriteString(os.Stdout, s)
	}
	if nonce := os.Getenv("IRIS_DOCTOR_NONCE"); nonce != "" && os.Getenv("IRIS_FAKE_NO_RECEIPT") != "1" {
		_, _ = io.WriteString(os.Stderr, "iris: canary "+nonce+"\n")
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
