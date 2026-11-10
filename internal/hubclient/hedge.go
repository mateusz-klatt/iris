package hubclient

import (
	"context"
	"io"
	"net"
	"net/http"
	"time"
)

// hedgeDelay is when a hook stops waiting for its first request alone. It is well
// above a normal round trip, so a healthy path never pays for a second one.
const hedgeDelay = 300 * time.Millisecond

// reconcilePath and scopedPath are not repeated. Each compares a snapshot with the last one and
// stores it, so a second copy finds nothing changed: if it answered first, the hook would lose the
// first copy's warning. They have their own short deadline instead.
const (
	reconcilePath = "/v1/shell/reconcile"
	scopedPath    = "/v1/shell/scoped"
)

// HedgedClient is for hooks. A hook is a new process, so every call starts with DNS,
// TCP and TLS, and a lost packet on any of them outlasts the hook's 1.5 s deadline.
// Measured against the public hub, 3.7% of requests took over 1.3 s alone and none did
// with a second request sent at 300 ms. The hub calls a hook makes can all be
// repeated safely, except reconcile in either form. See docs/architecture.md, D3.
func HedgedClient() *http.Client {
	return &http.Client{Transport: hedge{primary: hedgeTransport(), backup: hedgeTransport(), delay: hedgeDelay}}
}

// hedgeTransport has its own resolver, because one Resolver shares a lookup between
// concurrent callers, and the second request must not wait on the same lost packet.
func hedgeTransport() *http.Transport {
	dialer := &net.Dialer{Resolver: &net.Resolver{}}
	t := newTransport()
	t.DialContext = dialer.DialContext
	return t
}

type hedge struct {
	primary, backup http.RoundTripper
	delay           time.Duration
}

type hedgeResult struct {
	res *http.Response
	err error
	// index is which attempt this was, so the other can be stopped.
	index int
}

func (h hedge) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Path == reconcilePath || req.URL.Path == scopedPath {
		return h.primary.RoundTrip(req)
	}
	if req.Body != nil {
		// Each attempt reads its own copy from GetBody.
		_ = req.Body.Close()
	}
	results := make(chan hedgeResult, 2)
	var cancels []context.CancelFunc
	start := func(rt http.RoundTripper) {
		ctx, cancel := context.WithCancel(req.Context())
		index := len(cancels)
		cancels = append(cancels, cancel)
		attempt := req.Clone(ctx)
		if req.GetBody != nil {
			attempt.Body, _ = req.GetBody() // a bytes.Reader, which cannot fail
		}
		go func() {
			res, err := rt.RoundTrip(attempt)
			results <- hedgeResult{res, err, index}
		}()
	}
	start(h.primary)
	timer := time.NewTimer(h.delay)
	defer timer.Stop()
	var failure error
	for received := 0; received < len(cancels); {
		select {
		case <-timer.C:
			if len(cancels) == 1 {
				start(h.backup)
			}
		case r := <-results:
			received++
			if r.err == nil {
				return settle(r, cancels, results, len(cancels)-received), nil
			}
			cancels[r.index]()
			failure = r.err
			if len(cancels) == 1 {
				start(h.backup) // no reason to wait out the delay
			}
		}
	}
	return nil, failure
}

// settle stops the other attempt and keeps the winner's context alive until its body
// is closed. A late answer from the loser is discarded.
func settle(winner hedgeResult, cancels []context.CancelFunc, results <-chan hedgeResult, outstanding int) *http.Response {
	for i, cancel := range cancels {
		if i != winner.index {
			cancel()
		}
	}
	if outstanding > 0 {
		go func() {
			if late := <-results; late.res != nil {
				_ = late.res.Body.Close()
			}
		}()
	}
	winner.res.Body = cancelBody{winner.res.Body, cancels[winner.index]}
	return winner.res
}

type cancelBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b cancelBody) Close() error {
	err := b.ReadCloser.Close()
	b.cancel()
	return err
}
