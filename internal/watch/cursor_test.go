package watch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/mateusz-klatt/iris/internal/binding"
	"github.com/mateusz-klatt/iris/internal/hubclient"
)

// The monitor saves its cursor at the hub only once everything counted up to it has been
// printed. A frame held for its coalescing window is not saved: a monitor that died, or
// was replaced, would otherwise resume past attention nobody saw (gap review A5).
func TestHeldFramesAreSavedOnlyAfterTheirLineIsPrinted(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	var mu sync.Mutex
	var saved []int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/events" {
			if q := r.URL.Query(); q.Get("consumer_id") == "iris-watch" {
				n, _ := strconv.ParseInt(q.Get("after"), 10, 64)
				mu.Lock()
				saved = append(saved, n)
				mu.Unlock()
				// The hub has recorded the save but the client does not have its answer yet, which
				// is where a test that watches the hub would stop the client too early.
				time.Sleep(50 * time.Millisecond)
			}
			_, _ = w.Write([]byte(`{"pending":1}`))
			return
		}
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"iris.v1"}})
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		for _, frame := range []string{`{"stream":"s","cursor":3,"kind":"inbox_changed"}`, `{"stream":"s","cursor":4,"kind":"inbox_changed"}`} {
			if conn.Write(r.Context(), websocket.MessageText, []byte(frame)) != nil {
				return
			}
		}
		<-r.Context().Done()
	}))
	defer srv.Close()
	snapshot := func() []int64 { mu.Lock(); defer mu.Unlock(); return slices.Clone(saved) }

	clock := &testClock{at: time.Unix(1_800_000_000, 0)}
	counted := make(chan int64, 2)
	checkpoints := make(chan int64, 2)
	output := &lines{}
	r := &Runner{Resolver: &binding.Resolver{Hub: &hubclient.Client{Config: testConfig{url: srv.URL}}}, Now: clock.Now,
		received: func(f Frame) { counted <- f.Cursor }, checkpointed: func(c int64) { checkpoints <- c }}
	done := make(chan error, 1)
	cursor, stream := int64(0), ""
	go func() {
		done <- r.connect(ctx, binding.Bound{ExecutionID: "ex"}, &cursor, &stream, &Coalescer{Output: output, Window: time.Minute})
	}()
	for _, want := range []int64{3, 4} {
		select {
		case got := <-counted:
			if got != want {
				t.Fatalf("counted %d, want %d", got, want)
			}
		case <-ctx.Done():
			t.Fatal("frames never arrived")
		}
	}
	// Frame 3 was printed at once and saved. Frame 4 is held for the window, so the
	// saved cursor must not have moved to it.
	if got := snapshot(); !slices.Equal(got, []int64{3}) {
		t.Fatalf("saved %v while frame 4 was held, want [3]", got)
	}
	clock.Add(2 * time.Minute)
	// Wait for the client to finish both saves, not for the hub to see them: the hub records a
	// save before the client has its answer, and stopping the client then fails that save
	// (seen once on macOS under the full suite).
	for _, want := range []int64{3, 4} {
		select {
		case got := <-checkpoints:
			if got != want {
				t.Fatalf("checkpointed %d, want %d", got, want)
			}
		case <-ctx.Done():
			t.Fatalf("saved %v after the held line was printed, want [3 4]; output %q", snapshot(), output.String())
		}
	}
	if got := snapshot(); !slices.Equal(got, []int64{3, 4}) {
		t.Fatalf("saved %v, want [3 4]", got)
	}
	if want := "Iris: 1 new message. Use the inbox tool.\nIris: 1 new message. Use the inbox tool.\n"; output.String() != want {
		t.Fatalf("output %q", output.String())
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// failingSaves is a hub whose socket sends frames and whose cursor save fails for the
// cursors in fail. Other event reads answer with a snapshot.
func failingSaves(t *testing.T, frames []string, fail map[string]bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/events" {
			if q := r.URL.Query(); q.Get("consumer_id") == "iris-watch" && fail[q.Get("after")] {
				http.Error(w, "no save", http.StatusBadGateway)
				return
			}
			_, _ = w.Write([]byte(`{"stream":"s","high_water":9,"pending":2}`))
			return
		}
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"iris.v1"}})
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		for _, f := range frames {
			if conn.Write(r.Context(), websocket.MessageText, []byte(f)) != nil {
				return
			}
		}
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	return srv
}

// A save that fails once a held line is printed ends the connection, so the monitor
// reconnects and saves again rather than carrying on with an unsaved cursor.
func TestASaveAfterTheHeldLineFailsEndsTheConnection(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	srv := failingSaves(t, []string{`{"stream":"s","cursor":3,"kind":"inbox_changed"}`, `{"stream":"s","cursor":4,"kind":"inbox_changed"}`}, map[string]bool{"4": true})
	clock := &testClock{at: time.Unix(1_800_000_000, 0)}
	counted := make(chan struct{}, 2)
	r := &Runner{Resolver: &binding.Resolver{Hub: &hubclient.Client{Config: testConfig{url: srv.URL}}}, Now: clock.Now,
		received: func(Frame) { counted <- struct{}{} }}
	done := make(chan error, 1)
	cursor, stream := int64(0), ""
	go func() {
		done <- r.connect(ctx, binding.Bound{ExecutionID: "ex"}, &cursor, &stream, &Coalescer{Output: &lines{}, Window: time.Minute})
	}()
	for range 2 {
		select {
		case <-counted:
		case <-ctx.Done():
			t.Fatal("frames never arrived")
		}
	}
	clock.Add(2 * time.Minute)
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "502") {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("the failed save did not end the connection")
	}
	if r.acked != 3 {
		t.Fatalf("saved cursor %d, want 3: the failed save must not count", r.acked)
	}
}

// After a reset the monitor announces the waiting mail and saves the snapshot's cursor.
// A failed save ends the connection, so the next one is told about the mail again.
func TestASaveAfterAResetFailsEndsTheConnection(t *testing.T) {
	srv := failingSaves(t, []string{`{"kind":"reset"}`}, map[string]bool{"9": true})
	out := &lines{}
	r := &Runner{Resolver: &binding.Resolver{Hub: &hubclient.Client{Config: testConfig{url: srv.URL}}}}
	cursor, stream := int64(1), "old"
	err := r.connect(context.Background(), binding.Bound{ExecutionID: "ex"}, &cursor, &stream, &Coalescer{Output: out})
	if err == nil || !strings.Contains(err.Error(), "502") || !strings.Contains(out.String(), "2 new messages") {
		t.Fatalf("err %v, output %q", err, out.String())
	}
	if r.acked != 0 {
		t.Fatalf("saved cursor %d after a failed save", r.acked)
	}
}
