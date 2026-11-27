//go:build compat

package hook

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type compatExchange struct {
	method, uri, body string
	status            int
}

// compatTrace records wire bytes while forwarding each request to the current live hub.
// The proxy address stays fixed while the hub process and its ephemeral port change.
type compatTrace struct {
	url      string
	target   atomic.Pointer[url.URL]
	mu       sync.Mutex
	exchange []compatExchange
	changed  chan struct{}
}

func newCompatTrace(t *testing.T, h *liveCompatHub) *compatTrace {
	t.Helper()
	return newCompatTraceWithBindGate(t, h, nil)
}

func newCompatTraceWithBindGate(t *testing.T, h *liveCompatHub, releaseBind <-chan struct{}) *compatTrace {
	t.Helper()
	trace := &compatTrace{changed: make(chan struct{})}
	trace.useHub(t, h)
	base := http.DefaultTransport.(*http.Transport).Clone()
	t.Cleanup(base.CloseIdleConnections)
	proxy := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(trace.target.Load())
			r.Out.Host = "historical.iris.test"
		},
		Transport: compatRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			// At this point the proxy has set the upstream host and address. The URI
			// still has the client's path and query, including the original encoding.
			var raw []byte
			if req.Body != nil {
				var err error
				raw, err = io.ReadAll(req.Body)
				_ = req.Body.Close()
				if err != nil {
					return nil, err
				}
				req.Body = io.NopCloser(bytes.NewReader(raw))
			}
			record := compatExchange{method: req.Method, uri: req.URL.RequestURI(), body: string(raw)}
			res, err := base.RoundTrip(req)
			if err == nil {
				record.status = res.StatusCode
			}
			gated := releaseBind != nil && record.uri == "/v1/bind" && record.status == http.StatusOK
			if gated {
				// A test may stop the upstream process as soon as it sees this trace.
				// Finish reading the bind reply before publishing that signal.
				body, err := io.ReadAll(res.Body)
				_ = res.Body.Close()
				if err != nil {
					return nil, err
				}
				res.Body = io.NopCloser(bytes.NewReader(body))
			}
			trace.mu.Lock()
			trace.exchange = append(trace.exchange, record)
			close(trace.changed)
			trace.changed = make(chan struct{})
			trace.mu.Unlock()
			if gated {
				select {
				case <-releaseBind:
				case <-req.Context().Done():
				}
			}
			return res, err
		}),
	}
	server := httptest.NewServer(proxy)
	t.Cleanup(server.Close)
	trace.url = server.URL
	return trace
}

func (p *compatTrace) useHub(t *testing.T, h *liveCompatHub) {
	t.Helper()
	u, err := url.Parse(h.baseURL)
	if err != nil {
		t.Fatal(err)
	}
	p.target.Store(u)
}

func (p *compatTrace) snapshot() []compatExchange {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.exchange)
}

func (p *compatTrace) wait(t *testing.T, accept func([]compatExchange) bool) []compatExchange {
	t.Helper()
	timeout := time.NewTimer(40 * time.Second)
	defer timeout.Stop()
	for {
		p.mu.Lock()
		trace, changed := slices.Clone(p.exchange), p.changed
		p.mu.Unlock()
		if accept(trace) {
			return trace
		}
		select {
		case <-changed:
		case <-timeout.C:
			t.Fatalf("timed out waiting for hub traffic; got %+v", trace)
		case <-t.Context().Done():
			t.Fatalf("waiting for hub traffic: %v", t.Context().Err())
		}
	}
}

func assertCompatNoSucceed(t *testing.T, trace []compatExchange) {
	t.Helper()
	for _, req := range trace {
		if strings.Contains(req.uri, "/succeed") {
			t.Errorf("unexpected succession request: %+v", req)
		}
	}
}
