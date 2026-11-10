package hubclient

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type tripFunc func(*http.Request) (*http.Response, error)

func (f tripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func answer(text string) *http.Response {
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(text)), Header: http.Header{}}
}

func stalled(r *http.Request) (*http.Response, error) {
	<-r.Context().Done()
	return nil, r.Context().Err()
}

func hedged(t *testing.T, primary, backup tripFunc, path string) (*http.Response, error) {
	t.Helper()
	c := &http.Client{Transport: hedge{primary: primary, backup: backup, delay: 10 * time.Millisecond}}
	req, err := http.NewRequestWithContext(context.Background(), "POST", "http://hub"+path, bytes.NewReader([]byte("payload")))
	if err != nil {
		t.Fatal(err)
	}
	return c.Do(req)
}

func read(t *testing.T, res *http.Response) string {
	t.Helper()
	defer func() { _ = res.Body.Close() }()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestAFastAnswerSendsOneRequest(t *testing.T) {
	var backups atomic.Int32
	res, err := hedged(t,
		func(*http.Request) (*http.Response, error) { return answer("first"), nil },
		func(*http.Request) (*http.Response, error) { backups.Add(1); return answer("second"), nil },
		"/v1/sessions")
	if err != nil {
		t.Fatal(err)
	}
	if got := read(t, res); got != "first" {
		t.Fatal(got)
	}
	time.Sleep(30 * time.Millisecond)
	if backups.Load() != 0 {
		t.Fatal("a healthy path paid for a second request")
	}
}

func TestAStalledRequestIsOvertakenWithItsBody(t *testing.T) {
	stopped := make(chan struct{})
	res, err := hedged(t,
		func(r *http.Request) (*http.Response, error) {
			<-r.Context().Done()
			close(stopped)
			return nil, r.Context().Err()
		},
		func(r *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(r.Body)
			return answer("second:" + string(raw)), nil
		},
		"/v1/claims/touch")
	if err != nil {
		t.Fatal(err)
	}
	if got := read(t, res); got != "second:payload" {
		t.Fatal(got)
	}
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("the stalled attempt was left running")
	}
}

func TestAWinnerKeepsItsContextUntilItsBodyIsClosed(t *testing.T) {
	var ctx context.Context
	res, err := hedged(t,
		func(r *http.Request) (*http.Response, error) { ctx = r.Context(); return answer("x"), nil },
		stalled, "/v1/sessions")
	if err != nil {
		t.Fatal(err)
	}
	if ctx.Err() != nil {
		t.Fatal("cancelled before the body was read")
	}
	_ = read(t, res)
	if ctx.Err() == nil {
		t.Fatal("the context outlived the body")
	}
}

func TestAFastFailureStartsTheBackupAtOnce(t *testing.T) {
	c := &http.Client{Transport: hedge{
		primary: tripFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("refused") }),
		backup:  tripFunc(func(*http.Request) (*http.Response, error) { return answer("second"), nil }),
		delay:   time.Hour,
	}}
	res, err := c.Post("http://hub/v1/sessions", "application/json", strings.NewReader("x"))
	if err != nil {
		t.Fatal(err)
	}
	if got := read(t, res); got != "second" {
		t.Fatal(got)
	}
}

func TestBothFailingReportsAnError(t *testing.T) {
	refused := func(*http.Request) (*http.Response, error) { return nil, errors.New("refused") }
	if _, err := hedged(t, refused, refused, "/v1/sessions"); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatal(err)
	}
}

func TestALateLoserIsClosed(t *testing.T) {
	closed := make(chan struct{})
	release := make(chan struct{})
	late := answer("late")
	late.Body = closeNotice{io.NopCloser(strings.NewReader("")), closed}
	res, err := hedged(t,
		func(*http.Request) (*http.Response, error) { <-release; return late, nil },
		func(*http.Request) (*http.Response, error) { return answer("second"), nil },
		"/v1/sessions")
	if err != nil {
		t.Fatal(err)
	}
	_ = read(t, res)
	close(release)
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("the late response leaked")
	}
}

type closeNotice struct {
	io.ReadCloser
	closed chan struct{}
}

func (c closeNotice) Close() error { close(c.closed); return nil }

// No reconcile is sent twice, full or reduced, in either form, whatever path the hub URL puts
// before the routes: a second copy of a snapshot would find nothing changed, and answering first
// it would cost the hook the first copy's warning. That holds for a slow first answer, and for a
// failure after the request was sent, when the hub may have stored the first copy already.
func TestReconcileIsNeverRepeated(t *testing.T) {
	calls := map[string]func(*Client) error{
		"reconcile": func(c *Client) error {
			_, err := c.Reconcile(context.Background(), ReconcileRequest{})
			return err
		},
		"one call": func(c *Client) error {
			_, err := c.ReconcileSession(context.Background(), ShellSessionRequest{})
			return err
		},
		"scoped": func(c *Client) error {
			_, err := c.ReconcileScoped(context.Background(), ScopedRequest{})
			return err
		},
		"scoped one call": func(c *Client) error {
			_, err := c.ReconcileScopedSession(context.Background(), ScopedSessionRequest{})
			return err
		},
	}
	lost := errors.New("connection reset after the request was sent")
	for _, base := range []string{"http://127.0.0.1:7878", "https://hub.example/iris"} {
		for name, call := range calls {
			for _, fails := range []bool{false, true} {
				var backups atomic.Int32
				primary := func(*http.Request) (*http.Response, error) {
					if fails {
						return nil, lost
					}
					time.Sleep(40 * time.Millisecond)
					return answer("{}"), nil
				}
				backup := func(*http.Request) (*http.Response, error) { backups.Add(1); return answer("{}"), nil }
				c := &Client{Config: staticConfig{url: base}, HTTP: &http.Client{Transport: hedge{primary: tripFunc(primary), backup: tripFunc(backup), delay: 10 * time.Millisecond}}}
				if err := call(c); fails && !errors.Is(err, lost) || !fails && err != nil || backups.Load() != 0 {
					t.Fatalf("%s, %s, failing %t: %v with %d copies", base, name, fails, err, backups.Load())
				}
			}
		}
	}
}

func TestARequestWithoutABodyIsRepeatedToo(t *testing.T) {
	c := &http.Client{Transport: hedge{primary: tripFunc(stalled), backup: tripFunc(func(*http.Request) (*http.Response, error) { return answer("second"), nil }), delay: time.Millisecond}}
	res, err := c.Get("http://hub/v1/status")
	if err != nil {
		t.Fatal(err)
	}
	if got := read(t, res); got != "second" {
		t.Fatal(got)
	}
}

func TestHedgedClientReachesARealServer(t *testing.T) {
	var calls atomic.Int32
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			select {
			case <-release:
			case <-r.Context().Done():
			}
			return
		}
		_, _ = w.Write([]byte(`{"execution_id":"ex"}`))
	}))
	defer srv.Close()
	defer close(release)
	c := &Client{Config: hedgeConfig{srv.URL}, HTTP: HedgedClient()}
	got, err := c.RegisterSession(context.Background(), SessionRequest{})
	if err != nil || got.ExecutionID != "ex" {
		t.Fatal(got, err)
	}
}

type hedgeConfig struct{ url string }

func (c hedgeConfig) HubURL() string       { return c.url }
func (hedgeConfig) Token() (string, error) { return "t", nil }
