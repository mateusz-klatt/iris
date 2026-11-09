package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func merged(t *testing.T, profiles ...string) string {
	t.Helper()
	var in []io.Reader
	for _, p := range profiles {
		in = append(in, strings.NewReader(p))
	}
	var out bytes.Buffer
	if err := merge(in, &out); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

func TestABlockCoveredAnywhereCounts(t *testing.T) {
	linux := "mode: set\na.go:1.1,2.2 1 0\nb_linux.go:1.1,2.2 2 1\n"
	windows := "mode: set\r\na.go:1.1,2.2 1 1\r\nb_windows.go:1.1,2.2 3 0\r\n"
	want := "mode: set\na.go:1.1,2.2 1 1\nb_linux.go:1.1,2.2 2 1\nb_windows.go:1.1,2.2 3 0\n"
	if got := merged(t, linux, windows); got != want {
		t.Fatalf("%q", got)
	}
}

func TestAnUncoveredBlockStaysUncovered(t *testing.T) {
	p := "mode: set\na.go:1.1,2.2 1 0\n"
	if got := merged(t, p, p); got != p {
		t.Fatalf("%q", got)
	}
}

func checked(t *testing.T, profiles ...string) (string, error) {
	t.Helper()
	var in []io.Reader
	for _, p := range profiles {
		in = append(in, strings.NewReader(p))
	}
	var out bytes.Buffer
	err := checkCovered(in, &out)
	return out.String(), err
}

func TestCheckCatchesWhatAPercentageRoundsAway(t *testing.T) {
	report, err := checked(t, "mode: set\na.go:1.1,2.2 10306 1\nb.go:3.3,4.1 1 0\nb.go:5.3,6.1 2 0\n")
	if err == nil || !strings.Contains(err.Error(), "statements not covered: 3") {
		t.Fatalf("10306 of 10309 passed: %v", err)
	}
	want := "b.go:3.3,4.1: 1 not covered\nb.go:5.3,6.1: 2 not covered\n10306 of 10309 statements covered\n"
	if report != want {
		t.Fatalf("%q", report)
	}
}

func TestCheckPassesOnlyWhenEveryStatementIsCovered(t *testing.T) {
	report, err := checked(t, "mode: set\na.go:1.1,2.2 2 1\nempty.go:1.1,1.2 0 0\n")
	if err != nil || report != "2 of 2 statements covered\n" {
		t.Fatalf("%q %v", report, err)
	}
}

func TestCheckCreditsAStatementCoveredOnAnySystem(t *testing.T) {
	linux := "mode: set\na.go:1.1,2.2 1 0\n"
	windows := "mode: set\na.go:1.1,2.2 1 1\n"
	if report, err := checked(t, linux, windows); err != nil || report != "1 of 1 statements covered\n" {
		t.Fatalf("%q %v", report, err)
	}
}

func TestCheckRefusesAProfileThatMeasuredNothing(t *testing.T) {
	if _, err := checked(t, "mode: set\n"); err == nil || !strings.Contains(err.Error(), "no statements") {
		t.Fatal(err)
	}
	if _, err := checked(t, "mode: count\n"); err == nil || !strings.Contains(err.Error(), "header") {
		t.Fatal(err)
	}
}

func TestRefusals(t *testing.T) {
	for name, profiles := range map[string][]string{
		"empty":     {""},
		"mode":      {"mode: count\n"},
		"line":      {"mode: set\nnot a profile line\n"},
		"different": {"mode: set\na.go:1.1,2.2 1 1\n", "mode: set\na.go:1.1,2.2 2 1\n"},
		// A count the gate cannot read must not be taken for a covered statement.
		"statements": {"mode: set\na.go:1.1,2.2 x 1\n"},
		"negative":   {"mode: set\na.go:1.1,2.2 -1 1\n"},
		"count":      {"mode: set\na.go:1.1,2.2 1 x\n"},
	} {
		t.Run(name, func(t *testing.T) {
			var in []io.Reader
			for _, p := range profiles {
				in = append(in, strings.NewReader(p))
			}
			if err := merge(in, io.Discard); err == nil {
				t.Fatal("no error")
			}
		})
	}
}

type failing struct{}

func (failing) Read([]byte) (int, error)  { return 0, errors.New("read") }
func (failing) Write([]byte) (int, error) { return 0, errors.New("write") }

func TestIOFailuresAreReported(t *testing.T) {
	if err := merge([]io.Reader{io.MultiReader(strings.NewReader("mode: set\n"), failing{})}, io.Discard); err == nil {
		t.Fatal("read")
	}
	if err := merge(nil, failing{}); err == nil {
		t.Fatal("write")
	}
	if err := checkCovered([]io.Reader{io.MultiReader(strings.NewReader("mode: set\n"), failing{})}, io.Discard); err == nil {
		t.Fatal("check read")
	}
	if err := checkCovered(nil, failing{}); err == nil || !strings.Contains(err.Error(), "write") {
		t.Fatal("check write", err)
	}
}

func TestRunReadsFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.out")
	if err := os.WriteFile(path, []byte("mode: set\na.go:1.1,2.2 1 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := run([]string{path}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "a.go:1.1,2.2 1 1") {
		t.Fatal(out.String())
	}
	if err := run(nil, &out); err == nil {
		t.Fatal("usage")
	}
	if err := run([]string{path + "x"}, &out); err == nil {
		t.Fatal("missing file")
	}
	if err := run([]string{"-unknown", path}, &out); err == nil {
		t.Fatal("unknown flag")
	}
	if err := run([]string{"-check"}, &out); err == nil {
		t.Fatal("check without a profile")
	}
	out.Reset()
	if err := run([]string{"-check", path}, &out); err != nil || out.String() != "1 of 1 statements covered\n" {
		t.Fatalf("%q %v", out.String(), err)
	}
}
