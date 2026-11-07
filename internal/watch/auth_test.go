package watch

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/binding"
	"github.com/mateusz-klatt/iris/internal/hubclient"
)

type badToken struct{ url string }

func (c badToken) HubURL() string         { return c.url }
func (c badToken) Token() (string, error) { return "revoked-or-unknown", nil }

// A rejected token is said once, however many times the monitor retries.
func TestARejectedTokenIsSaidOnce(t *testing.T) {
	h := startHub(t)
	ex, _ := h.session(t, "s")
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	out := &lines{}
	resolver := &binding.Resolver{Hub: &hubclient.Client{Config: badToken{h.url}}, Getenv: func(k string) string {
		if k == "IRIS_EXECUTION" {
			return ex
		}
		return ""
	}}
	if err := (&Runner{Resolver: resolver, Output: out, Backoff: 5 * time.Millisecond}).Run(ctx); err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(out.String(), "rejected this host's token"); got != 1 {
		t.Fatalf("said %d times:\n%s", got, out.String())
	}
}

func TestTheStreamReportsARejectedToken(t *testing.T) {
	h := startHub(t)
	ex, gen := h.session(t, "s")
	r := &Runner{Resolver: &binding.Resolver{Hub: &hubclient.Client{Config: badToken{h.url}}}, Output: &lines{}}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := r.connect(ctx, binding.Bound{ExecutionID: ex, Generation: int(gen)}, new(int64), new(string), &Coalescer{Output: &lines{}})
	if !binding.IsCode(err, "unauthorized") {
		t.Fatalf("a 401 from the stream must read as unauthorized: %v", err)
	}
}

type brokenOutput struct{}

func (brokenOutput) Write([]byte) (int, error) { return 0, errors.New("closed") }

func TestAWarningThatCannotBePrintedStopsTheMonitor(t *testing.T) {
	h := startHub(t)
	ex, _ := h.session(t, "s")
	resolver := &binding.Resolver{Hub: &hubclient.Client{Config: badToken{h.url}}, Getenv: func(k string) string {
		if k == "IRIS_EXECUTION" {
			return ex
		}
		return ""
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := (&Runner{Resolver: resolver, Output: brokenOutput{}, Backoff: 5 * time.Millisecond}).Run(ctx); err == nil {
		t.Fatal("a monitor that cannot print must stop")
	}
}
