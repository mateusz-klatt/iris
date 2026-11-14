//go:build linux

package hook

import (
	"errors"
	"os"
	"strconv"
	"strings"
)

func init() { readCursorArgs = readProcArgs }

func readProcArgs(pid int, _ int64) ([]string, error) {
	if pid <= 0 {
		return nil, errors.New("no process")
	}
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
	if err != nil {
		return nil, err
	}
	return argsFromCmdline(b)
}

func argsFromCmdline(b []byte) ([]string, error) {
	b = []byte(strings.TrimRight(string(b), "\x00"))
	if len(b) == 0 {
		return nil, errors.New("no arguments")
	}
	return strings.Split(string(b), "\x00"), nil
}
