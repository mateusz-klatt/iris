package hook

import (
	"bytes"
	"context"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/hubclient"
)

func BenchmarkNoNetwork(b *testing.B) {
	payload, err := os.ReadFile("testdata/claude/PreToolUse-read.json")
	if err != nil {
		b.Fatal(err)
	}
	var calls atomic.Int32
	r := Runner{
		ClientName: "claude",
		EventName:  "PreToolUse",
		Getenv:     func(string) string { return "" },
		Hub:        benchHub{&calls},
		Deadline:   time.Second,
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r.Stdin = bytes.NewReader(payload)
		out := r.Run()
		if out.Exit != 0 {
			b.Fatalf("exit %d", out.Exit)
		}
	}
	b.StopTimer()
	if calls.Load() != 0 {
		b.Fatalf("hub calls %d", calls.Load())
	}
}

type benchHub struct{ calls *atomic.Int32 }

func (h benchHub) RegisterSession(context.Context, hubclient.SessionRequest) (hubclient.SessionResponse, error) {
	h.calls.Add(1)
	return hubclient.SessionResponse{}, errBench
}
func (h benchHub) Touch(context.Context, hubclient.TouchRequest) (hubclient.TouchResponse, error) {
	h.calls.Add(1)
	return hubclient.TouchResponse{}, errBench
}
func (h benchHub) EndExecution(context.Context, string, int) error {
	h.calls.Add(1)
	return errBench
}

var errBench = errBenchType{}

type errBenchType struct{}

func (errBenchType) Error() string { return "hub" }
