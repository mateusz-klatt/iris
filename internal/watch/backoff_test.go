package watch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/mateusz-klatt/iris/internal/binding"
	"github.com/mateusz-klatt/iris/internal/hubclient"
)

// streams serves a stream that opens and closes at once, and records each backoff.
func streams(t *testing.T, now func() time.Time, n int) []time.Duration {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/bind":
			_, _ = w.Write([]byte(`{"execution_id":"ex","generation":1}`))
		case "/v1/watch":
			conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"iris.v1"}})
			if err != nil {
				return
			}
			_ = conn.Close(websocket.StatusGoingAway, "restarting")
		default:
			_, _ = w.Write([]byte(`{"events":[],"pending":0}`))
		}
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var mu sync.Mutex
	var seen []time.Duration
	r := &Runner{
		Resolver: &binding.Resolver{Hub: &hubclient.Client{Config: testConfig{url: srv.URL}}, Getenv: func(k string) string {
			if k == "IRIS_EXECUTION" {
				return "ex"
			}
			return ""
		}},
		Output:  &bytes.Buffer{},
		Backoff: 4 * time.Millisecond,
		Now:     now,
		after: func(d time.Duration) <-chan time.Time {
			mu.Lock()
			seen = append(seen, d)
			if len(seen) == n {
				cancel()
			}
			mu.Unlock()
			now := make(chan time.Time, 1)
			now <- time.Time{}
			return now
		},
	}
	if err := r.Run(ctx); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	return seen
}

// A stream that stayed up and then dropped, as at a hub restart, is not a failure to
// connect. Without this the delay only ever doubled, so after a few restarts every later
// drop waited the full cap. On main this saw 4, 8, 16, 32 and 64 ms.
func TestBackoffStartsAgainAfterAStreamThatStayedUp(t *testing.T) {
	var mu sync.Mutex
	clock := time.Unix(1800000000, 0)
	lasting := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		clock = clock.Add(time.Minute) // every reading is a minute later than the last
		return clock
	}
	for i, d := range streams(t, lasting, 5) {
		if d != 4*time.Millisecond {
			t.Fatalf("drop %d waited %v after a stream that stayed up", i+1, d)
		}
	}
}

// A hub that accepts and closes at once still gets a growing backoff, not a retry every base delay.
func TestAStreamThatClosesAtOnceKeepsBackingOff(t *testing.T) {
	still := time.Unix(1800000000, 0)
	got := streams(t, func() time.Time { return still }, 4)
	want := []time.Duration{4 * time.Millisecond, 8 * time.Millisecond, 16 * time.Millisecond, 32 * time.Millisecond}
	if len(got) != len(want) {
		t.Fatal(got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%v, want %v", got, want)
		}
	}
}

// onceWriter takes one line and then fails, like a monitor whose reader went away.
type onceWriter struct {
	mu    sync.Mutex
	lines int
}

func (w *onceWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.lines++
	if w.lines > 1 {
		return 0, errors.New("closed")
	}
	return len(p), nil
}

// heldLine serves two mail events: the first is printed at once and the second is held by
// the coalescing window. drop closes the stream after them, so the held line comes due
// while the monitor waits to reconnect rather than while it is connected.
func heldLine(t *testing.T, drop bool) error {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/bind":
			_, _ = w.Write([]byte(`{"execution_id":"ex","generation":1}`))
		case "/v1/watch":
			conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"iris.v1"}})
			if err != nil {
				return
			}
			defer func() { _ = conn.CloseNow() }()
			for cursor := 1; cursor <= 2; cursor++ {
				_ = conn.Write(r.Context(), websocket.MessageText, []byte(fmt.Sprintf(`{"stream":"s","cursor":%d,"kind":"inbox_changed"}`, cursor)))
			}
			if drop {
				time.Sleep(20 * time.Millisecond)
				_ = conn.Close(websocket.StatusGoingAway, "restarting")
				return
			}
			<-r.Context().Done()
		default:
			_, _ = w.Write([]byte(`{"events":[],"pending":2}`))
		}
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	r := &Runner{
		Resolver: &binding.Resolver{Hub: &hubclient.Client{Config: testConfig{url: srv.URL}}, Getenv: func(k string) string {
			if k == "IRIS_EXECUTION" {
				return "ex"
			}
			return ""
		}},
		Output:  &onceWriter{},
		Window:  150 * time.Millisecond,
		Backoff: time.Second,
	}
	return r.Run(ctx)
}

func TestAHeldLineThatCannotBePrintedStopsTheMonitor(t *testing.T) {
	for name, drop := range map[string]bool{"while connected": false, "while waiting to reconnect": true} {
		if err := heldLine(t, drop); err == nil || !strings.Contains(err.Error(), "closed") {
			t.Fatalf("%s: %v", name, err)
		}
	}
}
