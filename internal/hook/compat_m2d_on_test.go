//go:build compat

package hook

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/binding"
	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/proc"
	"github.com/mateusz-klatt/iris/internal/sessionproof"
)

func compatSeedProcessSession(t *testing.T, hub *liveCompatHub, req hubclient.SessionRequest, tokenPath string) hubclient.SessionResponse {
	t.Helper()
	seed := compatClient(compatHookEnv(t, hub), nil)
	reg, err := seed.RegisterSession(t.Context(), req)
	if err != nil || reg.Generation != 1 || reg.ExecutionID == "" {
		t.Fatalf("hook registration = %+v, %v", reg, err)
	}
	m := NewSessions(tokenPath).memory
	m.Remember(req.Client, req.SessionID, reg.Generation)
	m2cWriteExact(t, m, req.Client, req.SessionID, sessionproof.Record{
		Version: sessionproof.FormatVersion, Origin: sessionproof.OriginHook, Generation: reg.Generation, ExecutionID: reg.ExecutionID,
	})
	return reg
}

func compatChangeProcessTuple(t *testing.T, hub *liveCompatHub, req hubclient.SessionRequest, prior hubclient.SessionResponse) {
	t.Helper()
	seed := compatClient(compatHookEnv(t, hub), nil)
	req.Generation = prior.Generation
	req.ClientPIDStartMS++
	reg, err := seed.RegisterSession(t.Context(), req)
	if err != nil || reg.ExecutionID != prior.ExecutionID || reg.Generation != prior.Generation {
		t.Fatalf("hook tuple update = %+v, %v, want %s G%d", reg, err, prior.ExecutionID, prior.Generation)
	}
}

func assertCompatProcessRegistration(t *testing.T, got compatExchange, seed hubclient.SessionRequest, generation, status int) {
	t.Helper()
	var body hubclient.SessionRequest
	if got.method != http.MethodPost || got.uri != "/v1/sessions?process_bound=true" || got.status != status ||
		json.Unmarshal([]byte(got.body), &body) != nil || body.Client != seed.Client || body.Host != seed.Host ||
		body.SessionID != seed.SessionID || body.Generation != generation || body.Source != "shim" ||
		body.ClientPID != seed.ClientPID || body.ClientPIDStartMS != seed.ClientPIDStartMS || body.SuccessionProof {
		t.Fatalf("reattachment = %+v, want process-bound POST /v1/sessions with the original tuple at G%d and status %d", got, generation, status)
	}
}

func compatProcessHeartbeat(execution string, status int) compatExchange {
	return compatExchange{method: http.MethodPost, uri: "/v1/executions/" + execution + "/heartbeat?process_bound=true",
		body: `{"generation":1,"source":"shim","succession_proof":true}`, status: status}
}

func TestCompatM2dONResolverRestartBeforeTick(t *testing.T) {
	bin := compatBuildCurrentBinary(t)
	for _, tc := range []struct {
		name    string
		changed bool
		refresh bool
	}{
		{name: "tick matching tuple"},
		{name: "tick changed tuple", changed: true},
		{name: "refresh matching tuple", refresh: true},
		{name: "refresh changed tuple", changed: true, refresh: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			h := startLiveCompatHubAt(t, bin, dir)
			trace := newCompatTrace(t, h)
			getenv := compatHookEnv(t, h)
			get := func(k string) string {
				if k == "IRIS_HUB_URL" {
					return trace.url
				}
				return getenv(k)
			}
			tokenPath := get("IRIS_TOKEN_FILE")
			cfg := hubclient.Resolved{ConfigFile: filepath.Join(filepath.Dir(tokenPath), "client.json")}
			if err := os.WriteFile(cfg.ConfigFile, []byte(`{"session_succession":true}`), 0o600); err != nil {
				t.Fatal(err)
			}
			self, ok := proc.Lookup(os.Getpid())
			if !ok || self.StartMS <= 0 {
				t.Fatal("could not identify the resolver's fixture process")
			}
			ancestor := proc.Ancestor{PID: self.PID, StartMS: self.StartMS, Name: "cursor-agent"}
			req := hubclient.SessionRequest{Client: "cursor", Host: "compat-host", SessionID: "cursor-session",
				Source: "hook", CWD: t.TempDir(), ClientPID: ancestor.PID, ClientPIDStartMS: ancestor.StartMS, SingleSession: true}
			reg := compatSeedProcessSession(t, h, req, tokenPath)
			sessions := NewSessions(tokenPath)
			resolver := &binding.Resolver{
				Hub: compatClient(get, nil), Client: req.Client, Host: req.Host, CWD: req.CWD,
				Getenv: get, Ancestor: func() (proc.Ancestor, error) { return ancestor, nil },
				Recall: sessions.RecallBindingGeneration, Evidence: sessions, SuccessionEnabled: cfg.SuccessionEnabledContext,
				ParentAlive: func() bool { return true },
			}
			bound, err := resolver.Resolve(t.Context(), "")
			if err != nil || bound.Rung != 3 || bound.ExecutionID != reg.ExecutionID || bound.Generation != reg.Generation || !bound.HookSeen {
				t.Fatalf("initial process binding = %+v, %v", bound, err)
			}
			if initial := trace.snapshot(); len(initial) != 1 || initial[0].uri != "/v1/bind" || initial[0].status != http.StatusOK {
				t.Fatalf("initial lookup traffic = %+v", initial)
			}
			if tc.changed {
				compatChangeProcessTuple(t, h, req, reg)
			}
			restart := func() {
				h.stop()
				h = startLiveCompatHubAt(t, bin, dir)
				trace.useHub(t, h)
			}
			if tc.refresh {
				restart()
				fresh, err := resolver.Resolve(t.Context(), "")
				if tc.changed {
					if !binding.IsCode(err, "lost") {
						t.Fatalf("refresh with changed tuple = %+v, %v", fresh, err)
					}
				} else if err != nil || fresh.ExecutionID != reg.ExecutionID || fresh.Generation != reg.Generation {
					t.Fatalf("refresh did not reattach the exact binding: %+v, %v", fresh, err)
				}
			} else {
				// Stop at the first tick's parent check before the resolver can send its heartbeat.
				tick := 0
				resolver.Maintain(t.Context(), "shim", time.Millisecond, func() bool {
					tick++
					if tick != 1 {
						return false
					}
					restart()
					return true
				}, func() {})
			}
			if resolver.Lost() != tc.changed {
				t.Fatalf("loss state = %t, want %t", resolver.Lost(), tc.changed)
			}
			exchanges := trace.snapshot()
			assertCompatNoSucceed(t, exchanges)
			if len(exchanges) != 3 {
				t.Fatalf("expected initial lookup and two recovery requests, got %+v", exchanges)
			}
			if tc.refresh {
				if got := exchanges[1]; got.method != http.MethodPost || got.uri != "/v1/bind" || got.status != http.StatusConflict {
					t.Fatalf("refresh lookup = %+v", got)
				}
			} else {
				if want := compatProcessHeartbeat(reg.ExecutionID, http.StatusConflict); exchanges[1] != want {
					t.Fatalf("tick heartbeat = %+v, want %+v", exchanges[1], want)
				}
			}
			status := http.StatusOK
			if tc.changed {
				status = http.StatusConflict
			}
			assertCompatProcessRegistration(t, exchanges[2], req, reg.Generation, status)
			compatRequireHubBinding(t, h, req.Host, req.Client, reg, tc.changed)
		})
	}
}

func TestCompatM2dONWakeRestartBeforeHeartbeat(t *testing.T) {
	hubBin := compatBuildCurrentBinary(t)
	wakeBin := compatBuildWakeDriver(t, compatRepoRoot(t), compatHistoricalRef{name: "current"})
	for _, client := range []string{"antigravity", "kimi"} {
		for _, changed := range []bool{false, true} {
			name := client + "/matching tuple"
			if changed {
				name = client + "/changed tuple"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				hubDir := t.TempDir()
				h := startLiveCompatHubAt(t, hubBin, hubDir)
				releaseBind := make(chan struct{})
				release := sync.OnceFunc(func() { close(releaseBind) })
				trace := newCompatTraceWithBindGate(t, h, releaseBind)
				defer release()
				wakeDir := compatWakeConfig(t, h, trace.url, true)
				host, err := os.Hostname()
				if err != nil {
					t.Fatal(err)
				}
				self, ok := proc.Lookup(os.Getpid())
				if !ok || self.StartMS <= 0 {
					t.Fatal("could not identify the wake workers' fixture process")
				}
				req := hubclient.SessionRequest{Client: client, Host: host, SessionID: "125bdacd-42d0-4c18-98aa-1babbf706a1a",
					Source: "hook", CWD: t.TempDir(), ClientPID: self.PID, ClientPIDStartMS: self.StartMS, SingleSession: true}
				reg := compatSeedProcessSession(t, h, req, filepath.Join(wakeDir, "hub.token"))
				driver := compatStartWakeDriver(t, wakeBin, wakeDir, req.CWD, client, req.SessionID)
				initial := trace.wait(t, func(got []compatExchange) bool { return len(got) > 0 })
				if len(initial) != 1 || initial[0].method != http.MethodPost || initial[0].uri != "/v1/bind" || initial[0].status != http.StatusOK {
					t.Fatalf("wake's initial lookup = %+v", initial)
				}
				if changed {
					compatChangeProcessTuple(t, h, req, reg)
				}
				// The old hub has served the initial lookup. Keep that reply in flight
				// until the new process owns the same database and the row is recovering.
				h.stop()
				h = startLiveCompatHubAt(t, hubBin, hubDir)
				trace.useHub(t, h)
				release()

				kimiLost := client == "kimi" && changed
				var attentionProbe compatExchange
				if client == "antigravity" && !changed {
					// Once reattached, the independent attention worker may confirm hook_seen.
					body, err := json.Marshal(map[string]string{"client": client, "execution": reg.ExecutionID, "host": host})
					if err != nil {
						t.Fatal(err)
					}
					attentionProbe = compatExchange{http.MethodPost, "/v1/bind", string(body), http.StatusOK}
				}
				renewal := func(got []compatExchange) []compatExchange { return compatSessionRequests(got, attentionProbe) }
				count := 3
				trace.wait(t, func(got []compatExchange) bool { return len(renewal(got)) >= 3 })
				if kimiLost {
					if out := driver.wait(t); !strings.Contains(out, "compat Kimi wake outcome 14\n") {
						t.Fatalf("Kimi wake did not report Lost: %s", out)
					}
				} else {
					count++
					trace.wait(t, func(got []compatExchange) bool { return len(renewal(got)) >= 4 })
					driver.stop(t)
				}

				exchanges := trace.snapshot()
				assertCompatNoSucceed(t, exchanges)
				sessionRequests := renewal(exchanges)
				if len(sessionRequests) != count {
					t.Fatalf("want %d binding and renewal requests, got %+v", count, sessionRequests)
				}
				if want := compatProcessHeartbeat(reg.ExecutionID, http.StatusConflict); sessionRequests[1] != want {
					t.Fatalf("first wake heartbeat = %+v, want %+v", sessionRequests[1], want)
				}
				status := http.StatusOK
				if changed {
					status = http.StatusConflict
				}
				assertCompatProcessRegistration(t, sessionRequests[2], req, reg.Generation, status)
				if !kimiLost {
					want := compatProcessHeartbeat(reg.ExecutionID, http.StatusOK)
					if changed {
						// Antigravity discards the old binding and its next tick can only
						// look up that tuple. It must not create a replacement session.
						want = initial[0]
						want.status = http.StatusConflict
					}
					if sessionRequests[3] != want {
						t.Fatalf("next wake tick = %+v, want %+v", sessionRequests[3], want)
					}
				}
				compatRequireHubBinding(t, h, req.Host, req.Client, reg, changed)
			})
		}
	}
}

func compatSessionRequests(trace []compatExchange, attentionProbe compatExchange) []compatExchange {
	var requests []compatExchange
	for _, e := range trace {
		if len(requests) >= 3 && e == attentionProbe {
			continue
		}
		if e.uri == "/v1/bind" || strings.HasPrefix(e.uri, "/v1/sessions") || strings.Contains(e.uri, "/heartbeat") {
			requests = append(requests, e)
		}
	}
	return requests
}
