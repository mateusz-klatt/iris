package watch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/mateusz-klatt/iris/internal/binding"
	"github.com/mateusz-klatt/iris/internal/hubclient"
)

func TestStoppedDiagnosticWaitsThenExplainsTheBindingFailureOnce(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/bind" {
			t.Errorf("a stopped monitor must only bind, got %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":{"code":"ambiguous","message":"multiple sessions match; provide an explicit execution handle"}}`))
	}))
	defer srv.Close()
	clock := time.Unix(1_800_000_000, 0)
	start := clock
	var out bytes.Buffer
	runner := &Runner{
		Resolver: &binding.Resolver{Hub: &hubclient.Client{Config: testConfig{srv.URL}}, Getenv: func(k string) string {
			if k == "IRIS_EXECUTION" {
				return "old"
			}
			return ""
		}},
		Output: &out,
		Now:    func() time.Time { return clock },
	}
	if err := runner.sayLost(); err != nil {
		t.Fatal(err)
	}
	stopped := out.String()
	diagnostic := "Iris: notifications still stopped: multiple sessions match; Iris cannot choose a replacement safely. Restart this session.\n"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	step := 0
	runner.after = func(time.Duration) <-chan time.Time {
		want := stopped
		if step >= 2 {
			want += diagnostic
		}
		if got := out.String(); got != want {
			t.Errorf("retry %d at %v: output %q, want %q", step, clock.Sub(start), got, want)
		}
		switch step {
		case 0:
			clock = start.Add(time.Minute - time.Nanosecond)
		case 1:
			clock = start.Add(time.Minute)
		case 2:
			clock = start.Add(10 * time.Minute)
		default:
			cancel()
		}
		step++
		ready := make(chan time.Time, 1)
		ready <- clock
		return ready
	}
	if err := runner.Run(ctx); !errors.Is(err, ErrLost) {
		t.Fatalf("a stopped monitor ended with %v", err)
	}
	if step != 4 || strings.Count(out.String(), "notifications still stopped:") != 1 {
		t.Fatalf("%d retries, output %q", step, out.String())
	}
}

func TestStoppedDiagnosticsStateOnlyKnownBindingReasons(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"unbound", &hubclient.Error{Code: "unbound", Message: "no positive binding; provide the SessionStart execution handle"}, "Iris cannot positively identify a replacement Execution. Run `iris doctor` to check the hooks, or restart this session."},
		{"handle mismatch", &hubclient.Error{Code: "unbound", Message: "handle does not match this client and host"}, "the execution handle does not match this client and host. Restart this session."},
		{"stale handle", &hubclient.Error{Code: "unbound", Message: "the execution handle names an Execution already found lost"}, "the execution handle names an Execution already found lost; Iris will not reuse it. Restart this session."},
		{"ambiguous", &hubclient.Error{Code: "ambiguous"}, "multiple sessions match; Iris cannot choose a replacement safely. Restart this session."},
		{"lost", &hubclient.Error{Code: "lost"}, "the binding retry still names an unavailable Execution. Restart this session."},
		{"stale result", &hubclient.Error{Code: "lost", Message: "the binding retry returned an Execution already found lost"}, "the binding retry returned an Execution already found lost; Iris will not reuse it. Restart this session."},
		{"token rejected", &hubclient.Error{Code: "unauthorized"}, "the hub rejected this host's token. Run `iris doctor`."},
		{"unknown hub failure", &hubclient.Error{Code: "new_code", Message: "arbitrary\ntext"}, "the reason a replacement binding is unavailable is unknown. Run `iris doctor`."},
		{"transport failure", errors.New("arbitrary\ntext"), "the reason a replacement binding is unavailable is unknown. Run `iris doctor`."},
		{"binding deadline", context.DeadlineExceeded, "the binding retry timed out. Run `iris doctor`."},
		{"no failed bind", nil, "the reason a replacement binding is unavailable is unknown. Run `iris doctor`."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := retryDiagnostic(tc.err); got != tc.want {
				t.Fatalf("diagnostic %q, want %q", got, tc.want)
			}
			// A confirmed loss keeps the concrete retry failure beneath its lost result.
			if tc.err != nil {
				if got := retryDiagnostic(&binding.LostError{Cause: tc.err}); got != tc.want {
					t.Fatalf("wrapped diagnostic %q, want %q", got, tc.want)
				}
			}
		})
	}
}

func TestStoppedDiagnosticResetsOnlyAfterResumingAndFollowsTheNewCursor(t *testing.T) {
	for _, tc := range []struct {
		name   string
		prompt bool
	}{{"after diagnosis", false}, {"before diagnosis", true}} {
		t.Run(tc.name, func(t *testing.T) {
			diagnosticRecovery(t, tc.prompt)
		})
	}
}

func diagnosticRecovery(t *testing.T, prompt bool) {
	t.Helper()
	answers := []string{"lost", "lost", "lost", "ambiguous", "unauthorized", "", "lost", "lost", "unbound", ""}
	ages := []time.Duration{0, 150 * time.Second, 209 * time.Second, 210 * time.Second, 500 * time.Second, 500 * time.Second, 500 * time.Second, 650 * time.Second, 710 * time.Second, 900 * time.Second}
	if prompt {
		ages = []time.Duration{0, 150 * time.Second, 160 * time.Second, 170 * time.Second, 175 * time.Second, 180 * time.Second, 180 * time.Second, 330 * time.Second, 340 * time.Second, 350 * time.Second}
	}
	var binds, watches, checkpoints atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/bind":
			i := int(binds.Add(1)) - 1
			if i >= len(answers) {
				t.Error("retried after the scenario ended")
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			if code := answers[i]; code != "" {
				w.WriteHeader(http.StatusConflict)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": code}})
				return
			}
			ex := "first"
			if i == 9 {
				ex = "second"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"execution_id": ex, "generation": 1})
		case "/v1/watch":
			i := watches.Add(1)
			want := "first"
			if i == 2 {
				want = "second"
			}
			q := r.URL.Query()
			if q.Get("execution_id") != want || q.Get("after") != "0" || q.Get("stream") != "" {
				t.Errorf("stream inherited another Execution's cursor: %v", q)
			}
			conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"iris.v1"}})
			if err != nil {
				t.Error(err)
				return
			}
			defer func() { _ = conn.CloseNow() }()
			_ = conn.Write(r.Context(), websocket.MessageText, []byte(`{"stream":"stream","cursor":8,"kind":"other"}`))
			_ = conn.Close(websocket.StatusGoingAway, "restarting")
		case "/v1/events":
			checkpoints.Add(1)
			if r.URL.Query().Get("after") != "8" {
				t.Errorf("did not save the resumed stream's cursor: %v", r.URL.Query())
			}
			_, _ = w.Write([]byte(`{}`))
		default:
			t.Errorf("unexpected request: %s", r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer srv.Close()
	start := time.Unix(1_800_000_000, 0)
	clock := start
	var out bytes.Buffer
	runner := &Runner{
		Resolver: &binding.Resolver{Hub: &hubclient.Client{Config: testConfig{srv.URL}}, Getenv: func(k string) string {
			if k == "IRIS_EXECUTION" {
				return "initial"
			}
			return ""
		}},
		Output: &out,
		Now:    func() time.Time { return clock },
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	step := 0
	wantDiagnostics := []int{0, 0, 0, 1, 1, 1, 1, 1, 2, 2}
	if prompt {
		wantDiagnostics = make([]int, len(ages))
	}
	runner.after = func(time.Duration) <-chan time.Time {
		if n := strings.Count(out.String(), "notifications still stopped:"); n != wantDiagnostics[step] {
			t.Errorf("retry %d: %d diagnostics, output %q", step, n, out.String())
		}
		step++
		if step == len(ages) {
			cancel()
		} else {
			clock = start.Add(ages[step])
		}
		ready := make(chan time.Time, 1)
		ready <- clock
		return ready
	}
	if err := runner.Run(ctx); err != nil {
		t.Fatalf("a resumed monitor ended with %v", err)
	}
	got := out.String()
	wantLines := 6
	if prompt {
		wantLines = 4
	}
	if strings.Count(got, "notifications stopped:") != 2 || strings.Count(got, "notifications resumed.") != 2 || strings.Count(got, "\n") != wantLines {
		t.Fatalf("expected stop, diagnostic, resume for each period: %q", got)
	}
	if !prompt && (!strings.Contains(got, "multiple sessions match") || !strings.Contains(got, "cannot positively identify")) || strings.Contains(got, "rejected this host's token") {
		t.Fatalf("diagnostics did not use the latest binding result exactly once per period: %q", got)
	}
	if binds.Load() != 10 || watches.Load() != 2 || checkpoints.Load() != 2 {
		t.Fatalf("binds %d, streams %d, saved cursors %d", binds.Load(), watches.Load(), checkpoints.Load())
	}
}

func TestStoppedDiagnosticCannotOutliveItsOutput(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":{"code":"ambiguous"}}`))
	}))
	defer srv.Close()
	clock := time.Unix(1_800_000_000, 0)
	runner := &Runner{
		Resolver: &binding.Resolver{Hub: &hubclient.Client{Config: testConfig{srv.URL}}, Getenv: func(string) string { return "ex" }},
		Output:   &bytes.Buffer{},
		Now:      func() time.Time { return clock },
		after: func(time.Duration) <-chan time.Time {
			t.Fatal("a failed diagnostic must stop without retrying")
			return nil
		},
	}
	if err := runner.sayLost(); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(time.Minute)
	runner.Output = brokenOutput{}
	err := runner.Run(context.Background())
	if !errors.As(err, new(outputError)) || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("a failed diagnostic ended with %v", err)
	}
}
