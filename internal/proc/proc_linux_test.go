//go:build linux

package proc

import "testing"

func TestParseStat(t *testing.T) {
	s := "1055815 (python3) R 1055804 1055804 1055804 0 -1 4194304 974 0 0 0 1 0 0 0 20 0 1 0 2375890 24555520 2498 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0"
	ppid, ticks, err := parseStat(s)
	if err != nil || ppid != 1055804 || ticks != 2375890 {
		t.Fatalf("ppid=%d ticks=%d err=%v", ppid, ticks, err)
	}
	// comm itself contains a parenthesis and spaces.
	s = "12 (a) b) S 99 1 1 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 50 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0"
	ppid, ticks, err = parseStat(s)
	if err != nil || ppid != 99 || ticks != 50 {
		t.Fatalf("comm with paren: ppid=%d ticks=%d err=%v", ppid, ticks, err)
	}
}
