// Package watch delivers bounded, content-free attention lines to client monitors.
package watch

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/mateusz-klatt/iris/internal/binding"
	"github.com/mateusz-klatt/iris/internal/hubclient"
)

// maxBackoff caps reconnect delay. Tests shrink it so the cap is observable.
var maxBackoff = 30 * time.Second

type Frame struct {
	Stream string `json:"stream"`
	Cursor int64  `json:"cursor"`
	Kind   string `json:"kind"`
}
type pending struct {
	count int
	next  time.Time
}
type Coalescer struct {
	Window time.Duration
	Output io.Writer
	// Recount asks how much mail is still waiting when a held line comes due, so a
	// line never announces mail fetched during the window. ok is false if unknown.
	Recount func() (n int, ok bool)
	kinds   map[string]*pending
}

func (c *Coalescer) Add(now time.Time, kind string, count int) error {
	if kind != "inbox_changed" && kind != "claim_released" {
		return nil
	}
	if count <= 0 {
		return nil
	}
	if c.kinds == nil {
		c.kinds = map[string]*pending{}
	}
	p := c.kinds[kind]
	if p == nil {
		p = &pending{}
		c.kinds[kind] = p
	}
	p.count += count
	if !now.Before(p.next) {
		return c.emit(now, kind, p)
	}
	return nil
}
func (c *Coalescer) Flush(now time.Time) error {
	for _, kind := range []string{"inbox_changed", "claim_released"} {
		p := c.kinds[kind]
		if p == nil || p.count <= 0 || now.Before(p.next) {
			continue
		}
		if kind == "inbox_changed" && c.Recount != nil {
			// Only ever down: waiting mail can include what an earlier line announced.
			if n, ok := c.Recount(); ok {
				p.count = min(p.count, n)
			}
		}
		if p.count == 0 {
			continue
		}
		if err := c.emit(now, kind, p); err != nil {
			return err
		}
	}
	return nil
}
func (c *Coalescer) emit(now time.Time, kind string, p *pending) error {
	var err error
	if kind == "inbox_changed" {
		noun := "messages"
		if p.count == 1 {
			noun = "message"
		}
		_, err = fmt.Fprintf(c.Output, "Iris: %d new %s. Use the inbox tool.\n", p.count, noun)
	} else {
		noun := "claims"
		if p.count == 1 {
			noun = "claim"
		}
		_, err = fmt.Fprintf(c.Output, "Iris: %d %s released. Use the claims tool.\n", p.count, noun)
	}
	if err != nil {
		return err
	}
	p.count = 0
	window := c.Window
	if window <= 0 {
		window = 30 * time.Second
	}
	p.next = now.Add(window)
	return nil
}

type Runner struct {
	Resolver        *binding.Resolver
	Output          io.Writer
	Window, Backoff time.Duration
}

func (r *Runner) Run(ctx context.Context) error {
	coalesce := &Coalescer{Output: r.Output, Window: r.Window}
	var cursor int64
	stream := ""
	delay := r.Backoff
	if delay <= 0 {
		delay = time.Second
	}
	for ctx.Err() == nil {
		call, cancel := context.WithTimeout(ctx, 5*time.Second)
		b, err := r.Resolver.Resolve(call, "")
		cancel()
		if err == nil {
			err = r.connect(ctx, b, &cursor, &stream, coalesce)
		}
		if binding.IsCode(err, "lost") {
			return nil
		}
		// Keep flushing pending attention even during a network outage.
		wait := time.NewTimer(delay)
		tick := time.NewTicker(100 * time.Millisecond)
	waiting:
		for {
			select {
			case <-ctx.Done():
				wait.Stop()
				tick.Stop()
				return nil
			case <-wait.C:
				break waiting
			case now := <-tick.C:
				if err = coalesce.Flush(now); err != nil {
					tick.Stop()
					wait.Stop()
					return err
				}
			}
		}
		tick.Stop()
		delay *= 2
		if delay > maxBackoff {
			delay = maxBackoff
		}
	}
	return nil
}

// waiting reads how much mail is unfetched. An older hub does not say.
func (r *Runner) waiting(ctx context.Context, b binding.Bound, cursor int64) (int, bool) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var out struct {
		Pending *int `json:"pending"`
	}
	path := "/v1/events?execution_id=" + url.QueryEscape(b.ExecutionID) + "&limit=1&after=" + fmt.Sprint(cursor)
	if r.Resolver.Hub.Do(ctx, "GET", path, nil, &out) != nil || out.Pending == nil {
		return 0, false
	}
	return *out.Pending, true
}

// readFailure hides the error a read reports because ctx ended. That is a normal
// stop, and either side of the select in connect can see it first.
func readFailure(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return nil
	}
	return err
}

func (r *Runner) connect(ctx context.Context, b binding.Bound, cursor *int64, stream *string, c *Coalescer) error {
	h := r.Resolver.Hub
	base := strings.TrimRight(h.Config.HubURL(), "/")
	if err := hubclient.ValidateURL(base); err != nil {
		return err
	}
	token, err := h.Config.Token()
	if err != nil {
		return err
	}
	u, _ := url.Parse(base)
	if u.Scheme == "https" {
		u.Scheme = "wss"
	} else {
		u.Scheme = "ws"
	}
	u.Path = "/v1/watch"
	q := url.Values{"execution_id": {b.ExecutionID}, "after": {fmt.Sprint(*cursor)}, "stream": {*stream}, "consumer_id": {"iris-watch"}}
	u.RawQuery = q.Encode()
	dial, cancel := context.WithTimeout(ctx, 5*time.Second)
	conn, _, err := websocket.Dial(dial, u.String(), &websocket.DialOptions{HTTPClient: h.HTTP, HTTPHeader: http.Header{"Authorization": {"Bearer " + token}}, Subprotocols: []string{"iris.v1"}})
	cancel()
	if err != nil {
		return err
	}
	defer func() { _ = conn.CloseNow() }()
	c.Recount = func() (int, bool) { return r.waiting(ctx, b, *cursor) }
	readCtx, stop := context.WithCancel(ctx)
	defer stop()
	frames := make(chan Frame)
	fail := make(chan error, 1)
	go func() {
		for {
			_, raw, err := conn.Read(readCtx)
			if err != nil {
				fail <- err
				return
			}
			var f Frame
			if err = json.Unmarshal(raw, &f); err != nil {
				fail <- err
				return
			}
			select {
			case frames <- f:
			case <-readCtx.Done():
				return
			}
		}
	}()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-fail:
			return readFailure(ctx, err)
		case now := <-tick.C:
			if err = c.Flush(now); err != nil {
				return err
			}
		case f := <-frames:
			if f.Kind == "reset" {
				var snapshot struct {
					Stream   string `json:"stream"`
					High     int64  `json:"high_water"`
					Pending  *int   `json:"pending"`
					Snapshot struct {
						Unread int `json:"unread"`
					} `json:"snapshot"`
				}
				path := "/v1/events?execution_id=" + url.QueryEscape(b.ExecutionID) + "&after=" + fmt.Sprint(*cursor) + "&stream=" + url.QueryEscape(*stream)
				if err = h.Do(ctx, "GET", path, nil, &snapshot); err != nil {
					return err
				}
				*cursor = snapshot.High
				*stream = snapshot.Stream
				waiting := snapshot.Snapshot.Unread
				if snapshot.Pending != nil { // an older hub does not send it
					waiting = *snapshot.Pending
				}
				if err = c.Add(time.Now(), "inbox_changed", waiting); err != nil {
					return err
				}
				continue
			}
			if *stream == f.Stream && f.Cursor <= *cursor {
				continue
			}
			*stream = f.Stream
			*cursor = f.Cursor
			if err = c.Add(time.Now(), f.Kind, 1); err != nil {
				return err
			}
			// Persist only a cursor consumed here, never a socket writer's high-water mark.
			ackCtx, ackCancel := context.WithTimeout(ctx, 5*time.Second)
			err = h.Do(ackCtx, "GET", "/v1/events?execution_id="+url.QueryEscape(b.ExecutionID)+"&consumer_id=iris-watch&limit=1&after="+fmt.Sprint(*cursor), nil, nil)
			ackCancel()
			if err != nil {
				return err
			}
		}
	}
}
