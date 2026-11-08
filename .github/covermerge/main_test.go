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

func TestRefusals(t *testing.T) {
	for name, profiles := range map[string][]string{
		"empty":     {""},
		"mode":      {"mode: count\n"},
		"line":      {"mode: set\nnot a profile line\n"},
		"different": {"mode: set\na.go:1.1,2.2 1 1\n", "mode: set\na.go:1.1,2.2 2 1\n"},
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
}
