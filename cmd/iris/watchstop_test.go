package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/mateusz-klatt/iris/internal/watch"
)

type brokenWriter struct{}

func (brokenWriter) Write([]byte) (int, error) { return 0, errors.New("closed") }

// A monitor that ends says why, once, on the output the model reads (gap: iris watch exited 0
// with no line during a deploy). A lost generation was already said by Run.
func TestAnEndingMonitorSaysWhyOnce(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		alive  bool
		reason string
	}{
		{"stopped", nil, true, "the monitor was stopped"},
		{"client gone", nil, false, "the client process ended"},
		{"failure", errors.New("dial refused"), true, "dial refused"},
		{"lost", watch.ErrLost, true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			err := stopLine(&out, tc.err, tc.alive)
			if !errors.Is(err, tc.err) && err != tc.err {
				t.Fatalf("err %v, want %v", err, tc.err)
			}
			want := ""
			if tc.reason != "" {
				want = "Iris: notifications stopped: " + tc.reason + ".\n"
			}
			if out.String() != want {
				t.Fatalf("output %q, want %q", out.String(), want)
			}
		})
	}
	if err := stopLine(brokenWriter{}, nil, true); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("a line that cannot be printed must be an error: %v", err)
	}
	if err := stopLine(brokenWriter{}, errors.New("first"), true); err == nil || err.Error() != "first" {
		t.Fatalf("the original failure must win: %v", err)
	}
}
