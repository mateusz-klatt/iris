package wake

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/hook"
	"github.com/mateusz-klatt/iris/internal/hub"
	"github.com/mateusz-klatt/iris/internal/sessionproof"
	"github.com/mateusz-klatt/iris/internal/store"
)

func TestAntigravityWakeHeartbeatAcrossHubRestart(t *testing.T) {
	const session = "e28a6164-6409-4810-b8ee-df8202a16a0a"
	const attempt = "00112233445566778899aabbccddeeff"
	for _, tc := range []struct {
		name    string
		opted   bool
		changed bool
	}{
		{name: "OFF uses the v0.1.15 trace despite corrupt exact proof"},
		{name: "ON reattaches the same process", opted: true},
		{name: "ON loses a changed process tuple", opted: true, changed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newAgyTestHarness(t, session)
			request := store.Request{Client: "antigravity", Host: "test-host", Cwd: "/repo", SessionID: session,
				ClientPID: int64(h.agyPID), ClientPIDStartMS: h.agyStartMS, SingleSession: true, Source: "hook", Generation: 1}
			b, err := h.store.Call(t.Context(), "bind", request)
			if err != nil {
				t.Fatal(err)
			}
			execution := b["execution_id"].(string)
			proofPath := wakeGenerationPath(h.tokenPath, "antigravity", session) + ".execution"
			if tc.opted {
				proof, err := sessionproof.EncodeRecord(sessionproof.Record{Version: sessionproof.FormatVersion,
					Origin: sessionproof.OriginHook, Generation: 1, ExecutionID: execution})
				if err != nil {
					t.Fatal(err)
				}
				writeWakeProof(t, proofPath, string(proof))
			} else {
				writeWakeProof(t, proofPath, "damaged exact record")
			}

			type exchange struct {
				method string
				uri    string
				body   string
				status int
			}
			type pendingReply struct {
				exchange
				release chan struct{}
			}
			trace := make(chan pendingReply)
			server := httptest.NewUnstartedServer(nil)
			var active atomic.Pointer[hub.Hub]
			hosts := []string{server.Listener.Addr().String()}
			active.Store(hub.New(h.store, "secret-hub-token", hosts))
			server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					active.Load().ServeHTTP(w, r)
					return
				}
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Errorf("read wake request: %v", err)
					return
				}
				r.Body = io.NopCloser(bytes.NewReader(body))
				result := httptest.NewRecorder()
				active.Load().ServeHTTP(result, r)
				release := make(chan struct{})
				select {
				case trace <- pendingReply{exchange{r.Method, r.URL.RequestURI(), string(body), result.Code}, release}:
				case <-r.Context().Done():
					return
				}
				select {
				case <-release:
					w.Header().Set("Content-Type", result.Header().Get("Content-Type"))
					w.WriteHeader(result.Code)
					_, _ = w.Write(result.Body.Bytes())
				case <-r.Context().Done():
				}
			})
			server.Start()
			defer server.Close()
			cfg := fmt.Appendf(nil, `{"hub_url":%q,"session_succession":%t}`, server.URL, tc.opted)
			if err := os.WriteFile(filepath.Join(h.configDir, "client.json"), cfg, 0o600); err != nil {
				t.Fatal(err)
			}
			owner := hook.AntigravityHelperOwner{AgyPID: h.agyPID, AgyStartMS: h.agyStartMS,
				HelperPID: h.agyPID, HelperStartMS: h.agyStartMS, SessionID: session, AttemptID: attempt}
			if _, err := hook.PublishAntigravityHelper(h.configDir, owner, nil); err != nil {
				t.Fatal(err)
			}
			opts := h.baseOptions(session, "synthetic-csrf")
			baseEnv := opts.getenv
			opts.getenv = func(k string) string {
				if k == antigravityHelperEnv {
					return fmt.Sprintf("%d:%d:%s", h.agyPID, h.agyStartMS, attempt)
				}
				return baseEnv(k)
			}
			// Keep the separate attention worker's HookSeen probe outside this recovery trace.
			opts.settle = time.Minute
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			done := make(chan error, 1)
			go func() { done <- runAntigravity(ctx, opts) }()
			var reopened *store.Store
			defer func() {
				cancel()
				if err := <-done; err != nil {
					t.Error(err)
				}
				if reopened != nil {
					_ = reopened.Close()
				}
			}()
			expect := func(want exchange) chan struct{} {
				t.Helper()
				select {
				case reply := <-trace:
					if reply.exchange != want {
						t.Fatalf("wake request = %+v, want %+v", reply.exchange, want)
					}
					return reply.release
				case <-ctx.Done():
					t.Fatalf("timed out waiting for %+v", want)
					return nil
				}
			}

			// The OFF lookup and heartbeat bytes are the v0.1.15 wire contract.
			bindBody := fmt.Sprintf(`{"client":"antigravity","client_pid":%d,"client_pid_start_ms":%d,"cwd":"/repo","host":"test-host"}`, h.agyPID, h.agyStartMS)
			beat := exchange{http.MethodPost, "/v1/executions/" + execution + "/heartbeat", `{"generation":1,"source":"shim"}`, http.StatusOK}
			if tc.opted {
				bindBody = bindBody[:len(bindBody)-1] + `,"succession_proof":true}`
				beat.uri += "?process_bound=true"
				beat.body = `{"generation":1,"source":"shim","succession_proof":true}`
			}
			lookup := exchange{http.MethodPost, "/v1/bind", bindBody, http.StatusOK}
			release := expect(lookup)
			if tc.changed {
				request.ClientPIDStartMS++
				if _, err := h.store.Call(t.Context(), "sessions", request); err != nil {
					t.Fatal(err)
				}
			}
			// Reopen SQLite after the process lookup commits and before wake can send a heartbeat.
			if err := h.store.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err = store.Open(filepath.Join(filepath.Dir(h.configDir), "hub.db"), nil)
			if err != nil {
				t.Fatal(err)
			}
			active.Store(hub.New(reopened, "secret-hub-token", hosts))
			close(release)
			if tc.opted {
				refused := beat
				refused.status = http.StatusConflict
				close(expect(refused))
				recovery := exchange{http.MethodPost, "/v1/sessions?process_bound=true",
					fmt.Sprintf(`{"common_dir":"","generation":1,"client":"antigravity","session_id":%q,"host":"test-host","cwd":"/repo","worktree":"","worktree_id":"","remote_url":"","root_commit":"","branch":"","parent_session_id":"","client_pid":%d,"client_pid_start_ms":%d,"single_session":false,"source":"shim"}`,
						session, h.agyPID, h.agyStartMS), http.StatusOK}
				if tc.changed {
					recovery.status = http.StatusConflict
				}
				close(expect(recovery))
			} else {
				close(expect(beat))
			}
			if tc.changed {
				// The lost binding is discarded. The next tick looks up the old tuple and
				// cannot renew it or fall back to a generation-zero registration.
				lookup.status = http.StatusConflict
				_ = expect(lookup)
				return
			}
			// An additional tick renews the same exact pair once recovery has completed.
			_ = expect(beat)
		})
	}
}
