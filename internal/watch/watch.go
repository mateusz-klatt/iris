// Package watch delivers bounded, content-free attention lines to client monitors.
package watch

import (
	"context"
	"encoding/json"
	"errors"
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

// ErrLost ends Run once a heartbeat confirms that this session's generation is gone at the
// hub. Run has already said so on its output.
var ErrLost = errors.New("this session's generation is gone at the hub")

// Stopped prints the line a monitor gives when it ends, so a dead monitor is never silent:
// its output reaches the model, which then knows notifications have stopped.
func Stopped(w io.Writer, reason string) error {
	_, err := fmt.Fprintf(w, "Iris: notifications stopped: %s.\n", reason)
	return err
}

// recoveryGrace is how long lost answers may last before the monitor believes them without a
// heartbeat. The hub keeps a recovering session for 120 s after a restart (the recovery_deadline
// set in store.openDatabase; change both together); past that, an
// Execution still answered lost is gone. It matters when the monitor never bound at all, so no
// heartbeat ever runs to confirm it.
const recoveryGrace = 150 * time.Second

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
	// claim_released goes to every agent in the repository, for the console. Only
	// claim_available, addressed to a session that was refused the path, is a wake.
	if kind != "inbox_changed" && kind != "claim_available" {
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
	for _, kind := range []string{"inbox_changed", "claim_available"} {
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

// holding reports whether a counted line is waiting for its window. Until it is
// printed, the cursor must not move past the frames it counts.
func (c *Coalescer) holding() bool {
	for _, p := range c.kinds {
		if p.count > 0 {
			return true
		}
	}
	return false
}

// outputError is a failure to print. Reconnecting cannot fix it, so Run stops on it.
type outputError struct{ error }

func (c *Coalescer) emit(now time.Time, kind string, p *pending) error {
	var err error
	if kind == "inbox_changed" {
		noun := "messages"
		if p.count == 1 {
			noun = "message"
		}
		_, err = fmt.Fprintf(c.Output, "Iris: %d new %s. Use the inbox tool.\n", p.count, noun)
	} else if p.count == 1 {
		_, err = fmt.Fprint(c.Output, "Iris: a path you were refused is free now. Use the claims tool.\n")
	} else {
		_, err = fmt.Fprintf(c.Output, "Iris: %d paths you were refused are free now. Use the claims tool.\n", p.count)
	}
	if err != nil {
		return outputError{err}
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
	// Now reads the clock for the coalescing window. Nil means time.Now.
	Now func() time.Time
	// after waits out a backoff. Tests replace it to see each delay without waiting.
	after func(time.Duration) <-chan time.Time
	// opened is when the current stream opened. A drop after a stream that stayed up for
	// at least the cap starts the backoff again; one that closes at once keeps backing off.
	opened time.Time
	// acked and ackedStream are the cursor last saved at the hub. received, when set,
	// sees each frame once it is counted and any save it allowed is done; tests use it
	// to know what the monitor holds.
	acked       int64
	ackedStream string
	received    func(Frame)
}

func (r *Runner) wait(d time.Duration) <-chan time.Time {
	if r.after != nil {
		return r.after(d)
	}
	return time.After(d)
}

func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Runner) Run(ctx context.Context) error {
	coalesce := &Coalescer{Output: r.Output, Window: r.Window}
	var cursor int64
	stream := ""
	warned := false
	base := r.Backoff
	if base <= 0 {
		base = time.Second
	}
	delay := base
	var lostSince time.Time
	for ctx.Err() == nil {
		call, cancel := context.WithTimeout(ctx, 5*time.Second)
		b, err := r.Resolver.Resolve(call, "")
		cancel()
		r.opened = time.Time{}
		if err == nil {
			err = r.connect(ctx, b, &cursor, &stream, coalesce)
		}
		if errors.As(err, new(outputError)) {
			return err
		}
		if !r.opened.IsZero() && r.now().Sub(r.opened) >= maxBackoff {
			delay = base
		}
		// A lost answer from the stream or a bind can be a session still recovering after a
		// hub restart, so it is retried like any other failure. Only the heartbeat's verdict
		// ends the monitor, and then with a line, never silently.
		if binding.IsCode(err, "lost") {
			now := r.now()
			if lostSince.IsZero() {
				lostSince = now
			}
			if r.Resolver.Lost() || now.Sub(lostSince) >= recoveryGrace {
				return r.lost()
			}
		} else {
			lostSince = time.Time{}
		}
		// Say it once: each retry would cost the model another turn.
		rejected := binding.IsCode(err, "unauthorized")
		if rejected && !warned {
			if _, werr := fmt.Fprintln(r.Output, "Iris: the hub rejected this host's token, so notifications have stopped. Tell the user to run `iris doctor`."); werr != nil {
				return werr
			}
		}
		warned = rejected
		// Keep flushing pending attention even during a network outage.
		wait := r.wait(delay)
		tick := time.NewTicker(100 * time.Millisecond)
	waiting:
		for {
			select {
			case <-ctx.Done():
				tick.Stop()
				return r.ended()
			case <-wait:
				break waiting
			case <-tick.C:
				if err = coalesce.Flush(r.now()); err != nil {
					tick.Stop()
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
	return r.ended()
}

// ended is Run's result when its context ends. The heartbeat cancels it once the generation is
// gone, and that is said; any other cancellation is the caller's, who says why.
func (r *Runner) ended() error {
	if r.Resolver.Lost() {
		return r.lost()
	}
	return nil
}

func (r *Runner) lost() error {
	if err := Stopped(r.Output, "the hub no longer has this session's generation; restart the monitor once the session is back"); err != nil {
		return outputError{err}
	}
	return ErrLost
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
	conn, resp, err := websocket.Dial(dial, u.String(), &websocket.DialOptions{HTTPClient: hubclient.NoRedirects(h.HTTP), HTTPHeader: http.Header{"Authorization": {"Bearer " + token}}, Subprotocols: []string{"iris.v1"}})
	cancel()
	if err != nil && resp != nil && resp.StatusCode == http.StatusUnauthorized {
		return &hubclient.Error{Code: "unauthorized", Message: "the hub rejected the watch stream's token"}
	}
	if err != nil {
		return err
	}
	r.opened = r.now()
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
		case <-tick.C:
			if err = c.Flush(r.now()); err != nil {
				return err
			}
			if err = r.checkpoint(ctx, b, *cursor, *stream, c); err != nil {
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
				if err = c.Add(r.now(), "inbox_changed", waiting); err != nil {
					return err
				}
				if err = r.checkpoint(ctx, b, *cursor, *stream, c); err != nil {
					return err
				}
				continue
			}
			if *stream == f.Stream && f.Cursor <= *cursor {
				continue
			}
			*stream = f.Stream
			*cursor = f.Cursor
			if err = c.Add(r.now(), f.Kind, 1); err != nil {
				return err
			}
			if err = r.checkpoint(ctx, b, *cursor, *stream, c); err != nil {
				return err
			}
			if r.received != nil {
				r.received(f)
			}
		}
	}
}

// checkpoint saves cursor at the hub once nothing counted up to it is still held.
// A line waiting for its window has not been printed, so saving the cursor then would
// let a monitor that dies, or is replaced, skip that attention for good. Saving it
// only after the print means a restart replays the held frames instead. Under steady
// traffic the saved cursor therefore lags, and a restart replays frames the coalescer
// then folds into one line: a repeated wake is the intended price of never losing one.
// The hub keeps the larger of the saved and the sent cursor, and only a cursor this
// monitor consumed is ever sent, never a socket writer's high-water mark.
func (r *Runner) checkpoint(ctx context.Context, b binding.Bound, cursor int64, stream string, c *Coalescer) error {
	if c.holding() || (cursor <= r.acked && stream == r.ackedStream) {
		return nil
	}
	ackCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := r.Resolver.Hub.Do(ackCtx, "GET", "/v1/events?execution_id="+url.QueryEscape(b.ExecutionID)+"&consumer_id=iris-watch&limit=1&after="+fmt.Sprint(cursor), nil, nil); err != nil {
		return err
	}
	r.acked, r.ackedStream = cursor, stream
	return nil
}
