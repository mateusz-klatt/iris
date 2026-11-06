//go:build linux

package proc

import (
	"encoding/binary"
	"os"
	"strconv"
	"strings"
	"sync"
)

func lookupOS(pid int) (Snapshot, bool) {
	if pid <= 0 {
		return Snapshot{}, false
	}
	status, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/status")
	if err != nil {
		return Snapshot{}, false
	}
	ppid, _ := strconv.Atoi(statusValue(status, "PPid:"))
	name := statusValue(status, "Name:")
	if argv := argv0(pid); argv != "" {
		name = argv
	}
	start, _ := startMS(pid)
	return Snapshot{PID: pid, PPID: ppid, Name: name, StartMS: start}, true
}

func statusValue(status []byte, key string) string {
	for _, line := range strings.Split(string(status), "\n") {
		if strings.HasPrefix(line, key) {
			return strings.TrimSpace(line[len(key):])
		}
	}
	return ""
}

func argv0(pid int) string {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
	if err != nil || len(b) == 0 {
		return ""
	}
	if i := strings.IndexByte(string(b), 0); i >= 0 {
		b = b[:i]
	}
	return string(b)
}

func startMS(pid int) (int64, error) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, err
	}
	_, ticks, err := parseStat(string(b))
	if err != nil {
		return 0, err
	}
	boot, clk, err := clock()
	if err != nil {
		return 0, err
	}
	return (boot*clk + ticks) * 1000 / clk, nil
}

func parseStat(s string) (ppid int, startTicks int64, err error) {
	end := strings.LastIndex(s, ")")
	if end < 0 || end+2 >= len(s) {
		return 0, 0, os.ErrInvalid
	}
	fields := strings.Fields(s[end+2:])
	// Fields after comm: state is 0, ppid is 1, starttime is 19 (proc field 22).
	if len(fields) < 20 {
		return 0, 0, os.ErrInvalid
	}
	ppid, err = strconv.Atoi(fields[1])
	if err != nil {
		return 0, 0, err
	}
	startTicks, err = strconv.ParseInt(fields[19], 10, 64)
	return ppid, startTicks, err
}

var (
	clockOnce sync.Once
	clockBoot int64
	clockTick int64
	clockErr  error
)

func clock() (boot, tick int64, err error) {
	clockOnce.Do(func() {
		b, readErr := os.ReadFile("/proc/stat")
		if readErr != nil {
			clockErr = readErr
			return
		}
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(line, "btime ") {
				clockBoot, clockErr = strconv.ParseInt(strings.TrimSpace(line[len("btime "):]), 10, 64)
				break
			}
		}
		if clockBoot == 0 && clockErr == nil {
			clockErr = os.ErrNotExist
		}
		clockTick = clkTck()
	})
	return clockBoot, clockTick, clockErr
}

// clkTck reads AT_CLKTCK from the auxiliary vector. Linux exposes /proc
// starttime in these ticks. 100 is the userspace ABI default.
func clkTck() int64 {
	const atClkTck = 17
	b, err := os.ReadFile("/proc/self/auxv")
	if err != nil {
		return 100
	}
	word := strconv.IntSize / 8
	for i := 0; i+2*word <= len(b); i += 2 * word {
		key := auxWord(b[i : i+word])
		val := auxWord(b[i+word : i+2*word])
		if key == atClkTck && val > 0 {
			return int64(val)
		}
		if key == 0 {
			break
		}
	}
	return 100
}

func auxWord(b []byte) uint64 {
	if len(b) == 8 {
		return binary.NativeEndian.Uint64(b)
	}
	return uint64(binary.NativeEndian.Uint32(b))
}
