package binding

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
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

func TestAdoptionRetriesHandoverUntilItsResponseArrives(t *testing.T) {
	for _, committed := range []bool{false, true} {
		name := "request_cancelled"
		if committed {
			name = "committed_response_lost"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s, err := store.Open(filepath.Join(t.TempDir(), "hub.db"), nil)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = s.Close() }()
				h := hub.New(s, "secret", []string{"127.0.0.1"})
				ctx := context.Background()
				call := func(op string, req store.Request) map[string]any {
					t.Helper()
					out, err := s.Call(ctx, op, req)
					if err != nil {
						t.Fatalf("%s: %v", op, err)
					}
					return out
				}
				var mine Bound
				var target string
				ends, slowBind := 0, true
				client := &hubclient.Client{Config: config{"http://127.0.0.1"}, HTTP: &http.Client{Transport: adoptionTransport(func(req *http.Request) (*http.Response, error) {
					if err := req.Context().Err(); err != nil {
						return nil, err
					}
					end := strings.HasSuffix(req.URL.Path, "/end")
					if end {
						var body struct {
							Generation int    `json:"generation"`
							HandOver   string `json:"hand_over"`
						}
						raw, err := io.ReadAll(req.Body)
						if err != nil || json.Unmarshal(raw, &body) != nil || body.Generation != mine.Generation || body.HandOver != target || req.URL.Path != "/v1/executions/"+mine.ExecutionID+"/end" {
							t.Fatalf("handover changed its source, proof or destination: %s %s, err %v", req.URL.Path, raw, err)
						}
						req.Body = io.NopCloser(bytes.NewReader(raw))
						ends++
					}
					w := httptest.NewRecorder()
					if !end || committed || ends >= 5 {
						h.ServeHTTP(w, req)
					}
					if target != "" && req.URL.Path == "/v1/bind" && w.Code == http.StatusOK && slowBind {
						slowBind = false
						time.Sleep(4800 * time.Millisecond)
					}
					if end && ends < 5 {
						// The first bind takes 4.8s and the handover needs 0.4s:
						// the shared 5s deadline cancels before its response.
						select {
						case <-req.Context().Done():
							return nil, req.Context().Err()
						case <-time.After(400 * time.Millisecond):
							return nil, errors.New("handover response unavailable")
						}
					}
					return w.Result(), nil
				})}}
				r := &Resolver{Hub: client, Client: "claude", Host: "host", CWD: t.TempDir(), Getenv: func(key string) string {
					if key == "CLAUDE_CODE_SESSION_ID" {
						return "fallback"
					}
					return ""
				}, Ancestor: func() (proc.Ancestor, error) { return proc.Ancestor{PID: 42, StartMS: 77}, nil }}
				mine, err = r.Resolve(ctx, "")
				if err != nil || !r.own {
					t.Fatalf("fallback: %+v %v", mine, err)
				}
				sender := call("sessions", store.Request{Client: "codex", Host: "host", SessionID: "sender", Cwd: r.CWD})
				message := call("messages", store.Request{ExecutionID: sender["execution_id"].(string), Generation: 1, Key: "mail", To: []string{mine.Agent.Name}, Subject: "waiting", Body: "mail for the session"})
				hook := call("sessions", store.Request{Client: "claude", Host: "host", SessionID: "hook", Cwd: r.CWD, Source: "hook", ClientPID: 42, ClientPIDStartMS: 77})
				target = hook["execution_id"].(string)
				run, stop := context.WithCancel(ctx)
				done := make(chan struct{})
				go func() {
					defer close(done)
					r.Maintain(run, "shim", 20*time.Second, func() bool { return true }, func() { t.Error("live parent stopped") })
				}()
				defer func() { stop(); <-done }()
				time.Sleep(90 * time.Second)
				synctest.Wait()
				if ends != 4 {
					t.Fatalf("failed handover was not retried while renewing: %d requests, want 4", ends)
				}
				if !committed {
					// More than the fallback's original 60s lease has passed. Its
					// mail must stay reachable while the handover is unconfirmed.
					call("validate", store.Request{ExecutionID: mine.ExecutionID, Generation: int64(mine.Generation)})
					got := call("inbox", store.Request{ExecutionID: mine.ExecutionID})["deliveries"].([]map[string]any)
					if len(got) != 1 || got[0]["message_id"] != message["message_id"] {
						t.Fatalf("pending fallback mail was lost: %v", got)
					}
				}
				time.Sleep(20 * time.Second)
				synctest.Wait()
				if ends != 5 || r.Lost() {
					t.Fatalf("handover did not settle without losing binding: requests %d, lost %v", ends, r.Lost())
				}
				got := call("inbox", store.Request{ExecutionID: target})["deliveries"].([]map[string]any)
				if len(got) != 1 || got[0]["message_id"] != message["message_id"] {
					t.Fatalf("handover did not deliver exactly once: %v", got)
				}
				time.Sleep(40 * time.Second)
				synctest.Wait()
				if ends != 5 {
					t.Fatalf("confirmed handover kept retrying: %d", ends)
				}
				b, err := r.Resolve(ctx, "")
				if err != nil || b.ExecutionID != target || b.Generation != 1 {
					t.Fatalf("handover changed the hook generation: %+v %v", b, err)
				}
			})
		})
	}
}
