package wake

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/binding"
	"github.com/mateusz-klatt/iris/internal/hook"
	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/kimiwake"
	"github.com/mateusz-klatt/iris/internal/proc"
)

const (
	onceKimiPID   = 4242
	onceKimiStart = int64(1_700_000_000_000)
	onceSession   = "session_114df8a5-1263-4744-bd4a-17c39274ffd6"
)

// onceConn is one open stream whose frames the test sends. Closing frames ends it.
type onceConn struct{ frames chan eventFrame }

func (c *onceConn) Read(ctx context.Context) (eventFrame, error) {
	select {
	case f, ok := <-c.frames:
		if !ok {
			return eventFrame{}, io.EOF
		}
		return f, nil
	case <-ctx.Done():
		return eventFrame{}, ctx.Err()
	}
}

func (c *onceConn) Close() {}

// onceHub is a hub whose answers the test sets. It records every call, in order.
type onceFake struct {
	t       *testing.T
	mu      sync.Mutex
	calls   []string
	binds   []error // the answers to the first bind calls; after them binds succeed
	read    func(after int64, stream string) (attentionRead, error)
	conns   []*onceConn
	connErr error
	beat    func() error
	saveErr error
	saved   []int64
	onBind  func()
	onSave  func()
}

// failingWriter is an output nobody can read any more.
type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("closed") }

// takeOver hands the record to another live watcher, as if this one had been taken for dead.
func (ot *onceTest) takeOver() {
	ot.setAlive(9001, 9002, false)
	other := kimiwake.Attempt{PID: 8001, StartMS: 8002, ID: "other"}
	ot.setAlive(other.PID, other.StartMS, true)
	if owned, err := ot.store.Own(ot.key, other, time.Second); err != nil || !owned {
		ot.t.Errorf("take over: %v %v", owned, err)
	}
}

func (h *onceFake) record(call string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls = append(h.calls, call)
}

func (h *onceFake) log() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.calls)
}

func (h *onceFake) kimiBind(ctx context.Context, host, cwd, session string, pid int, startMS int64) (binding.Bound, error) {
	h.record("bind")
	if h.onBind != nil {
		h.onBind()
	}
	if session != onceSession || pid != onceKimiPID || startMS != onceKimiStart {
		h.t.Errorf("bind for %s %d %d", session, pid, startMS)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.binds) > 0 {
		err := h.binds[0]
		h.binds = h.binds[1:]
		if err == errStall {
			<-ctx.Done()
			return binding.Bound{}, ctx.Err()
		}
		return binding.Bound{}, err
	}
	return binding.Bound{ExecutionID: "ex", Generation: 3, SessionID: onceSession}, nil
}

// errStall makes a call block until its deadline.
var errStall = errors.New("stall")

func (h *onceFake) Attention(_ context.Context, target binding.WakeTarget, after int64, stream string) (attentionRead, error) {
	h.record("attention")
	if target.Bound.ExecutionID != "ex" || target.Bound.Generation != 3 {
		h.t.Errorf("attention for %+v", target.Bound)
	}
	h.mu.Lock()
	read := h.read
	h.mu.Unlock()
	return read(after, stream)
}

// set changes the fake's answers while a watcher may be calling it.
func (h *onceFake) set(change func(h *onceFake)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	change(h)
}

func (h *onceFake) Connect(context.Context, binding.WakeTarget, int64, string) (eventConnection, error) {
	h.record("connect")
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.conns) == 0 {
		return nil, h.connErr
	}
	c := h.conns[0]
	h.conns = h.conns[1:]
	return c, nil
}

func (h *onceFake) heartbeat(_ context.Context, target binding.WakeTarget) error {
	h.record("heartbeat")
	if target.Bound.Generation != 3 {
		h.t.Errorf("heartbeat for generation %d", target.Bound.Generation)
	}
	h.mu.Lock()
	beat := h.beat
	h.mu.Unlock()
	if beat != nil {
		return beat()
	}
	return nil
}

func (h *onceFake) checkpoint(_ context.Context, _ binding.WakeTarget, cursor int64) error {
	h.record("checkpoint")
	if h.onSave != nil {
		h.onSave()
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.saveErr != nil {
		return h.saveErr
	}
	h.saved = append(h.saved, cursor)
	return nil
}

// onceOut is the watcher's output, which the watcher writes and the test reads.
type onceOut struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (o *onceOut) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.b.Write(p)
}

func (o *onceOut) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.b.String()
}

// onceTest is a watcher under a Kimi process whose session is mapped, with wake on.
type onceTest struct {
	t     *testing.T
	w     *kimiOnce
	hub   *onceFake
	store *kimiwake.Store
	out   *onceOut
	procs map[[2]int64]bool
	procM *sync.Mutex
	toldM sync.Mutex
	toldV hook.ToldSnapshot
	key   kimiwake.Key
}

// told sets what the hooks told the session.
func (ot *onceTest) told(stream string, cursor int64) {
	ot.toldM.Lock()
	defer ot.toldM.Unlock()
	ot.toldV = hook.ToldSnapshot{Present: true, Identity: hubclient.Identity{Known: true, Pending: true, Stream: stream, Cursor: cursor}}
}

func newOnceTest(t *testing.T) *onceTest {
	t.Helper()
	ot := &onceTest{t: t, out: &onceOut{}, procs: map[[2]int64]bool{}, procM: &sync.Mutex{}}
	alive := func(pid int, start int64) bool {
		ot.procM.Lock()
		defer ot.procM.Unlock()
		return ot.procs[[2]int64{int64(pid), start}]
	}
	ot.store = &kimiwake.Store{Root: kimiwake.Root(t.TempDir()), Alive: alive}
	ot.hub = &onceFake{t: t, read: func(int64, string) (attentionRead, error) {
		return attentionRead{Stream: "s1", HighWater: 10, Attention: hubclient.Attention{Identity: hubclient.Identity{Known: true}}}, nil
	}}
	self := kimiwake.Attempt{PID: 9001, StartMS: 9002, ID: "self"}
	ot.setAlive(onceKimiPID, onceKimiStart, true)
	ot.setAlive(self.PID, self.StartMS, true)
	ot.key = kimiwake.Key{PID: onceKimiPID, StartMS: onceKimiStart, Session: onceSession}
	ot.w = &kimiOnce{
		hub: ot.hub, store: ot.store, alive: alive, self: self, host: "host", cwd: "/repo", out: ot.out, now: time.Now,
		told: func(string) (hook.ToldSnapshot, error) {
			ot.toldM.Lock()
			defer ot.toldM.Unlock()
			return ot.toldV, nil
		},
		nearest: func() (proc.Ancestor, error) {
			return proc.Ancestor{PID: onceKimiPID, StartMS: onceKimiStart, Name: "kimi"}, nil
		},
	}
	if err := ot.store.MapSession(onceKimiPID, onceKimiStart, onceSession, time.Second); err != nil {
		t.Fatal(err)
	}
	ot.wake("on")
	return ot
}

func (ot *onceTest) setAlive(pid int, start int64, alive bool) {
	ot.procM.Lock()
	defer ot.procM.Unlock()
	ot.procs[[2]int64{int64(pid), start}] = alive
}

func (ot *onceTest) wake(text string) {
	ot.t.Helper()
	if err := os.WriteFile(filepath.Join(ot.store.Root, "preference"), []byte(text), 0o600); err != nil {
		ot.t.Fatal(err)
	}
}

func (ot *onceTest) record() kimiwake.Record {
	ot.t.Helper()
	r, _, err := ot.store.Read(ot.key, time.Second)
	if err != nil {
		ot.t.Fatal(err)
	}
	return r
}

// baseline gives the record a checkpoint, as an earlier watcher left it.
func (ot *onceTest) baseline(stream string, cursor int64, change func(*kimiwake.Record)) {
	ot.t.Helper()
	prior := kimiwake.Attempt{PID: 7001, StartMS: 7002, ID: "prior"}
	ot.setAlive(prior.PID, prior.StartMS, true)
	if owned, err := ot.store.Own(ot.key, prior, time.Second); err != nil || !owned {
		ot.t.Fatalf("%v %v", owned, err)
	}
	if owned, err := ot.store.UpdateOwned(ot.key, "prior", time.Second, func(r *kimiwake.Record) {
		r.Checkpoint = kimiwake.Checkpoint{Stream: stream, Cursor: cursor, Initialized: true}
		if change != nil {
			change(r)
		}
		r.Attempt = nil
	}); err != nil || !owned {
		ot.t.Fatalf("%v %v", owned, err)
	}
	ot.setAlive(prior.PID, prior.StartMS, false)
}

// recordPath is the file of the watcher's record.
func (ot *onceTest) recordPath() string {
	if err := os.MkdirAll(filepath.Join(ot.store.Root, "watchers"), 0o700); err != nil {
		ot.t.Fatal(err)
	}
	return filepath.Join(ot.store.Root, "watchers", "4242-1700000000000-"+onceSession+".json")
}

// stream opens one stream the test feeds.
func (ot *onceTest) stream() *onceConn {
	c := &onceConn{frames: make(chan eventFrame)}
	ot.hub.mu.Lock()
	ot.hub.conns = append(ot.hub.conns, c)
	ot.hub.mu.Unlock()
	return c
}

// start runs the watcher and returns its outcome when it ends.
func (ot *onceTest) start(ctx context.Context) <-chan Outcome {
	done := make(chan Outcome, 1)
	go func() { done <- ot.w.run(ctx) }()
	return done
}

func mail(stream string, unread int, cursor, high int64) attentionRead {
	return attentionRead{Stream: stream, HighWater: high, Attention: hubclient.Attention{
		Unread: unread, Identity: hubclient.Identity{Known: true, Pending: unread > 0, Stream: stream, Cursor: cursor},
	}}
}

func claim(stream string, cursor, high int64) attentionRead {
	return attentionRead{Stream: stream, HighWater: high, Attention: hubclient.Attention{
		Available: 1, Identity: hubclient.Identity{Known: true, Pending: true, Stream: stream, Cursor: cursor},
	}}
}

// reads sets the next authoritative reads; the last answer repeats.
func (ot *onceTest) reads(answers ...attentionRead) {
	var mu sync.Mutex
	ot.hub.set(func(h *onceFake) {
		h.read = func(int64, string) (attentionRead, error) {
			mu.Lock()
			defer mu.Unlock()
			a := answers[0]
			if len(answers) > 1 {
				answers = answers[1:]
			}
			return a, nil
		}
	})
}
