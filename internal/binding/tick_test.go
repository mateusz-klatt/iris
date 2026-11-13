package binding

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/mateusz-klatt/iris/internal/hub"
	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/proc"
	"github.com/mateusz-klatt/iris/internal/store"
)

// The tests of a shim's tick (ADR 027). Most run in a synctest bubble, where the tick's real
// 20-second interval and the hub's leases pass in virtual time: sleeping 20 s past a tick lets
// exactly that tick run.

// tickHub is an in-memory hub that answers each request as its reply says and keeps one line
// per request: "bind" with the evidence the request names, "beat <execution> <generation>
// <source>", or the path.
type tickHub struct {
	mu    sync.Mutex
	lines []string
	reply func(req *http.Request, body map[string]any) (int, any)
}

func (h *tickHub) client() *hubclient.Client {
	return &hubclient.Client{Config: config{"http://127.0.0.1"}, HTTP: &http.Client{Transport: adoptionTransport(func(req *http.Request) (*http.Response, error) {
		var body map[string]any
		_ = json.NewDecoder(req.Body).Decode(&body)
		h.mu.Lock()
		h.lines = append(h.lines, tickLine(req.URL.Path, body))
		h.mu.Unlock()
		status, out := h.reply(req, body)
		if status == 0 {
			return nil, req.Context().Err()
		}
		w := httptest.NewRecorder()
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(out)
		return w.Result(), nil
	})}}
}

func tickLine(path string, body map[string]any) string {
	if execution, ok := strings.CutSuffix(strings.TrimPrefix(path, "/v1/executions/"), "/heartbeat"); ok {
		return fmt.Sprintf("beat %s %v %v", execution, body["generation"], body["source"])
	}
	if path != "/v1/bind" {
		return path
	}
	line := "bind"
	for _, key := range []string{"execution", "session_id", "client_pid", "client_pid_start_ms", "exclude"} {
		if value, ok := body[key]; ok {
			line += fmt.Sprintf(" %s=%v", key, value)
		}
	}
	return line
}

// expect fails the test unless the hub was asked exactly these, in this order.
func (h *tickHub) expect(t *testing.T, want ...string) {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	if !slices.Equal(h.lines, want) && (len(h.lines) != 0 || len(want) != 0) {
		t.Fatalf("the hub was asked\n  %s\nwant\n  %s", strings.Join(h.lines, "\n  "), strings.Join(want, "\n  "))
	}
}

// snapshot copies what the hub was asked, under its lock: a tick may still be adding to it.
func (h *tickHub) snapshot() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.lines)
}

func (h *tickHub) count(line string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, l := range h.lines {
		if l == line {
			n++
		}
	}
	return n
}

func found(execution string, generation int, session string) (int, any) {
	return http.StatusOK, map[string]any{"execution_id": execution, "generation": generation, "session_id": session}
}

func refused(code string) (int, any) {
	return http.StatusConflict, map[string]any{"error": map[string]any{"code": code, "message": code}}
}

func leaseGranted() (int, any) { return http.StatusOK, map[string]any{"lease_ms": 60000} }

func tickResolver(c *hubclient.Client, client string, env map[string]string, ancestor proc.Ancestor) *Resolver {
	return &Resolver{Hub: c, Client: client, Host: "host", CWD: "/repo",
		Getenv:   func(key string) string { return env[key] },
		Ancestor: func() (proc.Ancestor, error) { return ancestor, nil }}
}

// maintain runs r.Maintain with the 20-second interval cmd/iris uses until the returned
// function stops it, as the transport's end or a cancelled context would. A test that fails
// first stops it in cleanup, so the bubble ends with the failure rather than a deadlock.
func maintain(t *testing.T, r *Resolver, source string) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.Maintain(ctx, source, 20*time.Second, func() bool { return true }, func() { t.Error("a live parent was stopped") })
	}()
	stop := func() { cancel(); <-done }
	t.Cleanup(stop)
	return stop
}

func boundNow(r *Resolver) Bound {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.bound
}

var sessionS = map[string]string{"CLAUDE_CODE_SESSION_ID": "S"}

// A shim that has had no tool call binds its session on its tick, through the session id, and
// renews it in the same tick. It decides nothing: the first call still decides legacy, and
// refreshes the tick's binding rather than looking the session up again.
func TestAShimTickBindsItsSessionAndRenewsIt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := &tickHub{reply: func(req *http.Request, _ map[string]any) (int, any) {
			if req.URL.Path == "/v1/bind" {
				return found("ex_1", 1, "S")
			}
			return leaseGranted()
		}}
		r := tickResolver(h.client(), "claude", sessionS, proc.Ancestor{})
		stop := maintain(t, r, "shim")
		time.Sleep(21 * time.Second)
		stop()
		h.expect(t, "bind session_id=S", "beat ex_1 1 shim")
		if r.bound.ExecutionID != "ex_1" || r.bound.Rung != 2 || r.decided.Load() != nil {
			t.Fatalf("bound %+v, mode %v", r.bound, r.decided.Load())
		}
		b, primary, err := r.ResolveCall(context.Background(), "", "", "")
		if err != nil || b.ExecutionID != "ex_1" || b.Rung != 2 || !primary {
			t.Fatalf("the first call: %+v, primary %v, %v", b, primary, err)
		}
		if m := r.decided.Load(); m == nil || m.mode != legacy {
			t.Fatalf("the first call decided %v", m)
		}
		h.expect(t, "bind session_id=S", "beat ex_1 1 shim", "bind execution=ex_1")
	})
}

// With no override, no session id and no client process to look by, the tick sends nothing, as
// a call without a handle would find nothing.
func TestAShimTickWithoutEvidenceSendsNothing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := &tickHub{reply: func(*http.Request, map[string]any) (int, any) { return leaseGranted() }}
		r := tickResolver(h.client(), "claude", nil, proc.Ancestor{})
		stop := maintain(t, r, "shim")
		time.Sleep(61 * time.Second)
		stop()
		h.expect(t)
		if r.bound.ExecutionID != "" {
			t.Fatalf("bound %+v without evidence", r.bound)
		}
	})
}

// A session id with nothing live behind it, as when a client gives its MCP server another id
// than its hooks, falls through to the client process within the same lookup.
func TestAShimTickFallsThroughFromTheSessionIDToTheClientProcess(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := &tickHub{reply: func(req *http.Request, body map[string]any) (int, any) {
			if req.URL.Path != "/v1/bind" {
				return leaseGranted()
			}
			if body["session_id"] != nil {
				return refused("unbound")
			}
			return found("ex_3", 1, "hook-session")
		}}
		r := tickResolver(h.client(), "claude", sessionS, proc.Ancestor{PID: 42, StartMS: 7})
		stop := maintain(t, r, "shim")
		time.Sleep(21 * time.Second)
		stop()
		h.expect(t, "bind session_id=S", "bind client_pid=42 client_pid_start_ms=7", "beat ex_3 1 shim")
		if r.bound.ExecutionID != "ex_3" || r.bound.Rung != 3 {
			t.Fatalf("bound %+v", r.bound)
		}
	})
}

// A live IRIS_EXECUTION binds only that Execution, even where the session id names another.
// Once found lost it is skipped, and the lookup goes on through the session id, as a call's does.
func TestAShimTickHonoursALiveOverrideAndSkipsALostOne(t *testing.T) {
	env := map[string]string{"IRIS_EXECUTION": "ex_o", "CLAUDE_CODE_SESSION_ID": "S"}
	t.Run("live", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := &tickHub{reply: func(req *http.Request, body map[string]any) (int, any) {
				if req.URL.Path != "/v1/bind" {
					return leaseGranted()
				}
				return found("ex_o", 1, "")
			}}
			r := tickResolver(h.client(), "claude", env, proc.Ancestor{})
			stop := maintain(t, r, "shim")
			time.Sleep(21 * time.Second)
			stop()
			h.expect(t, "bind execution=ex_o", "beat ex_o 1 shim")
			if r.bound.Rung != 1 {
				t.Fatalf("bound %+v", r.bound)
			}
		})
	})
	t.Run("lost", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := &tickHub{reply: func(req *http.Request, body map[string]any) (int, any) {
				if req.URL.Path != "/v1/bind" {
					return leaseGranted()
				}
				return found("ex_s", 2, "S")
			}}
			r := tickResolver(h.client(), "claude", env, proc.Ancestor{})
			r.gone = map[string]bool{"ex_o": true}
			stop := maintain(t, r, "shim")
			time.Sleep(21 * time.Second)
			stop()
			h.expect(t, "bind session_id=S", "beat ex_s 2 shim")
			if r.bound.ExecutionID != "ex_s" || r.bound.Rung != 2 {
				t.Fatalf("bound %+v", r.bound)
			}
		})
	})
}

// A heartbeat that finds the generation lost drops it, and the next ticks look for the
// session's current Execution. One the hub answers with but this process found lost is refused,
// as D9 requires, and nothing is renewed for it. The hook's next generation is bound and
// renewed, and doctor hears of the rebind once.
func TestAShimTickRebindsTheNextGenerationAndNeverALostOne(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var lookups atomic.Int32
		h := &tickHub{reply: func(req *http.Request, _ map[string]any) (int, any) {
			switch req.URL.Path {
			case "/v1/bind":
				if lookups.Add(1) == 1 {
					return found("ex_1", 1, "S")
				}
				return found("ex_2", 2, "S")
			case "/v1/executions/ex_1/heartbeat":
				return refused("lost")
			}
			return leaseGranted()
		}}
		r := tickResolver(h.client(), "claude", sessionS, proc.Ancestor{})
		r.bound = Bound{ExecutionID: "ex_1", Generation: 1, Rung: 2}
		var seen []string
		r.Observe = func(text string) { seen = append(seen, text) }
		stop := maintain(t, r, "shim")
		time.Sleep(21 * time.Second)
		if !r.Lost() || boundNow(r).ExecutionID != "" {
			t.Fatalf("after the lost heartbeat: lost %v, bound %+v", r.Lost(), boundNow(r))
		}
		time.Sleep(20 * time.Second)
		if !r.Lost() || boundNow(r).ExecutionID != "" {
			t.Fatalf("the tick bound an Execution found lost: lost %v, bound %+v", r.Lost(), boundNow(r))
		}
		time.Sleep(40 * time.Second)
		stop()
		h.expect(t, "beat ex_1 1 shim", "bind session_id=S", "bind session_id=S", "beat ex_2 2 shim", "beat ex_2 2 shim")
		if r.Lost() || r.bound.ExecutionID != "ex_2" || !slices.Equal(seen, []string{ObservedRebound}) {
			t.Fatalf("lost %v, bound %+v, observed %q", r.Lost(), r.bound, seen)
		}
	})
}

// The tick only binds. Registering stays with the hooks and the first call's fallback (D6):
// however long the session is not found, past any grace period, the tick sends no registration
// and does not start the first call's grace clock.
func TestAShimTickNeverRegisters(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := &tickHub{reply: func(req *http.Request, _ map[string]any) (int, any) {
			if req.URL.Path != "/v1/bind" {
				t.Errorf("the tick asked for %s", req.URL.Path)
			}
			return refused("unbound")
		}}
		r := tickResolver(h.client(), "claude", sessionS, proc.Ancestor{})
		r.Grace = time.Millisecond
		stop := maintain(t, r, "shim")
		time.Sleep(5*time.Minute + time.Second)
		stop()
		if n := h.count("bind session_id=S"); n != 15 || !r.since.IsZero() || r.own || r.bound.ExecutionID != "" {
			t.Fatalf("%d lookups, grace clock %v, own %v, bound %+v", n, r.since, r.own, r.bound)
		}
	})
}

// The tick leaves to calls what calls decide (ADR 017): every Codex process, whose calls can
// name their session; a process a call put in id mode, whatever its client; and handles, which
// only a call carries.
func TestCodexIDModeAndHandlesAreLeftToCalls(t *testing.T) {
	for _, c := range []struct {
		name, client string
		mode         *callMode
	}{
		{"codex undecided", "codex", nil},
		{"codex legacy", "codex", &callMode{mode: legacy}},
		{"codex root", "codex", &callMode{mode: root, session: "S", thread: "S"}},
		{"claude root", "claude", &callMode{mode: root, session: "S", thread: "S"}},
		{"claude sub-agent", "claude", &callMode{mode: subagent, session: "S", thread: "T"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := &tickHub{reply: func(*http.Request, map[string]any) (int, any) { return found("ex_1", 1, "S") }}
				r := tickResolver(h.client(), c.client, sessionS, proc.Ancestor{PID: 42, StartMS: 7})
				if c.mode != nil {
					r.decided.Store(c.mode)
				}
				stop := maintain(t, r, "shim")
				time.Sleep(61 * time.Second)
				stop()
				h.expect(t)
				if r.bound.ExecutionID != "" {
					t.Fatalf("bound %+v", r.bound)
				}
			})
		})
	}
	// A binding a call made by handle is renewed as it is. After its loss the tick has no
	// handle to look by, and only a call binds by one again.
	t.Run("handle", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := &tickHub{reply: func(req *http.Request, body map[string]any) (int, any) {
				switch req.URL.Path {
				case "/v1/bind":
					return found(body["execution"].(string), 2, "")
				case "/v1/executions/ex_h/heartbeat":
					return refused("lost")
				}
				return leaseGranted()
			}}
			r := tickResolver(h.client(), "claude", nil, proc.Ancestor{})
			r.bound = Bound{ExecutionID: "ex_h", Generation: 1, Rung: 4}
			stop := maintain(t, r, "shim")
			time.Sleep(41 * time.Second)
			b, err := r.Resolve(context.Background(), "ex_h2")
			if err != nil || b.ExecutionID != "ex_h2" || b.Rung != 4 {
				t.Fatalf("a call by handle: %+v %v", b, err)
			}
			time.Sleep(20 * time.Second)
			stop()
			h.expect(t, "beat ex_h 1 shim", "bind execution=ex_h2", "beat ex_h2 2 shim")
		})
	})
}

// The watch's own loop binds afresh, with its backoff and its stopped and resumed notices, so its
// Maintain does not look.
func TestTheWatchLeavesTheLookupToItsLoop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := &tickHub{reply: func(*http.Request, map[string]any) (int, any) { return found("ex_1", 1, "S") }}
		r := tickResolver(h.client(), "claude", sessionS, proc.Ancestor{})
		stop := maintain(t, r, "watch")
		time.Sleep(61 * time.Second)
		stop()
		h.expect(t)
	})
}

// A fallback this process registered, or a handover still in flight, is adopt's: the tick does
// not look besides it.
func TestAFallbackOrAHandoverIsLeftToAdopt(t *testing.T) {
	t.Run("own fallback", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := &tickHub{reply: func(*http.Request, map[string]any) (int, any) { return refused("unbound") }}
			r := tickResolver(h.client(), "claude", sessionS, proc.Ancestor{})
			r.own, r.lost, r.gone = true, true, map[string]bool{"ex_f": true}
			stop := maintain(t, r, "shim")
			time.Sleep(61 * time.Second)
			stop()
			// adopt looks once a tick; the tick adds no second lookup.
			h.expect(t, "bind session_id=S", "bind session_id=S", "bind session_id=S")
		})
	})
	t.Run("pending handover", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := &tickHub{reply: func(req *http.Request, _ map[string]any) (int, any) {
				if strings.HasSuffix(req.URL.Path, "/end") {
					return refused("unavailable")
				}
				return leaseGranted()
			}}
			r := tickResolver(h.client(), "claude", sessionS, proc.Ancestor{})
			r.pending = &handover{from: Bound{ExecutionID: "ex_f", Generation: 1}, to: "ex_d"}
			stop := maintain(t, r, "shim")
			time.Sleep(21 * time.Second)
			stop()
			h.expect(t, "/v1/executions/ex_f/end", "beat ex_f 1 shim")
		})
	})
}

// A lookup that fails, for whatever reason, changes nothing, and the next tick tries again.
func TestALookupThatFailsChangesNothing(t *testing.T) {
	for _, c := range []struct {
		name  string
		reply func(*http.Request) (int, any)
	}{
		{"unbound", func(*http.Request) (int, any) { return refused("unbound") }},
		{"ambiguous", func(*http.Request) (int, any) { return refused("ambiguous") }},
		{"hub failure", func(*http.Request) (int, any) { return http.StatusBadGateway, map[string]any{} }},
		// A stalled lookup ends at its own five seconds, well inside the 60-second lease.
		{"stall", func(req *http.Request) (int, any) {
			<-req.Context().Done()
			return 0, nil
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := &tickHub{reply: func(req *http.Request, _ map[string]any) (int, any) { return c.reply(req) }}
				r := tickResolver(h.client(), "claude", sessionS, proc.Ancestor{})
				stop := maintain(t, r, "shim")
				start := time.Now()
				time.Sleep(46 * time.Second)
				stop()
				h.expect(t, "bind session_id=S", "bind session_id=S")
				if r.bound.ExecutionID != "" || r.Lost() || time.Since(start) != 46*time.Second {
					t.Fatalf("bound %+v, lost %v", r.bound, r.Lost())
				}
			})
		})
	}
}

// A confirmed loss stays confirmed while the lookups find nothing, so a call still answers that
// the session is lost, with its guidance, rather than a bare unbound. Only a bind clears it, and
// its rebind is observed once, outside the binding's lock.
func TestALossStaysConfirmedUntilATickRebinds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var hookRegistered atomic.Bool
		h := &tickHub{reply: func(req *http.Request, _ map[string]any) (int, any) {
			switch req.URL.Path {
			case "/v1/bind":
				if hookRegistered.Load() {
					return found("ex_2", 2, "S")
				}
				return refused("unbound")
			case "/v1/executions/ex_1/heartbeat":
				return refused("lost")
			}
			return leaseGranted()
		}}
		r := tickResolver(h.client(), "claude", sessionS, proc.Ancestor{})
		r.bound = Bound{ExecutionID: "ex_1", Generation: 1, Rung: 2}
		var seen []string
		underLock := false
		r.Observe = func(text string) {
			if r.mu.TryLock() {
				r.mu.Unlock()
			} else {
				underLock = true
			}
			seen = append(seen, text)
		}
		stop := maintain(t, r, "shim")
		time.Sleep(41 * time.Second)
		if !r.Lost() {
			t.Fatal("a failed lookup cleared a confirmed loss")
		}
		_, err := r.Resolve(context.Background(), "")
		var lost *LostError
		if !errors.As(err, &lost) || !IsCode(err, "lost") {
			t.Fatalf("a call after a failed lookup answered %v", err)
		}
		hookRegistered.Store(true)
		time.Sleep(40 * time.Second)
		stop()
		h.expect(t, "beat ex_1 1 shim", "bind session_id=S", "bind session_id=S", "bind session_id=S", "beat ex_2 2 shim", "beat ex_2 2 shim")
		if r.Lost() || r.bound.ExecutionID != "ex_2" || !slices.Equal(seen, []string{ObservedRebound}) || underLock {
			t.Fatalf("lost %v, bound %+v, observed %q, under the lock %v", r.Lost(), r.bound, seen, underLock)
		}
	})
}

// The heartbeat after a lookup decides as any heartbeat does: lost drops the new binding at once,
// and another failure keeps it for the next tick. A rebind whose heartbeat failed is not observed.
func TestAHeartbeatAfterALookupStillDecides(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var lookups atomic.Int32
		h := &tickHub{reply: func(req *http.Request, _ map[string]any) (int, any) {
			switch req.URL.Path {
			case "/v1/bind":
				if lookups.Add(1) == 1 {
					return found("ex_1", 1, "S")
				}
				return found("ex_2", 2, "S")
			case "/v1/executions/ex_1/heartbeat":
				return refused("lost")
			}
			return http.StatusBadGateway, map[string]any{}
		}}
		r := tickResolver(h.client(), "claude", sessionS, proc.Ancestor{})
		var seen []string
		r.Observe = func(text string) { seen = append(seen, text) }
		stop := maintain(t, r, "shim")
		time.Sleep(21 * time.Second)
		if !r.Lost() || boundNow(r).ExecutionID != "" || !r.gone["ex_1"] {
			t.Fatalf("a lost heartbeat after a lookup: lost %v, bound %+v", r.Lost(), boundNow(r))
		}
		time.Sleep(20 * time.Second)
		stop()
		h.expect(t, "bind session_id=S", "beat ex_1 1 shim", "bind session_id=S", "beat ex_2 2 shim")
		if r.Lost() || r.bound.ExecutionID != "ex_2" || len(seen) != 0 {
			t.Fatalf("lost %v, bound %+v, observed %q", r.Lost(), r.bound, seen)
		}
	})
}

// parkedOnLock waits until a goroutine whose stack passes through fn is parked on a mutex, so a
// test knows that one side really waits for the binding's lock while the other holds it. The
// runtime's goroutine dump names that wait reason.
func parkedOnLock(t *testing.T, fn string) {
	t.Helper()
	buf := make([]byte, 1<<20)
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		n := runtime.Stack(buf, true)
		for _, g := range strings.Split(string(buf[:n]), "\n\n") {
			if strings.Contains(g, "[sync.Mutex.Lock") && strings.Contains(g, fn) {
				return
			}
		}
	}
	t.Fatalf("nothing in %s waited for the lock", fn)
}

// asked waits until the hub was asked line at least n times.
func (h *tickHub) asked(t *testing.T, line string, n int) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); h.count(line) < n; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the hub was not asked %q %d times: %q", line, n, h.snapshot())
		}
	}
}

// A call and a tick share the binding's lock, so whichever comes first binds and the other follows
// it: a tick that waited for a call's handle binding looks for nothing, and a call that waited for
// the tick's binding refreshes it. Each case first sees the second side parked on the lock. Real
// time, because a goroutine waiting on a mutex holds a synctest bubble still.
func TestACallAndATickBindOnce(t *testing.T) {
	// ticking runs Maintain with a 1 ms interval until the test ends, failed or not.
	ticking := func(t *testing.T, r *Resolver) {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			defer close(done)
			r.Maintain(ctx, "shim", time.Millisecond, func() bool { return true }, func() { t.Error("a live parent was stopped") })
		}()
		t.Cleanup(func() { cancel(); <-done })
	}
	// gate holds the hub's answer to one request until the test opens it, and opens it in
	// cleanup too, so a failed test leaves no request blocked.
	gate := func(t *testing.T) (entered, release chan struct{}, open func()) {
		entered, release = make(chan struct{}), make(chan struct{})
		var once sync.Once
		open = func() { once.Do(func() { close(release) }) }
		t.Cleanup(open)
		return entered, release, open
	}
	type result struct {
		b       Bound
		primary bool
		err     error
	}
	t.Run("call first, by handle", func(t *testing.T) {
		entered, release, open := gate(t)
		h := &tickHub{reply: func(req *http.Request, body map[string]any) (int, any) {
			switch {
			case req.URL.Path != "/v1/bind":
				return leaseGranted()
			case body["execution"] == "ex_h":
				close(entered)
				<-release
				return found("ex_h", 1, "")
			}
			return refused("unbound")
		}}
		r := tickResolver(h.client(), "claude", sessionS, proc.Ancestor{})
		called := make(chan result, 1)
		go func() {
			b, primary, err := r.ResolveCall(context.Background(), "ex_h", "", "")
			called <- result{b, primary, err}
		}()
		<-entered // The call holds the lock while the hub answers its handle.
		ticking(t, r)
		parkedOnLock(t, "(*Resolver).Maintain")
		open()
		got := <-called
		h.asked(t, "beat ex_h 1 shim", 2)
		bound := boundNow(r)
		if got.err != nil || !got.primary || got.b.ExecutionID != "ex_h" || got.b.Generation != 1 || got.b.Rung != 4 {
			t.Fatalf("the call: %+v, primary %v, %v", got.b, got.primary, got.err)
		}
		if bound.ExecutionID != "ex_h" || bound.Generation != 1 || bound.Rung != 4 {
			t.Fatalf("after the tick the binding is %+v", bound)
		}
		if m := r.decided.Load(); m == nil || m.mode != legacy {
			t.Fatalf("the call decided %v", m)
		}
		// The call's own rung-2 attempt is the only lookup by session: the tick made none.
		if h.count("bind session_id=S") != 1 || h.count("bind execution=ex_h") != 1 {
			t.Fatalf("the hub was asked %q", h.snapshot())
		}
	})
	t.Run("tick first", func(t *testing.T) {
		entered, release, open := gate(t)
		var first atomic.Bool
		h := &tickHub{reply: func(req *http.Request, body map[string]any) (int, any) {
			if req.URL.Path != "/v1/bind" {
				return leaseGranted()
			}
			if body["session_id"] == "S" && first.CompareAndSwap(false, true) {
				close(entered)
				<-release
			}
			return found("ex_1", 1, "S")
		}}
		r := tickResolver(h.client(), "claude", sessionS, proc.Ancestor{})
		ticking(t, r)
		<-entered // The tick holds the lock while the hub answers its lookup.
		called := make(chan result, 1)
		go func() {
			b, primary, err := r.ResolveCall(context.Background(), "", "", "")
			called <- result{b, primary, err}
		}()
		parkedOnLock(t, "(*Resolver).overrideOrDecide")
		open()
		got := <-called
		h.asked(t, "beat ex_1 1 shim", 2)
		bound := boundNow(r)
		if got.err != nil || !got.primary || got.b.ExecutionID != "ex_1" || got.b.Generation != 1 || got.b.Rung != 2 {
			t.Fatalf("the call: %+v, primary %v, %v", got.b, got.primary, got.err)
		}
		if bound.ExecutionID != "ex_1" || bound.Generation != 1 || bound.Rung != 2 {
			t.Fatalf("the binding is %+v", bound)
		}
		if m := r.decided.Load(); m == nil || m.mode != legacy {
			t.Fatalf("the call decided %v", m)
		}
		// The tick looked once; the call refreshed what the tick bound.
		if h.count("bind session_id=S") != 1 || h.count("bind execution=ex_1") != 1 {
			t.Fatalf("the hub was asked %q", h.snapshot())
		}
	})
}

// Kimi's wake follows only a decided legacy process's rung-3 binding (ADR 026). After a loss the
// tick's rebind gives it a target again, for the new Execution only: the old target is refused.
// An undecided Kimi process binds on its tick but stays without a wake target.
func TestATickRebindGivesKimiItsWakeTargetBackForTheNewExecutionOnly(t *testing.T) {
	kimi := proc.Ancestor{PID: 42, StartMS: 7}
	t.Run("decided", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := &tickHub{reply: func(req *http.Request, _ map[string]any) (int, any) {
				switch req.URL.Path {
				case "/v1/bind":
					return found("ex_2", 2, "K")
				case "/v1/executions/ex_1/heartbeat":
					return refused("lost")
				}
				return leaseGranted()
			}}
			r := tickResolver(h.client(), "kimi", nil, kimi)
			r.decided.Store(&callMode{mode: legacy})
			r.bound = Bound{ExecutionID: "ex_1", Generation: 1, Rung: 3, SessionID: "K"}
			old, ok := r.Primary()
			if !ok {
				t.Fatal("the bound Kimi process had no wake target")
			}
			stop := maintain(t, r, "shim")
			time.Sleep(21 * time.Second)
			if _, ok := r.Primary(); ok {
				t.Fatal("a lost binding kept its wake target")
			}
			time.Sleep(20 * time.Second)
			stop()
			now, ok := r.Primary()
			if !ok || now.Bound.ExecutionID != "ex_2" || now.SessionID != "K" {
				t.Fatalf("after the rebind: %+v %v", now, ok)
			}
			started := false
			if admitted, err := r.StartIfPrimary(old, func() error { started = true; return nil }); admitted || err != nil || started {
				t.Fatalf("the old target was admitted: %v %v", admitted, err)
			}
			if admitted, err := r.StartIfPrimary(now, func() error { return nil }); !admitted || err != nil {
				t.Fatalf("the new target was refused: %v %v", admitted, err)
			}
		})
	})
	t.Run("undecided", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := &tickHub{reply: func(req *http.Request, _ map[string]any) (int, any) {
				if req.URL.Path == "/v1/bind" {
					return found("ex_1", 1, "K")
				}
				return leaseGranted()
			}}
			r := tickResolver(h.client(), "kimi", nil, kimi)
			stop := maintain(t, r, "shim")
			time.Sleep(21 * time.Second)
			stop()
			if r.bound.ExecutionID != "ex_1" || r.bound.Rung != 3 {
				t.Fatalf("bound %+v", r.bound)
			}
			if _, ok := r.Primary(); ok {
				t.Fatal("an undecided Kimi process got a wake target")
			}
		})
	})
}

// The parent check comes before the lookup: a process whose parent is gone stops without
// looking, and one that ends before its first tick, as at the end of its stdin, never looks.
func TestAShimLooksOnlyWhileItsSessionCanBeAlive(t *testing.T) {
	t.Run("orphan", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := &tickHub{reply: func(*http.Request, map[string]any) (int, any) { return found("ex_1", 1, "S") }}
			r := tickResolver(h.client(), "claude", sessionS, proc.Ancestor{})
			stopped := make(chan struct{})
			go r.Maintain(context.Background(), "shim", 20*time.Second, func() bool { return false }, func() { close(stopped) })
			<-stopped
			h.expect(t)
		})
	})
	t.Run("ended before its first tick", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := &tickHub{reply: func(*http.Request, map[string]any) (int, any) { return found("ex_1", 1, "S") }}
			r := tickResolver(h.client(), "claude", sessionS, proc.Ancestor{})
			stop := maintain(t, r, "shim")
			time.Sleep(10 * time.Second)
			stop()
			h.expect(t)
		})
	})
}

// liveHub serves the real store through an in-memory transport, so that in a synctest bubble the
// hub's leases run on the bubble's clock.
type liveHub struct {
	file string
	s    *store.Store
	h    atomic.Pointer[hub.Hub]
}

func newLiveHub(t *testing.T) *liveHub {
	t.Helper()
	l := &liveHub{file: filepath.Join(t.TempDir(), "hub.db")}
	l.open(t)
	t.Cleanup(func() { _ = l.s.Close() })
	return l
}

func (l *liveHub) open(t *testing.T) {
	t.Helper()
	s, err := store.Open(l.file, nil)
	if err != nil {
		t.Fatal(err)
	}
	l.s = s
	l.h.Store(hub.New(s, "secret", []string{"127.0.0.1"}))
}

// restart closes the store and opens it again, as a hub restart does: every active Execution
// is then recovering until it proves itself (D9).
func (l *liveHub) restart(t *testing.T) {
	t.Helper()
	if err := l.s.Close(); err != nil {
		t.Fatal(err)
	}
	l.open(t)
}

func (l *liveHub) client() *hubclient.Client {
	return &hubclient.Client{Config: config{"http://127.0.0.1"}, HTTP: &http.Client{Transport: adoptionTransport(func(req *http.Request) (*http.Response, error) {
		w := httptest.NewRecorder()
		l.h.Load().ServeHTTP(w, req)
		return w.Result(), nil
	})}}
}

// hook registers the session as iris hook does (internal/hook/runner.go, register): with its
// remembered generation as proof, and once more without it, naming it as retry_generation, when
// that generation is lost.
func (l *liveHub) hook(t *testing.T, remembered *int64, req store.Request) map[string]any {
	t.Helper()
	req.Generation = *remembered
	out, err := l.s.Call(context.Background(), "sessions", req)
	var lost *store.Error
	if req.Generation != 0 && errors.As(err, &lost) && lost.Code == "lost" {
		req.RetryGeneration, req.Generation = req.Generation, 0
		out, err = l.s.Call(context.Background(), "sessions", req)
	}
	if err != nil {
		t.Fatal(err)
	}
	*remembered = out["generation"].(int64)
	return out
}

var hookS = store.Request{Client: "claude", SessionID: "S", Host: "host", Cwd: "/repo", Source: "hook"}

// The case that cost a Grok session nine generations in four hours (ADR 027): a hook every 30
// minutes, no tool call, and a live shim. Before, the shim renewed nothing, so the hook-only
// 15-minute lease lapsed between hooks and each hook took the next generation. The shim's tick
// now binds the hook's Execution and renews it, so the session keeps one generation.
func TestAPeriodicSessionKeepsOneGenerationOnceItsShimIsAlive(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := newLiveHub(t)
		var remembered int64
		first := l.hook(t, &remembered, hookS)
		if first["lease_ms"] != int64(900000) {
			t.Fatalf("a hook-only session's lease is %v", first["lease_ms"])
		}
		r := tickResolver(l.client(), "claude", sessionS, proc.Ancestor{})
		stop := maintain(t, r, "shim")
		for period := 1; period <= 2; period++ {
			time.Sleep(30 * time.Minute)
			out := l.hook(t, &remembered, hookS)
			if out["execution_id"] != first["execution_id"] || out["generation"] != int64(1) || out["lease_ms"] != int64(60000) {
				t.Fatalf("after period %d the hook got execution %v generation %v lease %v; want %v, 1 and 60000",
					period, out["execution_id"], out["generation"], out["lease_ms"], first["execution_id"])
			}
		}
		stop()
	})
}

// A host that sleeps past the lease costs the session one generation, and only one: the shim's
// heartbeat finds its generation lost, the hook registers the next, and the shim's tick binds that
// one and renews it, never the lost one.
func TestAShimRebindsTheHooksNextGenerationAfterALoss(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := newLiveHub(t)
		var remembered int64
		first := l.hook(t, &remembered, hookS)
		r := tickResolver(l.client(), "claude", sessionS, proc.Ancestor{})
		var seen []string
		r.Observe = func(text string) { seen = append(seen, text) }
		stop := maintain(t, r, "shim")
		time.Sleep(21 * time.Second)
		stop() // The host sleeps: nothing renews the session.
		time.Sleep(2 * time.Minute)
		second := l.hook(t, &remembered, hookS)
		if second["generation"] != int64(2) {
			t.Fatalf("the hook after the sleep got %v", second)
		}
		stop = maintain(t, r, "shim") // The host wakes, and the same process carries on.
		time.Sleep(41 * time.Second)
		stop()
		if r.bound.ExecutionID != second["execution_id"] || r.bound.Generation != 2 || r.Lost() || !r.gone[first["execution_id"].(string)] {
			t.Fatalf("bound %+v, lost %v, gone %v", r.bound, r.Lost(), r.gone)
		}
		if !slices.Equal(seen, []string{ObservedRebound}) {
			t.Fatalf("observed %q", seen)
		}
		if out := l.hook(t, &remembered, hookS); out["execution_id"] != second["execution_id"] || out["lease_ms"] != int64(60000) {
			t.Fatalf("the next hook got %v", out)
		}
	})
}

// After a hub restart every active Execution is recovering until it proves itself (D9). The tick
// looks as a call does, so by override, session id or client process it finds nothing: it binds
// nothing, registers nothing and renews nothing, and the session keeps its claim and its fetched
// mail. A shim that kept its binding reattaches with its generation, as before.
func TestATickCannotFindARecoveringSession(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		l := newLiveHub(t)
		var remembered int64
		hook := hookS
		hook.ClientPID, hook.ClientPIDStartMS = 42, 7
		session := l.hook(t, &remembered, hook)
		ex := session["execution_id"].(string)
		if _, err := l.s.Call(ctx, "claim", store.Request{ExecutionID: ex, Generation: 1, Key: "held", Kind: "exact", Path: "held.go"}); err != nil {
			t.Fatal(err)
		}
		sender, err := l.s.Call(ctx, "sessions", store.Request{Client: "codex", SessionID: "sender", Host: "host", Cwd: "/repo", Source: "hook"})
		if err != nil {
			t.Fatal(err)
		}
		recipient := session["agent"].(map[string]any)["name"].(string)
		if _, err := l.s.Call(ctx, "messages", store.Request{ExecutionID: sender["execution_id"].(string), Generation: sender["generation"].(int64), Key: "m", To: []string{recipient}, Subject: "s", Body: "b"}); err != nil {
			t.Fatal(err)
		}
		fetched, err := l.s.Call(ctx, "inbox", store.Request{ExecutionID: ex, Generation: 1})
		if err != nil {
			t.Fatal(err)
		}
		mail := fetched["deliveries"].([]map[string]any)
		if len(mail) != 1 {
			t.Fatalf("fetched %v", mail)
		}
		keeper := tickResolver(l.client(), "claude", sessionS, proc.Ancestor{})
		keeper.bound = Bound{ExecutionID: ex, Generation: 1, Rung: 2}
		l.restart(t)

		for _, evidence := range []struct {
			name     string
			env      map[string]string
			ancestor proc.Ancestor
		}{
			{"override", map[string]string{"IRIS_EXECUTION": ex}, proc.Ancestor{}},
			{"session id", sessionS, proc.Ancestor{}},
			{"client process", nil, proc.Ancestor{PID: 42, StartMS: 7}},
		} {
			var paths []string
			base := l.client()
			c := &hubclient.Client{Config: base.Config, HTTP: &http.Client{Transport: adoptionTransport(func(req *http.Request) (*http.Response, error) {
				paths = append(paths, req.URL.Path)
				return base.HTTP.Transport.RoundTrip(req)
			})}}
			r := tickResolver(c, "claude", evidence.env, evidence.ancestor)
			stop := maintain(t, r, "shim")
			time.Sleep(21 * time.Second)
			stop()
			if r.bound.ExecutionID != "" || !slices.Equal(paths, []string{"/v1/bind"}) {
				t.Fatalf("%s: bound %+v after asking %q", evidence.name, r.bound, paths)
			}
		}

		stop := maintain(t, keeper, "shim")
		time.Sleep(21 * time.Second)
		stop()
		if keeper.bound.ExecutionID != ex || keeper.Lost() {
			t.Fatalf("the retained binding did not reattach: %+v, lost %v", keeper.bound, keeper.Lost())
		}
		again := l.hook(t, &remembered, hook)
		claims := again["claims"].([]map[string]any)
		if again["execution_id"] != ex || len(claims) != 1 || claims[0]["path"] != "held.go" {
			t.Fatalf("after the restart: execution %v, claims %v", again["execution_id"], claims)
		}
		ack := store.Request{ExecutionID: ex, Generation: 1, Key: mail[0]["id"].(string), Attempt: 1}
		if _, err := l.s.Call(ctx, "ack", ack); err != nil {
			t.Fatalf("mail fetched before the restart could not be acknowledged: %v", err)
		}
	})
}

// A process with session evidence that lives past a tick binds and puts the session into
// heartbeat mode, whatever it was launched for. After it ends, a hook renews the session for 60
// seconds, not 15 minutes: the accepted consequence ADR 027 records, the same as one Iris call
// has always had.
func TestAShortLivedProcessThatBindsLeavesTheSessionOnTheShortLease(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := newLiveHub(t)
		var remembered int64
		if first := l.hook(t, &remembered, hookS); first["lease_ms"] != int64(900000) {
			t.Fatalf("a hook-only session's lease is %v", first["lease_ms"])
		}
		r := tickResolver(l.client(), "claude", sessionS, proc.Ancestor{})
		stop := maintain(t, r, "shim")
		time.Sleep(21 * time.Second)
		stop()
		if out := l.hook(t, &remembered, hookS); out["generation"] != int64(1) || out["lease_ms"] != int64(60000) {
			t.Fatalf("after the process ended the hook got generation %v lease %v", out["generation"], out["lease_ms"])
		}
	})
}
