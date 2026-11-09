package binding

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/mateusz-klatt/iris/internal/hub"
	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/proc"
	"github.com/mateusz-klatt/iris/internal/store"
)

type adoptionTransport func(*http.Request) (*http.Response, error)

func (f adoptionTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// A fallback registrar looks for the hook before each heartbeat. A stalled lookup
// or handover must not starve lease renewal and force the next hook to replace a
// live session. Virtual time lets the real store expire the lease without waiting.
func TestStalledAdoptionDoesNotReplaceALiveGeneration(t *testing.T) {
	for _, stage := range []string{"bind", "handover"} {
		t.Run(stage, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s, err := store.Open(filepath.Join(t.TempDir(), "hub.db"), nil)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = s.Close() }()
				h := hub.New(s, "secret", []string{"127.0.0.1"})
				ctx, stop := context.WithCancel(context.Background())
				defer stop()
				stalled, beats := 0, 0
				stall := false
				client := &hubclient.Client{Config: config{"http://127.0.0.1"}, HTTP: &http.Client{Transport: adoptionTransport(func(req *http.Request) (*http.Response, error) {
					if err := req.Context().Err(); err != nil {
						return nil, err
					}
					if stall && ((stage == "bind" && req.URL.Path == "/v1/bind") || (stage == "handover" && strings.HasSuffix(req.URL.Path, "/end"))) {
						stalled++
						select {
						case <-req.Context().Done():
							return nil, req.Context().Err()
						case <-time.After(65 * time.Second):
							return nil, errors.New("stalled adoption request")
						}
					}
					w := httptest.NewRecorder()
					h.ServeHTTP(w, req)
					if strings.HasSuffix(req.URL.Path, "/heartbeat") && w.Code == http.StatusOK {
						beats++
					}
					return w.Result(), nil
				})}}
				r := &Resolver{Hub: client, Client: "claude", Host: "host", CWD: t.TempDir(), Getenv: func(key string) string {
					if key == "CLAUDE_CODE_SESSION_ID" {
						return "running"
					}
					return ""
				}, Ancestor: func() (proc.Ancestor, error) { return proc.Ancestor{PID: 42, StartMS: 77}, nil }}
				before, err := r.Resolve(ctx, "")
				if err != nil || !r.own || before.Generation != 1 {
					t.Fatalf("fallback registration: %+v, own %v, err %v", before, r.own, err)
				}
				hook := store.Request{Client: "claude", Host: "host", SessionID: "running", Cwd: r.CWD, Source: "hook", ClientPID: 42, ClientPIDStartMS: 77}
				if stage == "handover" {
					// On Claude resume the MCP's session id can differ from the hook's.
					late := hook
					late.SessionID = "hook-session"
					out, err := s.Call(ctx, "sessions", late)
					if err != nil {
						t.Fatal(err)
					}
					hook = late
					before.ExecutionID = out["execution_id"].(string)
				}
				claim := store.Request{ExecutionID: before.ExecutionID, Generation: int64(before.Generation), Key: "held", Kind: "exact", Path: "held.go"}
				if _, err := s.Call(ctx, "claim", claim); err != nil {
					t.Fatal(err)
				}
				stall = true
				done := make(chan struct{})
				go func() {
					defer close(done)
					r.Maintain(ctx, "watch", 20*time.Second, func() bool { return true }, func() { t.Error("a live parent was stopped") })
				}()
				elapsed, wantBeats := 100*time.Second, 3
				if stage == "handover" {
					elapsed, wantBeats = 30*time.Second, 1
				}
				time.Sleep(elapsed)
				stop()
				<-done
				// Follow the hook's actual retry: send remembered proof, then drop it
				// only on lost. This used to create generation 2 without a restart.
				hook.Generation = int64(before.Generation)
				after, err := s.Call(context.Background(), "sessions", hook)
				var lost *store.Error
				if errors.As(err, &lost) && lost.Code == "lost" {
					hook.Generation = 0
					after, err = s.Call(context.Background(), "sessions", hook)
				}
				if err != nil || after["execution_id"] != before.ExecutionID || after["generation"] != int64(before.Generation) {
					t.Fatalf("stalled %s replaced a live generation: before %+v, after %v, err %v (stalls %d, heartbeats %d)", stage, before, after, err, stalled, beats)
				}
				if stalled == 0 || beats < wantBeats || r.Lost() {
					t.Fatalf("adoption blocked liveness: stalls %d, heartbeats %d, lost %v", stalled, beats, r.Lost())
				}
				if claims := after["claims"].([]map[string]any); len(claims) != 1 || claims[0]["path"] != "held.go" {
					t.Fatalf("a stalled adoption lost the live session's claim: %v", claims)
				}
				// Once renewal really stops, the old generation still loses authority.
				refused := func(op string, req store.Request) {
					t.Helper()
					_, err := s.Call(context.Background(), op, req)
					var lost *store.Error
					if !errors.As(err, &lost) || lost.Code != "lost" {
						t.Fatalf("stale %s was not refused: %v", op, err)
					}
				}
				time.Sleep(61 * time.Second)
				beat := store.Request{ExecutionID: before.ExecutionID, Generation: int64(before.Generation), Source: "watch"}
				refused("heartbeat", beat)
				hook.Generation = 0
				replacement, err := s.Call(context.Background(), "sessions", hook)
				if err != nil || replacement["execution_id"] == before.ExecutionID || replacement["generation"] != int64(before.Generation+1) {
					t.Fatalf("a truly expired session did not advance: %v, err %v", replacement, err)
				}
				refused("heartbeat", beat)
				refused("claim", claim)
				refused("end", beat)
				hook.Generation = int64(before.Generation)
				refused("sessions", hook)
			})
		})
	}
}
