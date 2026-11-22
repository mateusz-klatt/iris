package binding

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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

type testRealClock struct {
	mu   sync.Mutex
	wall time.Time
	tick int64
}

func newTestRealClock() *testRealClock {
	return &testRealClock{wall: time.Date(2026, 11, 22, 0, 0, 0, 0, time.UTC)}
}

func (c *testRealClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.wall }
func (c *testRealClock) Tick() int64    { c.mu.Lock(); defer c.mu.Unlock(); return c.tick }
func (c *testRealClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.wall = c.wall.Add(d)
	c.tick += d.Milliseconds()
	c.mu.Unlock()
}

type restartableBindingHub struct {
	t           *testing.T
	file        string
	clock       *testRealClock
	store       *store.Store
	srv         *httptest.Server
	serving     atomic.Pointer[hub.Hub]
	mu          sync.Mutex
	sessionReqs []hubclient.SessionRequest
	heartbeats  []string
}

func newRestartableBindingHub(t *testing.T) *restartableBindingHub {
	t.Helper()
	h := &restartableBindingHub{
		t:     t,
		file:  filepath.Join(t.TempDir(), "hub.db"),
		clock: newTestRealClock(),
	}
	h.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/sessions" || r.URL.Path == "/v1/sessions/outside" {
			var copyBody hubclient.SessionRequest
			raw, _ := json.Marshal(r.Body)
			_ = raw
			var buf []byte
			if r.Body != nil {
				var m map[string]any
				_ = json.NewDecoder(r.Body).Decode(&m)
				buf, _ = json.Marshal(m)
				_ = json.Unmarshal(buf, &copyBody)
				r.Body = http.NoBody
				if len(buf) > 0 {
					r.Body = nopReadCloser{strings.NewReader(string(buf))}
				}
			}
			h.mu.Lock()
			h.sessionReqs = append(h.sessionReqs, copyBody)
			h.mu.Unlock()
		}
		if strings.HasSuffix(r.URL.Path, "/heartbeat") {
			h.mu.Lock()
			h.heartbeats = append(h.heartbeats, r.URL.Path)
			h.mu.Unlock()
		}
		h.serving.Load().ServeHTTP(w, r)
	}))
	h.open()
	h.srv.Start()
	t.Cleanup(func() { h.srv.Close(); _ = h.store.Close() })
	return h
}

type nopReadCloser struct{ *strings.Reader }

func (nopReadCloser) Close() error { return nil }

func (h *restartableBindingHub) open() {
	h.t.Helper()
	s, err := store.Open(h.file, h.clock)
	if err != nil {
		h.t.Fatal(err)
	}
	h.store = s
	h.serving.Store(hub.New(s, "secret", []string{h.srv.Listener.Addr().String()}))
}

func (h *restartableBindingHub) restart() {
	h.t.Helper()
	if err := h.store.Close(); err != nil {
		h.t.Fatal(err)
	}
	h.open()
}

func (h *restartableBindingHub) sessions() []hubclient.SessionRequest {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]hubclient.SessionRequest(nil), h.sessionReqs...)
}

func (h *restartableBindingHub) beats() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.heartbeats...)
}

func TestBoundMCPReattachesOnToolCallInsideWindowWithoutMaintain(t *testing.T) {
	ctx := context.Background()
	for _, outside := range []bool{false, true} {
		name := "git"
		if outside {
			name = "outside"
		}
		t.Run(name, func(t *testing.T) {
			h := newRestartableBindingHub(t)
			op := "sessions"
			req := store.Request{Client: "claude", Host: "host", SessionID: "s1", Cwd: "/repo", Worktree: "/repo", RemoteURL: "https://example.com/a/b.git", RootCommit: "root1", Source: "hook"}
			if outside {
				op = "sessions_outside"
				req = store.Request{Client: "claude", Host: "host", SessionID: "s1", Cwd: "/notes", Source: "hook"}
			}
			reg, err := h.store.Call(ctx, op, req)
			if err != nil {
				t.Fatal(err)
			}
			var oldFence int64
			if !outside {
				claimed, err := h.store.Call(ctx, "claim", store.Request{ExecutionID: reg["execution_id"].(string), Generation: 1, Key: "k1", Kind: "exact", Path: "a.go"})
				if err != nil {
					t.Fatal(err)
				}
				oldFence = claimed["fence"].(int64)
			}
			r := &Resolver{
				Hub:    &hubclient.Client{Config: config{h.srv.URL}},
				Client: "claude", Host: "host", CWD: req.Cwd,
				Getenv: func(k string) string {
					if k == "CLAUDE_CODE_SESSION_ID" {
						return "s1"
					}
					return ""
				},
			}
			before, err := r.Resolve(ctx, "")
			if err != nil || before.ExecutionID != reg["execution_id"] || !before.HookSeen || before.Outside != outside {
				t.Fatalf("initial bind: %+v %v", before, err)
			}

			// Restart hub: Execution enters 120 s recovery window. No Maintain tick runs.
			h.restart()
			after, err := r.Resolve(ctx, "")
			if err != nil || after.ExecutionID != before.ExecutionID || after.Generation != 1 || r.Lost() {
				t.Fatalf("bound tool call did not reattach immediately: %+v %v lost=%v", after, err, r.Lost())
			}
			if len(h.beats()) != 1 {
				t.Fatalf("expected 1 immediate heartbeat, got %v", h.beats())
			}
			if !outside {
				claims, err := h.store.Call(ctx, "claims", store.Request{ExecutionID: after.ExecutionID, Generation: 1})
				if err != nil {
					t.Fatal(err)
				}
				list := claims["claims"].([]map[string]any)
				if len(list) != 1 || list[0]["fence"].(int64) <= oldFence {
					t.Fatalf("claim fence was not rotated on reattach: old=%d claims=%+v", oldFence, list)
				}
			}
		})
	}
}

func TestUnboundMCPReattachesFromHookMemoryAndRefusesBadProof(t *testing.T) {
	ctx := context.Background()

	t.Run("legacy env reattach inside window", func(t *testing.T) {
		h := newRestartableBindingHub(t)
		reg, err := h.store.Call(ctx, "sessions", store.Request{Client: "claude", Host: "host", SessionID: "s1", Cwd: "/repo", Worktree: "/repo", Source: "hook"})
		if err != nil {
			t.Fatal(err)
		}
		h.restart()

		r := &Resolver{
			Hub:    &hubclient.Client{Config: config{h.srv.URL}},
			Client: "claude", Host: "host", CWD: "/repo/sub",
			Getenv: func(k string) string {
				if k == "CLAUDE_CODE_SESSION_ID" {
					return "s1"
				}
				return ""
			},
			Recall: func(client, session string) (int, bool, error) {
				if client == "claude" && session == "s1" {
					return 1, true, nil
				}
				return 0, false, nil
			},
		}
		b, err := r.Resolve(ctx, "")
		if err != nil || b.ExecutionID != reg["execution_id"] || b.Generation != 1 || b.Rung != 2 || !b.HookSeen || b.Worktree != "/repo" {
			t.Fatalf("unbound legacy reattach failed: %+v %v", b, err)
		}
		if reqs := h.sessions(); len(reqs) != 1 || reqs[0].Source != "shim" || reqs[0].Generation != 1 {
			t.Fatalf("expected one shim gen-1 reattach request, got %+v", reqs)
		}
	})

	t.Run("codex root reattach inside window", func(t *testing.T) {
		h := newRestartableBindingHub(t)
		reg, err := h.store.Call(ctx, "sessions", store.Request{Client: "codex", Host: "host", SessionID: "c1", Cwd: "/repo", Worktree: "/repo", Source: "hook"})
		if err != nil {
			t.Fatal(err)
		}
		h.restart()

		r := &Resolver{
			Hub:    &hubclient.Client{Config: config{h.srv.URL}},
			Client: "codex", Host: "host", CWD: "/repo",
			Getenv: func(string) string { return "" },
			Recall: func(client, session string) (int, bool, error) {
				if client == "codex" && session == "c1" {
					return 1, true, nil
				}
				return 0, false, nil
			},
		}
		// Sub-agent call never reattaches or calls /v1/sessions.
		sub := &Resolver{
			Hub:    &hubclient.Client{Config: config{h.srv.URL}},
			Client: "codex", Host: "host", CWD: "/repo",
			Getenv: func(string) string { return "" },
			Recall: r.Recall,
		}
		if _, _, err := sub.ResolveCall(ctx, "", "c1", "child"); !isText(err, errUnusable) {
			t.Fatalf("sub-agent should not reattach: %v", err)
		}
		if len(h.sessions()) != 0 {
			t.Fatalf("sub-agent sent registration: %+v", h.sessions())
		}

		// Root call immediately reattaches with gen 1.
		b, primary, err := r.ResolveCall(ctx, "", "c1", "c1")
		if err != nil || !primary || b.ExecutionID != reg["execution_id"] || b.Generation != 1 || b.Rung != 2 {
			t.Fatalf("unbound codex root reattach failed: %+v primary=%v err=%v", b, primary, err)
		}
	})

	t.Run("malformed and mismatched memory never fall back to generation 0", func(t *testing.T) {
		for _, tc := range []struct {
			name     string
			codex    bool
			gen      int
			present  bool
			recallEr error
			wantReqs int
		}{
			{"legacy malformed error", false, 0, true, errors.New("bad"), 0},
			{"legacy zero gen", false, 0, true, nil, 0},
			{"legacy mismatched gen", false, 2, true, nil, 1},
			{"codex malformed error", true, 0, true, errors.New("bad"), 0},
			{"codex mismatched gen", true, 2, true, nil, 1},
			{"codex missing memory", true, 0, false, nil, 0},
		} {
			t.Run(tc.name, func(t *testing.T) {
				h := newRestartableBindingHub(t)
				client := "claude"
				if tc.codex {
					client = "codex"
				}
				if _, err := h.store.Call(ctx, "sessions", store.Request{Client: client, Host: "host", SessionID: "s1", Cwd: "/repo", Worktree: "/repo", Source: "hook"}); err != nil {
					t.Fatal(err)
				}
				h.restart()

				r := &Resolver{
					Hub:    &hubclient.Client{Config: config{h.srv.URL}},
					Client: client, Host: "host", CWD: "/repo",
					Getenv: func(k string) string {
						if !tc.codex && k == "CLAUDE_CODE_SESSION_ID" {
							return "s1"
						}
						return ""
					},
					Recall: func(string, string) (int, bool, error) { return tc.gen, tc.present, tc.recallEr },
				}
				var err error
				if tc.codex {
					_, _, err = r.ResolveCall(ctx, "", "s1", "s1")
					if tc.present && !IsCode(err, "lost") {
						t.Fatalf("expected lost, got %v", err)
					}
					if !tc.present && !isText(err, errUnusable) {
						t.Fatalf("expected errUnusable, got %v", err)
					}
				} else {
					_, err = r.Resolve(ctx, "")
					if !IsCode(err, "lost") {
						t.Fatalf("expected lost, got %v", err)
					}
				}
				reqs := h.sessions()
				if len(reqs) != tc.wantReqs {
					t.Fatalf("got %d /v1/sessions requests (%+v), want %d", len(reqs), reqs, tc.wantReqs)
				}
				for _, req := range reqs {
					if req.Generation <= 0 {
						t.Fatalf("fell back to generation-0 registration: %+v", req)
					}
				}
			})
		}
	})

	t.Run("override and explicit handle do not invoke memory reattach", func(t *testing.T) {
		h := &scriptedHub{binds: []string{"unbound", "unbound"}}
		called := 0
		r := resolverFor(t, h, "claude", map[string]string{"IRIS_EXECUTION": "ex_live", "CLAUDE_CODE_SESSION_ID": "s1"}, proc.Ancestor{})
		r.Recall = func(string, string) (int, bool, error) { called++; return 1, true, nil }
		if _, err := r.Resolve(ctx, ""); err != nil && !IsCode(err, "unbound") {
			t.Fatal(err)
		}
		if called != 0 {
			t.Fatalf("live IRIS_EXECUTION invoked Recall %d times", called)
		}

		r2 := resolverFor(t, &scriptedHub{binds: []string{"unbound", "unbound"}}, "claude", map[string]string{"CLAUDE_CODE_SESSION_ID": "s1"}, proc.Ancestor{})
		r2.Recall = func(string, string) (int, bool, error) { called++; return 1, true, nil }
		if _, err := r2.Resolve(ctx, "ex_explicit"); err != nil && !IsCode(err, "unbound") {
			t.Fatal(err)
		}
		if called != 0 {
			t.Fatalf("explicit non-gone handle invoked Recall %d times", called)
		}

		// With Recall configured but no session env var and no prior binding, Recall is not called.
		r3 := resolverFor(t, &scriptedHub{binds: []string{"unbound"}}, "cursor", nil, proc.Ancestor{})
		r3.Recall = func(string, string) (int, bool, error) { called++; return 1, true, nil }
		if _, err := r3.Resolve(ctx, ""); !IsCode(err, "unbound") || called != 0 {
			t.Fatalf("client without session identity invoked Recall: err=%v called=%d", err, called)
		}
	})

	t.Run("unbound shim tick reattaches via memory without generation 0", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			var sessions int
			h := &tickHub{reply: func(req *http.Request, body map[string]any) (int, any) {
				switch req.URL.Path {
				case "/v1/bind":
					return refused("unbound")
				case "/v1/sessions":
					sessions++
					if body["source"] != "shim" || body["generation"] != float64(3) {
						t.Errorf("unexpected reattach body: %+v", body)
					}
					return http.StatusOK, map[string]any{"execution_id": "ex_3", "generation": 3, "session_id": "S"}
				default:
					return leaseGranted()
				}
			}}
			r := tickResolver(h.client(), "claude", sessionS, proc.Ancestor{})
			r.Recall = func(client, session string) (int, bool, error) {
				if client == "claude" && session == "S" {
					return 3, true, nil
				}
				return 0, false, nil
			}
			stop := maintain(t, r, "shim")
			time.Sleep(21 * time.Second)
			stop()
			if sessions != 1 || r.bound.ExecutionID != "ex_3" || r.bound.Generation != 3 {
				t.Fatalf("tick did not reattach via memory: sessions=%d bound=%+v", sessions, r.bound)
			}
		})
	})
}

func TestPermanentLatchRemovedAndEndedStaysTerminal(t *testing.T) {
	ctx := context.Background()

	t.Run("PID-bound session survives prior loss and later restart via retained session id", func(t *testing.T) {
		h := newRestartableBindingHub(t)
		anc := proc.Ancestor{PID: 5000, StartMS: 12345}
		// Hook registers gen 1 for a client without a session env var (cursor).
		ex1, err := h.store.Call(ctx, "sessions", store.Request{
			Client: "cursor", Host: "host", SessionID: "cur-1", Cwd: "/repo", Worktree: "/repo",
			ClientPID: int64(anc.PID), ClientPIDStartMS: anc.StartMS, SingleSession: true, Source: "hook",
		})
		if err != nil {
			t.Fatal(err)
		}
		memGen := 1
		r := &Resolver{
			Hub:    &hubclient.Client{Config: config{h.srv.URL}},
			Client: "cursor", Host: "host", CWD: "/repo",
			Getenv:   func(string) string { return "" },
			Ancestor: func() (proc.Ancestor, error) { return anc, nil },
			Recall: func(client, session string) (int, bool, error) {
				if client == "cursor" && session == "cur-1" && memGen > 0 {
					return memGen, true, nil
				}
				return 0, false, nil
			},
		}
		b1, err := r.Resolve(ctx, "")
		if err != nil || b1.ExecutionID != ex1["execution_id"] || !b1.HookSeen || b1.Rung != 3 {
			t.Fatalf("initial PID bind: %+v %v", b1, err)
		}

		// Expire gen 1 past its lease; refresh's heartbeat confirms loss.
		h.clock.Advance(16 * time.Minute)
		if _, err := r.Resolve(ctx, ""); !IsCode(err, "lost") || !r.Lost() {
			t.Fatalf("expected confirmed loss after expiry, got err=%v lost=%v", err, r.Lost())
		}
		// Missing memory after loss must not fall back to generation 0.
		memGen = 0
		if _, err := r.Resolve(ctx, ""); !IsCode(err, "lost") || !r.Lost() {
			t.Fatalf("expected lost with missing memory, got err=%v lost=%v", err, r.Lost())
		}

		// Hook registers gen 2 and hub restarts before MCP sees it (so ex_2 is recovering).
		ex2, err := h.store.Call(ctx, "sessions", store.Request{
			Client: "cursor", Host: "host", SessionID: "cur-1", Cwd: "/repo", Worktree: "/repo",
			ClientPID: int64(anc.PID), ClientPIDStartMS: anc.StartMS, SingleSession: true, Source: "hook",
		})
		if err != nil {
			t.Fatal(err)
		}
		memGen = 2
		h.restart()

		// Even with the stale gen-1 handle, MCP reattaches gen 2 using retained session identity + memory.
		b2, err := r.Resolve(ctx, b1.ExecutionID)
		if err != nil || b2.ExecutionID != ex2["execution_id"] || b2.Generation != 2 || r.Lost() {
			t.Fatalf("did not reattach gen 2 after prior loss: %+v %v lost=%v", b2, err, r.Lost())
		}
		// Subsequent call with stale gone handle refreshes live ex_2.
		b2Again, err := r.Resolve(ctx, b1.ExecutionID)
		if err != nil || b2Again.ExecutionID != b2.ExecutionID {
			t.Fatalf("stale gone handle failed while live: %+v %v", b2Again, err)
		}
	})

	t.Run("refresh loss falls through to replacement on same call for legacy and codex root", func(t *testing.T) {
		h := newRestartableBindingHub(t)
		ex1, err := h.store.Call(ctx, "sessions", store.Request{Client: "codex", Host: "host", SessionID: "s-fall", Cwd: "/repo", Worktree: "/repo", Source: "hook"})
		if err != nil {
			t.Fatal(err)
		}
		r := &Resolver{
			Hub:    &hubclient.Client{Config: config{h.srv.URL}},
			Client: "codex", Host: "host", CWD: "/repo",
			Getenv: func(string) string { return "" },
		}
		if b, _, err := r.ResolveCall(ctx, "", "s-fall", "s-fall"); err != nil || b.ExecutionID != ex1["execution_id"] {
			t.Fatalf("initial root bind: %+v %v", b, err)
		}
		// Expire ex1 and have hook register ex2 before the next tool call.
		h.clock.Advance(16 * time.Minute)
		ex2, err := h.store.Call(ctx, "sessions", store.Request{Client: "codex", Host: "host", SessionID: "s-fall", Cwd: "/repo", Worktree: "/repo", Source: "hook"})
		if err != nil {
			t.Fatal(err)
		}
		b, primary, err := r.ResolveCall(ctx, "", "s-fall", "s-fall")
		if err != nil || !primary || b.ExecutionID != ex2["execution_id"] || r.Lost() {
			t.Fatalf("codex root did not fall through to ex2 on refresh loss: %+v primary=%v err=%v lost=%v", b, primary, err, r.Lost())
		}
	})

	t.Run("ended stays terminal and is never revived", func(t *testing.T) {
		h := newRestartableBindingHub(t)
		ex1, err := h.store.Call(ctx, "sessions", store.Request{Client: "claude", Host: "host", SessionID: "ended-s", Cwd: "/repo", Worktree: "/repo", Source: "hook"})
		if err != nil {
			t.Fatal(err)
		}
		r := &Resolver{
			Hub:    &hubclient.Client{Config: config{h.srv.URL}},
			Client: "claude", Host: "host", CWD: "/repo",
			Getenv: func(k string) string {
				if k == "CLAUDE_CODE_SESSION_ID" {
					return "ended-s"
				}
				return ""
			},
			Recall: func(string, string) (int, bool, error) { return 1, true, nil },
		}
		if _, err := r.Resolve(ctx, ""); err != nil {
			t.Fatal(err)
		}
		if _, err := h.store.Call(ctx, "end", store.Request{ExecutionID: ex1["execution_id"].(string), Generation: 1}); err != nil {
			t.Fatal(err)
		}
		if _, err := r.Resolve(ctx, ""); !IsCode(err, "lost") || !r.Lost() {
			t.Fatalf("ended session was revived or did not report lost: %v lost=%v", err, r.Lost())
		}
		// Confirm no generation-2 Execution was created.
		status, err := h.store.Call(ctx, "status", store.Request{})
		if err != nil {
			t.Fatal(err)
		}
		if got := status["counts"].(map[string]int)["Execution"]; got != 1 {
			t.Fatalf("ended session spawned a replacement Execution: count=%d", got)
		}
	})
}

func TestRefreshTransientHeartbeatFailureAndWatchSourceAndEmptyHeartbeat(t *testing.T) {
	ctx := context.Background()
	var gotSource string
	var status int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/bind" {
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":{"code":"lost","message":"recovering"}}`))
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotSource, _ = body["source"].(string)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	// Unbound Heartbeat is a zero-request no-op.
	empty := &Resolver{Hub: &hubclient.Client{Config: config{srv.URL}}, Client: "claude", Host: "host"}
	if err := empty.Heartbeat(ctx, "watch"); err != nil || gotSource != "" {
		t.Fatalf("empty Heartbeat failed or sent request: err=%v source=%q", err, gotSource)
	}

	// Non-lost heartbeat error preserves r.bound and does not mark lost.
	status = http.StatusServiceUnavailable
	before := Bound{ExecutionID: "ex_1", Generation: 2, Rung: 2}
	r := &Resolver{
		Hub:             &hubclient.Client{Config: config{srv.URL}},
		Client:          "claude",
		Host:            "host",
		HeartbeatSource: "watch",
		bound:           before,
	}
	if _, err := r.Resolve(ctx, ""); err == nil || r.bound != before || r.Lost() || gotSource != "watch" {
		t.Fatalf("transient heartbeat error broke state: err=%v bound=%+v lost=%v source=%q", err, r.bound, r.Lost(), gotSource)
	}

	// Heartbeat succeeds on a bound resolver, and retry /v1/bind failure keeps r.bound intact.
	status = http.StatusOK
	gotSource = ""
	if err := r.Heartbeat(ctx, "watch"); err != nil || r.bound != before || r.Lost() || gotSource != "watch" {
		t.Fatalf("bound Heartbeat failed: err=%v bound=%+v lost=%v source=%q", err, r.bound, r.Lost(), gotSource)
	}
	if _, err := r.Resolve(ctx, ""); !IsCode(err, "lost") || r.bound != before || r.Lost() {
		t.Fatalf("retry bind failure after ok heartbeat broke state: err=%v bound=%+v lost=%v", err, r.bound, r.Lost())
	}
}
