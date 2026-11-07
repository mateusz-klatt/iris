package watch

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/mateusz-klatt/iris/internal/binding"
	"github.com/mateusz-klatt/iris/internal/hubclient"
)

type failWriter struct{ err error }

func (f failWriter) Write([]byte) (int, error) { return 0, f.err }

func TestCoalescerSkipsEmptyCountsAndReportsAWriteFailure(t *testing.T) {
	var out bytes.Buffer
	c := &Coalescer{Output: &out}
	now := time.Now()
	if err := c.Add(now, "inbox_changed", 0); err != nil || out.Len() != 0 {
		t.Fatal(err, out.String())
	}
	if err := c.Add(now, "claim_available", 2); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "2 paths you were refused are free now") {
		t.Fatal(out.String())
	}
	if err := c.Flush(now.Add(time.Minute)); err != nil || strings.Count(out.String(), "\n") != 1 {
		t.Fatal(err, out.String())
	}
	broken := &Coalescer{Output: failWriter{err: errors.New("closed")}}
	if err := broken.Add(now, "inbox_changed", 1); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatal(err)
	}
	if err := broken.Flush(now.Add(time.Minute)); err == nil {
		t.Fatal("flush swallowed the write error")
	}
}

type tokenFail struct{}

func (tokenFail) HubURL() string         { return "http://127.0.0.1:9" }
func (tokenFail) Token() (string, error) { return "", errors.New("no token") }

type urlFail struct{}

func (urlFail) HubURL() string         { return "ftp://127.0.0.1" }
func (urlFail) Token() (string, error) { return "t", nil }

func TestConnectRejectsABadHubBeforeDialing(t *testing.T) {
	r := &Runner{Resolver: &binding.Resolver{Hub: &hubclient.Client{Config: urlFail{}}}}
	err := r.connect(context.Background(), binding.Bound{ExecutionID: "ex"}, new(int64), new(string), &Coalescer{Output: &bytes.Buffer{}})
	if err == nil || !strings.Contains(err.Error(), "scheme") {
		t.Fatal(err)
	}
	r.Resolver.Hub.Config = tokenFail{}
	err = r.connect(context.Background(), binding.Bound{ExecutionID: "ex"}, new(int64), new(string), &Coalescer{Output: &bytes.Buffer{}})
	if err == nil || !strings.Contains(err.Error(), "no token") {
		t.Fatal(err)
	}
	r.Resolver.Hub.Config = testConfig{url: "https://127.0.0.1:1"}
	err = r.connect(context.Background(), binding.Bound{ExecutionID: "ex"}, new(int64), new(string), &Coalescer{Output: &bytes.Buffer{}})
	if err == nil {
		t.Fatal("wss dial should fail")
	}
}

func TestConnectAcksACursorAndStopsOnAResetFailure(t *testing.T) {
	var out bytes.Buffer
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/events" {
			if r.URL.Query().Get("limit") == "1" {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			http.Error(w, "no snapshot", http.StatusBadGateway)
			return
		}
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"iris.v1"}})
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		ctx := r.Context()
		_ = conn.Write(ctx, websocket.MessageText, []byte(`{"stream":"s","cursor":4,"kind":"inbox_changed"}`))
		_ = conn.Write(ctx, websocket.MessageText, []byte(`{"stream":"s","cursor":4,"kind":"inbox_changed"}`))
		// A release everyone sees is not a wake; the addressed one is.
		_ = conn.Write(ctx, websocket.MessageText, []byte(`{"stream":"s","cursor":5,"kind":"claim_released"}`))
		_ = conn.Write(ctx, websocket.MessageText, []byte(`{"stream":"s","cursor":6,"kind":"claim_available"}`))
		_ = conn.Write(ctx, websocket.MessageText, []byte(`{"kind":"reset"}`))
		<-ctx.Done()
	}))
	defer srv.Close()
	cursor := int64(0)
	stream := ""
	r := &Runner{Resolver: &binding.Resolver{Hub: &hubclient.Client{Config: testConfig{url: srv.URL}}}}
	err := r.connect(context.Background(), binding.Bound{ExecutionID: "ex/1"}, &cursor, &stream, &Coalescer{Output: &out, Window: time.Hour})
	if err == nil || !strings.Contains(err.Error(), "502") {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "1 new message") || !strings.Contains(out.String(), "a path you were refused is free now") || strings.Contains(out.String(), "released") {
		t.Fatal(out.String())
	}
	if cursor != 6 || stream != "s" {
		t.Fatalf("cursor %d stream %s", cursor, stream)
	}
}

func TestConnectReturnsWhenTheSocketClosesOrTheContextEnds(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"iris.v1"}})
		if err != nil {
			return
		}
		_ = conn.CloseNow()
	}))
	defer srv.Close()
	r := &Runner{Resolver: &binding.Resolver{Hub: &hubclient.Client{Config: testConfig{url: srv.URL}}}}
	if err := r.connect(context.Background(), binding.Bound{ExecutionID: "ex"}, new(int64), new(string), &Coalescer{Output: &bytes.Buffer{}}); err == nil {
		t.Fatal("closed socket")
	}

	hold := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"iris.v1"}})
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		<-r.Context().Done()
	}))
	defer hold.Close()
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	r.Resolver.Hub.Config = testConfig{url: hold.URL}
	if err := r.connect(ctx, binding.Bound{ExecutionID: "ex"}, new(int64), new(string), &Coalescer{Output: &bytes.Buffer{}}); err != nil {
		t.Fatal(err)
	}
}

func TestReadFailureIsQuietOnlyAfterCancellation(t *testing.T) {
	failure := errors.New("cut")
	if got := readFailure(context.Background(), failure); got != failure {
		t.Fatal(got)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := readFailure(ctx, failure); got != nil {
		t.Fatal(got)
	}
}

func TestRunCapsBackoffAndDefaultsAMissingDelay(t *testing.T) {
	orig := maxBackoff
	t.Cleanup(func() { maxBackoff = orig })
	maxBackoff = 5 * time.Millisecond
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	// Each backoff is recorded and ends at once, so the sequence does not depend on how
	// many dials a slow machine fits into a stretch of real time.
	backoffs := func(runner *Runner, n int) []time.Duration {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		var mu sync.Mutex
		var seen []time.Duration
		runner.after = func(d time.Duration) <-chan time.Time {
			mu.Lock()
			seen = append(seen, d)
			if len(seen) == n {
				cancel()
			}
			mu.Unlock()
			now := make(chan time.Time, 1)
			now <- time.Time{}
			return now
		}
		if err := runner.Run(ctx); err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		defer mu.Unlock()
		return seen
	}
	runner := &Runner{
		Resolver: binding.New(&hubclient.Client{Config: testConfig{url: srv.URL}}, "claude"),
		Output:   &bytes.Buffer{},
		Backoff:  4 * time.Millisecond,
	}
	ms := time.Millisecond
	if got := backoffs(runner, 4); !reflect.DeepEqual(got, []time.Duration{4 * ms, 5 * ms, 5 * ms, 5 * ms}) {
		t.Fatalf("backoff was not capped: %v", got)
	}
	runner.Backoff = 0
	if got := backoffs(runner, 1); !reflect.DeepEqual(got, []time.Duration{time.Second}) {
		t.Fatalf("missing delay: %v", got)
	}
	runner.after = nil

	lost := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":{"code":"lost","message":"restart"}}`))
	}))
	defer lost.Close()
	runner.Backoff = 0
	runner.Resolver = binding.New(&hubclient.Client{Config: testConfig{url: lost.URL}}, "claude")
	if err := runner.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestConnectResetSnapshotAndAckFailure(t *testing.T) {
	var out bytes.Buffer
	mode := "snapshot"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/events" {
			if mode == "ack" || r.URL.Query().Get("limit") == "1" && mode != "snapshot" {
				http.Error(w, "no ack", http.StatusBadGateway)
				return
			}
			_, _ = w.Write([]byte(`{"stream":"s","high_water":9,"snapshot":{"unread":2}}`))
			return
		}
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"iris.v1"}})
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		ctx := r.Context()
		body := []byte(`{"kind":"reset"}`)
		switch mode {
		case "bad":
			body = []byte("not-json")
		case "ack":
			body = []byte(`{"stream":"s","cursor":3,"kind":"inbox_changed"}`)
		}
		_ = conn.Write(ctx, websocket.MessageText, body)
		_ = conn.Close(websocket.StatusNormalClosure, "bye")
	}))
	defer srv.Close()
	r := &Runner{Resolver: &binding.Resolver{Hub: &hubclient.Client{Config: testConfig{url: srv.URL}}}}
	cursor, stream := int64(1), "old"
	err := r.connect(context.Background(), binding.Bound{ExecutionID: "ex"}, &cursor, &stream, &Coalescer{Output: &out})
	if err == nil {
		t.Fatal("socket stayed open")
	}
	if cursor != 9 || stream != "s" || !strings.Contains(out.String(), "2 new messages") {
		t.Fatalf("cursor %d stream %s out %s err %v", cursor, stream, out.String(), err)
	}

	mode = "bad"
	err = r.connect(context.Background(), binding.Bound{ExecutionID: "ex"}, new(int64), new(string), &Coalescer{Output: &bytes.Buffer{}})
	if err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Fatal(err)
	}

	mode = "ack"
	err = r.connect(context.Background(), binding.Bound{ExecutionID: "ex"}, new(int64), new(string), &Coalescer{Output: &bytes.Buffer{}, Window: time.Hour})
	if err == nil || !strings.Contains(err.Error(), "502") {
		t.Fatal(err)
	}

	mode = "snapshot"
	err = r.connect(context.Background(), binding.Bound{ExecutionID: "ex"}, new(int64), new(string), &Coalescer{Output: failWriter{err: errors.New("closed")}})
	if err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatal(err)
	}
}

type releaseWriter struct {
	started chan struct{}
	release chan struct{}
	once    bool
}

func (w *releaseWriter) Write(p []byte) (int, error) {
	if !w.once {
		w.once = true
		close(w.started)
		<-w.release
	}
	return len(p), nil
}

func TestConnectStopsAReaderWhenTheContextEndsDuringAWrite(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"iris.v1"}})
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		for i := 0; i < 4; i++ {
			if conn.Write(r.Context(), websocket.MessageText, []byte(`{"stream":"s","cursor":`+string(rune('1'+i))+`,"kind":"inbox_changed"}`)) != nil {
				return
			}
		}
		<-r.Context().Done()
	}))
	defer srv.Close()
	out := &releaseWriter{started: make(chan struct{}), release: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-out.started
		time.Sleep(50 * time.Millisecond)
		cancel()
		close(out.release)
	}()
	r := &Runner{Resolver: &binding.Resolver{Hub: &hubclient.Client{Config: testConfig{url: srv.URL}}}}
	err := r.connect(ctx, binding.Bound{ExecutionID: "ex"}, new(int64), new(string), &Coalescer{Output: out, Window: time.Hour})
	if err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestRunStopsForALostExecutionAndACancelledContext(t *testing.T) {
	lost := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":{"code":"lost","message":"restart"}}`))
	}))
	defer lost.Close()
	runner := &Runner{Resolver: binding.New(&hubclient.Client{Config: testConfig{url: lost.URL}}, "claude"), Output: &bytes.Buffer{}, Backoff: time.Millisecond}
	if err := runner.Run(context.Background()); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	runner.Resolver = binding.New(&hubclient.Client{Config: testConfig{url: "http://127.0.0.1:1"}}, "claude")
	if err := runner.Run(ctx); err != nil {
		t.Fatal(err)
	}

	done, cancelDone := context.WithCancel(context.Background())
	cancelDone()
	if err := runner.Run(done); err != nil {
		t.Fatal(err)
	}
}

func TestRunReturnsAFlushFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/bind" {
			_, _ = w.Write([]byte(`{"execution_id":"ex","generation":1,"repository_id":"r","agent":{"name":"claude-host"}}`))
			return
		}
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"iris.v1"}})
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		_ = conn.Write(r.Context(), websocket.MessageText, []byte(`{"stream":"s","cursor":1,"kind":"inbox_changed"}`))
		<-r.Context().Done()
	}))
	defer srv.Close()
	t.Setenv("IRIS_EXECUTION", "ex")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	runner := &Runner{
		Resolver: binding.New(&hubclient.Client{Config: testConfig{url: srv.URL}}, "claude"),
		Output:   failWriter{err: errors.New("closed")},
		Backoff:  10 * time.Millisecond,
		Window:   time.Hour,
	}
	err := runner.Run(ctx)
	if err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatal(err)
	}
}
