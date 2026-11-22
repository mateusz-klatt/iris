package binding

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
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
	_ "modernc.org/sqlite"
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

func (h *restartableBindingHub) backup() string {
	h.t.Helper()
	if err := h.store.Close(); err != nil {
		h.t.Fatal(err)
	}
	raw, err := os.ReadFile(h.file)
	if err != nil {
		h.t.Fatal(err)
	}
	dst := filepath.Join(h.t.TempDir(), "backup.db")
	if err := os.WriteFile(dst, raw, 0o600); err != nil {
		h.t.Fatal(err)
	}
	h.open()
	return dst
}

func (h *restartableBindingHub) restore(backupPath string) {
	h.t.Helper()
	if err := h.store.Close(); err != nil {
		h.t.Fatal(err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		_ = os.Remove(h.file + suffix)
	}
	raw, err := os.ReadFile(backupPath)
	if err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(h.file, raw, 0o600); err != nil {
		h.t.Fatal(err)
	}
	db, err := sql.Open("sqlite", h.file)
	if err != nil {
		h.t.Fatal(err)
	}
	if err := store.MarkRestore(context.Background(), db, h.clock.Now()); err != nil {
		_ = db.Close()
		h.t.Fatal(err)
	}
	if err := db.Close(); err != nil {
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

func TestForeignRung1AndRung4NeverLeakIntoLast(t *testing.T) {
	ctx := context.Background()

	for _, tc := range []struct {
		name   string
		rung1  bool
		tick   bool
		cursor bool
	}{
		{"lost rung 1 on handle-less call uses env session not override session", true, false, false},
		{"lost rung 1 on maintain tick uses env session not override session", true, true, false},
		{"lost rung 4 on handle-less call uses env session not handle session", false, false, false},
		{"lost rung 4 on maintain tick uses env session not handle session", false, true, false},
		{"lost rung 1 on cursor without env never recalls foreign session", true, false, true},
		{"lost rung 4 on cursor without env never recalls foreign session", false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newRestartableBindingHub(t)
			client := "claude"
			if tc.cursor {
				client = "cursor"
			}
			exB, err := h.store.Call(ctx, "sessions", store.Request{Client: client, Host: "host", SessionID: "S_B", Cwd: "/repo", Worktree: "/repo", Source: "hook"})
			if err != nil {
				t.Fatal(err)
			}
			exBID := exB["execution_id"].(string)
			env := map[string]string{}
			if !tc.cursor {
				env["CLAUDE_CODE_SESSION_ID"] = "S_A"
			}
			if tc.rung1 {
				env["IRIS_EXECUTION"] = exBID
			}
			var recalled []string
			var recallMu sync.Mutex
			r := &Resolver{
				Hub:    &hubclient.Client{Config: config{h.srv.URL}},
				Client: client, Host: "host", CWD: "/repo",
				Getenv:   func(k string) string { return env[k] },
				Ancestor: func() (proc.Ancestor, error) { return proc.Ancestor{}, nil },
				Recall: func(c, session string) (int, bool, error) {
					recallMu.Lock()
					recalled = append(recalled, session)
					recallMu.Unlock()
					switch session {
					case "S_A":
						return 1, true, nil
					case "S_B":
						return 2, true, nil
					default:
						return 0, false, nil
					}
				},
			}
			// Initial bind uses Rung 1 (override) or Rung 4 (explicit handle) for foreign session S_B,
			// before S_A is registered so Rung 4 is reached without Rung 2 matching first.
			handle := ""
			wantRung := 1
			if !tc.rung1 {
				handle = exBID
				wantRung = 4
			}
			initial, err := r.Resolve(ctx, handle)
			if err != nil || initial.ExecutionID != exBID || initial.Rung != wantRung {
				t.Fatalf("initial bind: %+v %v", initial, err)
			}
			if r.last.ExecutionID != "" {
				t.Fatalf("foreign rung %d polluted r.last: %+v", wantRung, r.last)
			}

			// Register S_A, end ex_B, register S_B gen 2, and restart hub so both S_A (gen 1) and S_B (gen 2) are recovering.
			exA, err := h.store.Call(ctx, "sessions", store.Request{Client: client, Host: "host", SessionID: "S_A", Cwd: "/repo", Worktree: "/repo", Source: "hook"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := h.store.Call(ctx, "end", store.Request{ExecutionID: exBID, Generation: 1}); err != nil {
				t.Fatal(err)
			}
			if _, err := h.store.Call(ctx, "sessions", store.Request{Client: client, Host: "host", SessionID: "S_B", Cwd: "/repo", Worktree: "/repo", Source: "hook"}); err != nil {
				t.Fatal(err)
			}
			h.restart()

			if tc.tick {
				// Confirm ex_B lost via Heartbeat, then let Maintain's tick look up the process's own session.
				if err := r.Heartbeat(ctx, "shim"); !IsCode(err, "lost") || !r.Lost() {
					t.Fatalf("expected heartbeat lost for ex_B: err=%v lost=%v", err, r.Lost())
				}
				maintainUntil(t, r, func(int) bool {
					r.mu.Lock()
					defer r.mu.Unlock()
					return r.bound.ExecutionID == exA["execution_id"].(string)
				})
				r.mu.Lock()
				bound := r.bound
				r.mu.Unlock()
				if bound.ExecutionID != exA["execution_id"].(string) || bound.SessionID != "S_A" || bound.Rung != 2 {
					t.Fatalf("maintain bound wrong session: %+v", bound)
				}
			} else if tc.cursor {
				if _, err := r.Resolve(ctx, ""); !IsCode(err, "lost") || !r.Lost() {
					t.Fatalf("expected lost without env session: err=%v lost=%v", err, r.Lost())
				}
				recallMu.Lock()
				gotRecalled := append([]string(nil), recalled...)
				recallMu.Unlock()
				if len(gotRecalled) != 0 {
					t.Fatalf("cursor without process-owned binding recalled foreign sessions: %v", gotRecalled)
				}
			} else {
				got, err := r.Resolve(ctx, "")
				if err != nil || got.ExecutionID != exA["execution_id"].(string) || got.SessionID != "S_A" || got.Rung != 2 {
					t.Fatalf("expected S_A reattach, got %+v %v", got, err)
				}
			}
			for _, req := range h.sessions() {
				if req.SessionID == "S_B" {
					t.Fatalf("leaked foreign session S_B into shim reattach: %+v", req)
				}
			}
		})
	}
}

func TestOverrideOrDecideDecidesModeAfterRefreshConfirmsLoss(t *testing.T) {
	ctx := context.Background()

	t.Run("sub-agent call after override loss dispatches as lookup-only subagent", func(t *testing.T) {
		h := newRestartableBindingHub(t)
		overrideReg, err := h.store.Call(ctx, "sessions", store.Request{Client: "codex", Host: "host", SessionID: "override-s", Cwd: "/repo", Worktree: "/repo", Source: "hook"})
		if err != nil {
			t.Fatal(err)
		}
		rootReg, err := h.store.Call(ctx, "sessions", store.Request{Client: "codex", Host: "host", SessionID: "root-s", Cwd: "/repo", Worktree: "/repo", Source: "hook"})
		if err != nil {
			t.Fatal(err)
		}
		overrideID := overrideReg["execution_id"].(string)
		rootID := rootReg["execution_id"].(string)

		r := &Resolver{
			Hub:    &hubclient.Client{Config: config{h.srv.URL}},
			Client: "codex", Host: "host", CWD: "/repo",
			Getenv: func(k string) string {
				if k == "IRIS_EXECUTION" {
					return overrideID
				}
				return ""
			},
			Ancestor: func() (proc.Ancestor, error) { return proc.Ancestor{}, nil },
		}
		// Initial call binds the live IRIS_EXECUTION override without deciding mode.
		if b, primary, err := r.ResolveCall(ctx, "", "root-s", "child-thread"); err != nil || !primary || b.ExecutionID != overrideID || decidedMode(r) != "undecided" {
			t.Fatalf("initial override bind: %+v primary=%v err=%v mode=%s", b, primary, err, decidedMode(r))
		}
		// End override Execution (no Maintain runs); next call's refresh confirms loss.
		if _, err := h.store.Call(ctx, "end", store.Request{ExecutionID: overrideID, Generation: 1}); err != nil {
			t.Fatal(err)
		}
		// Sub-agent call must decide subagent mode and dispatch as lookup-only (primary=false).
		b, primary, err := r.ResolveCall(ctx, "ex_foreign_handle", "root-s", "child-thread")
		if err != nil || primary || b.ExecutionID != rootID || decidedMode(r) != "subagent" || r.bound.ExecutionID != "" {
			t.Fatalf("sub-agent call after override loss broke isolation: %+v primary=%v err=%v mode=%s bound=%+v", b, primary, err, decidedMode(r), r.bound)
		}
		if _, primary, err := r.ResolveCall(ctx, "", "", ""); err != errNoSession || primary {
			t.Fatalf("sub-agent process did not reject call without session: err=%v primary=%v", err, primary)
		}
		if len(h.sessions()) != 0 {
			t.Fatalf("sub-agent call registered a session: %+v", h.sessions())
		}
	})

	t.Run("root call after override loss decides root mode and binds session", func(t *testing.T) {
		h := newRestartableBindingHub(t)
		overrideReg, err := h.store.Call(ctx, "sessions", store.Request{Client: "codex", Host: "host", SessionID: "override-s", Cwd: "/repo", Worktree: "/repo", Source: "hook"})
		if err != nil {
			t.Fatal(err)
		}
		rootReg, err := h.store.Call(ctx, "sessions", store.Request{Client: "codex", Host: "host", SessionID: "root-s", Cwd: "/repo", Worktree: "/repo", Source: "hook"})
		if err != nil {
			t.Fatal(err)
		}
		overrideID := overrideReg["execution_id"].(string)
		rootID := rootReg["execution_id"].(string)

		r := &Resolver{
			Hub:    &hubclient.Client{Config: config{h.srv.URL}},
			Client: "codex", Host: "host", CWD: "/repo",
			Getenv: func(k string) string {
				if k == "IRIS_EXECUTION" {
					return overrideID
				}
				return ""
			},
			Ancestor: func() (proc.Ancestor, error) { return proc.Ancestor{}, nil },
		}
		if _, _, err := r.ResolveCall(ctx, "", "root-s", "root-s"); err != nil {
			t.Fatal(err)
		}
		if _, err := h.store.Call(ctx, "end", store.Request{ExecutionID: overrideID, Generation: 1}); err != nil {
			t.Fatal(err)
		}
		b, primary, err := r.ResolveCall(ctx, "", "root-s", "root-s")
		if err != nil || !primary || b.ExecutionID != rootID || decidedMode(r) != "root" || r.Lost() {
			t.Fatalf("root call after override loss failed: %+v primary=%v err=%v mode=%s lost=%v", b, primary, err, decidedMode(r), r.Lost())
		}
	})

	t.Run("initial override bind error stays overridden without deciding mode", func(t *testing.T) {
		h := &callHub{sessions: map[string]string{"root-s": "ex_root"}, lost: map[string]bool{"ex_bad": true}}
		r, _ := callResolver(t, h, map[string]string{"IRIS_EXECUTION": "ex_bad"})
		if _, primary, err := r.ResolveCall(ctx, "", "root-s", "child"); !IsCode(err, "lost") || !primary || decidedMode(r) != "undecided" {
			t.Fatalf("initial override bind failure: primary=%v err=%v mode=%s", primary, err, decidedMode(r))
		}
		if got := h.seen(); len(got.lookups) != 0 {
			t.Fatalf("initial override failure leaked into session lookup: %+v", got)
		}
	})
}

func TestRung3ProcessIdentityAndBeatOnePreservesHookSeen(t *testing.T) {
	ctx := context.Background()

	t.Run("beatOne preserves HookSeen when heartbeat omits hook_seen", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"lease_ms":60000}`))
		}))
		defer srv.Close()
		r := &Resolver{
			Hub:    &hubclient.Client{Config: config{srv.URL}},
			Client: "cursor", Host: "host",
			bound: Bound{ExecutionID: "ex_1", Generation: 1, SessionID: "s1", Rung: 3, HookSeen: true},
		}
		if err := r.beatOne(ctx, Bound{ExecutionID: "ex_1", Generation: 1, SessionID: "s1", Rung: 3, HookSeen: false}, "shim"); err != nil {
			t.Fatal(err)
		}
		if !r.bound.HookSeen || !r.last.HookSeen {
			t.Fatalf("beatOne cleared HookSeen: bound=%+v last=%+v", r.bound, r.last)
		}
	})

	t.Run("rung 3 memory reattach sends client process identity and refuses foreign process", func(t *testing.T) {
		h := newRestartableBindingHub(t)
		ancA := proc.Ancestor{PID: 5000, StartMS: 12345}
		ancB := proc.Ancestor{PID: 6000, StartMS: 99999}
		// Hook registers S1 under process A.
		ex1, err := h.store.Call(ctx, "sessions", store.Request{
			Client: "cursor", Host: "host", SessionID: "S1", Cwd: "/repo", Worktree: "/repo",
			ClientPID: int64(ancA.PID), ClientPIDStartMS: ancA.StartMS, SingleSession: true, Source: "hook",
		})
		if err != nil {
			t.Fatal(err)
		}
		curAnc := ancA
		memGen := 1
		rA := &Resolver{
			Hub:    &hubclient.Client{Config: config{h.srv.URL}},
			Client: "cursor", Host: "host", CWD: "/repo",
			Getenv:   func(string) string { return "" },
			Ancestor: func() (proc.Ancestor, error) { return curAnc, nil },
			Recall: func(client, session string) (int, bool, error) {
				if client == "cursor" && session == "S1" {
					return memGen, true, nil
				}
				return 0, false, nil
			},
		}
		b1, err := rA.Resolve(ctx, "")
		if err != nil || b1.ExecutionID != ex1["execution_id"] || b1.Rung != 3 || !b1.HookSeen {
			t.Fatalf("initial rung-3 bind: %+v %v", b1, err)
		}

		// Expire gen 1, confirm loss in process A, then hook resumes S1 in process B and hub restarts.
		h.clock.Advance(16 * time.Minute)
		if _, err := rA.Resolve(ctx, ""); !IsCode(err, "lost") || !rA.Lost() {
			t.Fatalf("expected loss for process A: err=%v lost=%v", err, rA.Lost())
		}
		ex2, err := h.store.Call(ctx, "sessions", store.Request{
			Client: "cursor", Host: "host", SessionID: "S1", Cwd: "/repo", Worktree: "/repo",
			ClientPID: int64(ancB.PID), ClientPIDStartMS: ancB.StartMS, SingleSession: true, Source: "hook",
		})
		if err != nil {
			t.Fatal(err)
		}
		memGen = 2
		h.restart()

		// Without a client process identity, rung-3 memory reattach sends no /v1/sessions call.
		beforeMissing := len(h.sessions())
		curAnc = proc.Ancestor{}
		if _, err := rA.Resolve(ctx, ""); !IsCode(err, "lost") || len(h.sessions()) != beforeMissing {
			t.Fatalf("missing ancestor sent reattach: err=%v reqs=%+v", err, h.sessions())
		}

		// Process A sends its own PID/StartMS and is refused because S1 now belongs to process B.
		curAnc = ancA
		if _, err := rA.Resolve(ctx, ""); !IsCode(err, "lost") || !rA.Lost() {
			t.Fatalf("process A reattached process B's session: err=%v lost=%v", err, rA.Lost())
		}
		reqs := h.sessions()[beforeMissing:]
		if len(reqs) != 1 || reqs[0].ClientPID != ancA.PID || reqs[0].ClientPIDStartMS != ancA.StartMS {
			t.Fatalf("rung-3 reattach did not send client process identity: %+v", reqs)
		}

		// Process B with matching PID/StartMS reattaches cleanly.
		curAnc = ancB
		b2, err := rA.Resolve(ctx, "")
		if err != nil || b2.ExecutionID != ex2["execution_id"] || b2.Generation != 2 || rA.Lost() {
			t.Fatalf("process B failed to reattach its own session: %+v %v lost=%v", b2, err, rA.Lost())
		}
	})
}

func TestPost120sEndedRestoreAndMaintainBadMemoryRefusals(t *testing.T) {
	ctx := context.Background()

	t.Run("post-120s refusal for bound MCP", func(t *testing.T) {
		h := newRestartableBindingHub(t)
		if _, err := h.store.Call(ctx, "sessions", store.Request{Client: "claude", Host: "host", SessionID: "s1", Cwd: "/repo", Worktree: "/repo", Source: "hook"}); err != nil {
			t.Fatal(err)
		}
		r := &Resolver{
			Hub:    &hubclient.Client{Config: config{h.srv.URL}},
			Client: "claude", Host: "host", CWD: "/repo",
			Getenv: func(k string) string { return map[string]string{"CLAUDE_CODE_SESSION_ID": "s1"}[k] },
			Recall: func(string, string) (int, bool, error) { return 1, true, nil },
		}
		if _, err := r.Resolve(ctx, ""); err != nil {
			t.Fatal(err)
		}
		h.restart()
		h.clock.Advance(121 * time.Second)
		if _, err := r.Resolve(ctx, ""); !IsCode(err, "lost") || !r.Lost() {
			t.Fatalf("bound MCP revived past 120s: err=%v lost=%v", err, r.Lost())
		}
		status, err := h.store.Call(ctx, "status", store.Request{})
		if err != nil || status["counts"].(map[string]int)["Execution"] != 1 {
			t.Fatalf("post-120s bound MCP created new Execution: %+v %v", status, err)
		}
	})

	t.Run("post-120s refusal for unbound legacy MCP with stale memory", func(t *testing.T) {
		h := newRestartableBindingHub(t)
		if _, err := h.store.Call(ctx, "sessions", store.Request{Client: "claude", Host: "host", SessionID: "s1", Cwd: "/repo", Worktree: "/repo", Source: "hook"}); err != nil {
			t.Fatal(err)
		}
		h.restart()
		h.clock.Advance(121 * time.Second)
		r := &Resolver{
			Hub:    &hubclient.Client{Config: config{h.srv.URL}},
			Client: "claude", Host: "host", CWD: "/repo",
			Getenv: func(k string) string { return map[string]string{"CLAUDE_CODE_SESSION_ID": "s1"}[k] },
			Recall: func(string, string) (int, bool, error) { return 1, true, nil },
		}
		if _, err := r.Resolve(ctx, ""); !IsCode(err, "lost") {
			t.Fatalf("unbound legacy MCP revived past 120s: %v", err)
		}
		reqs := h.sessions()
		if len(reqs) != 1 || reqs[0].Generation != 1 || reqs[0].Source != "shim" {
			t.Fatalf("expected single gen-1 shim reattach attempt, got %+v", reqs)
		}
	})

	t.Run("post-120s refusal for unbound codex root with stale memory", func(t *testing.T) {
		h := newRestartableBindingHub(t)
		if _, err := h.store.Call(ctx, "sessions", store.Request{Client: "codex", Host: "host", SessionID: "c1", Cwd: "/repo", Worktree: "/repo", Source: "hook"}); err != nil {
			t.Fatal(err)
		}
		h.restart()
		h.clock.Advance(121 * time.Second)
		r := &Resolver{
			Hub:    &hubclient.Client{Config: config{h.srv.URL}},
			Client: "codex", Host: "host", CWD: "/repo",
			Getenv: func(string) string { return "" },
			Recall: func(string, string) (int, bool, error) { return 1, true, nil },
		}
		if _, _, err := r.ResolveCall(ctx, "", "c1", "c1"); !IsCode(err, "lost") {
			t.Fatalf("unbound codex root revived past 120s: %v", err)
		}
		reqs := h.sessions()
		if len(reqs) != 1 || reqs[0].Generation != 1 || reqs[0].Source != "shim" {
			t.Fatalf("expected single gen-1 shim reattach attempt, got %+v", reqs)
		}
	})

	t.Run("unbound MCP against ended session with stale memory never revives", func(t *testing.T) {
		h := newRestartableBindingHub(t)
		reg, err := h.store.Call(ctx, "sessions", store.Request{Client: "claude", Host: "host", SessionID: "s-ended", Cwd: "/repo", Worktree: "/repo", Source: "hook"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := h.store.Call(ctx, "end", store.Request{ExecutionID: reg["execution_id"].(string), Generation: 1}); err != nil {
			t.Fatal(err)
		}
		r := &Resolver{
			Hub:    &hubclient.Client{Config: config{h.srv.URL}},
			Client: "claude", Host: "host", CWD: "/repo",
			Getenv: func(k string) string { return map[string]string{"CLAUDE_CODE_SESSION_ID": "s-ended"}[k] },
			Recall: func(string, string) (int, bool, error) { return 1, true, nil },
		}
		if _, err := r.Resolve(ctx, ""); !IsCode(err, "lost") {
			t.Fatalf("unbound MCP revived ended session: %v", err)
		}
		status, err := h.store.Call(ctx, "status", store.Request{})
		if err != nil || status["counts"].(map[string]int)["Execution"] != 1 {
			t.Fatalf("ended session spawned replacement: %+v %v", status, err)
		}
	})

	t.Run("bound MCP against restored older generation is refused without downgrade", func(t *testing.T) {
		h := newRestartableBindingHub(t)
		ex1, err := h.store.Call(ctx, "sessions", store.Request{Client: "claude", Host: "host", SessionID: "s-restore", Cwd: "/repo", Worktree: "/repo", Source: "hook"})
		if err != nil {
			t.Fatal(err)
		}
		backupPath := h.backup()
		// Reattach ex1 after backup's reopen so end can transition it cleanly to gen 2.
		if _, err := h.store.Call(ctx, "heartbeat", store.Request{ExecutionID: ex1["execution_id"].(string), Generation: 1, Source: "shim"}); err != nil {
			t.Fatal(err)
		}
		if _, err := h.store.Call(ctx, "end", store.Request{ExecutionID: ex1["execution_id"].(string), Generation: 1}); err != nil {
			t.Fatal(err)
		}
		ex2, err := h.store.Call(ctx, "sessions", store.Request{Client: "claude", Host: "host", SessionID: "s-restore", Cwd: "/repo", Worktree: "/repo", Source: "hook"})
		if err != nil {
			t.Fatal(err)
		}
		r := &Resolver{
			Hub:    &hubclient.Client{Config: config{h.srv.URL}},
			Client: "claude", Host: "host", CWD: "/repo",
			Getenv: func(k string) string { return map[string]string{"CLAUDE_CODE_SESSION_ID": "s-restore"}[k] },
			Recall: func(string, string) (int, bool, error) { return 2, true, nil },
		}
		b, err := r.Resolve(ctx, "")
		if err != nil || b.ExecutionID != ex2["execution_id"] || b.Generation != 2 {
			t.Fatalf("initial gen-2 bind: %+v %v", b, err)
		}

		// Restore gen-1 backup with restore marker: gen-2 MCP must be refused lost and never downgraded.
		h.restore(backupPath)
		if _, err := r.Resolve(ctx, ""); !IsCode(err, "lost") || !r.Lost() || r.bound.ExecutionID != "" {
			t.Fatalf("bound MCP downgraded to restored older generation: err=%v lost=%v bound=%+v", err, r.Lost(), r.bound)
		}
	})

	t.Run("maintain tick with malformed or mismatched memory stays unbound without generation 0", func(t *testing.T) {
		for _, tc := range []struct {
			name     string
			gen      int
			recallEr error
			wantReqs int
		}{
			{"malformed", 0, errors.New("bad"), 0},
			{"mismatched", 2, nil, 1},
		} {
			t.Run(tc.name, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					var sessions int
					h := &tickHub{reply: func(req *http.Request, body map[string]any) (int, any) {
						switch req.URL.Path {
						case "/v1/bind":
							return refused("unbound")
						case "/v1/sessions":
							sessions++
							if body["generation"] == float64(0) {
								t.Errorf("maintain fell back to generation 0: %+v", body)
							}
							return refused("lost")
						default:
							return leaseGranted()
						}
					}}
					r := tickResolver(h.client(), "claude", sessionS, proc.Ancestor{})
					r.Recall = func(string, string) (int, bool, error) { return tc.gen, true, tc.recallEr }
					stop := maintain(t, r, "shim")
					time.Sleep(21 * time.Second)
					stop()
					if sessions != tc.wantReqs || r.bound.ExecutionID != "" {
						t.Fatalf("maintain with %s memory: sessions=%d (want %d) bound=%+v", tc.name, sessions, tc.wantReqs, r.bound)
					}
				})
			})
		}
	})
}
