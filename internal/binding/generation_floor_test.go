package binding

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"testing/synctest"
	"time"

	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/proc"
	"github.com/mateusz-klatt/iris/internal/store"
)

func TestRetainedClientsReattachOnlyBeforeTheRecoveryDeadline(t *testing.T) {
	for _, source := range []string{"shim", "watch"} {
		for _, tc := range []struct {
			name    string
			elapsed time.Duration
			lost    bool
		}{
			{"one millisecond before the deadline", 120*time.Second - time.Millisecond, false},
			{"at the deadline", 120 * time.Second, true},
		} {
			t.Run(source+"/"+tc.name, func(t *testing.T) {
				ctx := t.Context()
				h := newRestartableBindingHub(t)
				reg, err := h.store.Call(ctx, "sessions", hookS)
				if err != nil {
					t.Fatal(err)
				}
				r := tickResolver(&hubclient.Client{Config: config{h.srv.URL}}, "claude", sessionS, proc.Ancestor{})
				r.HeartbeatSource = source
				b, err := r.Resolve(ctx, "")
				if err != nil || b.ExecutionID != reg["execution_id"] || b.Generation != 1 {
					t.Fatalf("initial binding = %+v, %v", b, err)
				}
				h.restart()
				h.clock.Advance(tc.elapsed)
				if source == "watch" {
					err = r.Heartbeat(ctx, source)
				} else {
					_, err = r.Resolve(ctx, "")
				}
				if tc.lost {
					if !IsCode(err, "lost") || !r.Lost() || r.bound != (Bound{}) {
						t.Fatalf("at the deadline binding = %+v, loss = %t, error = %v", r.bound, r.Lost(), err)
					}
				} else if err != nil || r.Lost() || r.bound.ExecutionID != b.ExecutionID || r.bound.Generation != b.Generation {
					t.Fatalf("reattached binding = %+v, loss = %t, error = %v", r.bound, r.Lost(), err)
				}
				if beats := h.beats(); len(beats) != 1 || beats[0] != "/v1/executions/"+b.ExecutionID+"/heartbeat" || len(h.sessions()) != 0 {
					t.Fatalf("reattachment used unexpected requests: heartbeats %v, registrations %+v", beats, h.sessions())
				}
			})
		}
	}
}

func TestRetainedGenerationRejectsALiveBindFromOlderBackup(t *testing.T) {
	for _, mode := range []string{"shim", "watch", "codex"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			h := newRestartableBindingHub(t)
			client := "claude"
			if mode == "codex" {
				client = "codex"
			}
			req := store.Request{Client: client, Host: "host", SessionID: "S", Cwd: "/repo", Source: "hook"}
			call := func(operation string, r store.Request) map[string]any {
				t.Helper()
				out, err := h.store.Call(ctx, operation, r)
				if err != nil {
					t.Fatalf("%s: %v", operation, err)
				}
				return out
			}
			original := call("sessions", req)["execution_id"].(string)
			backup := h.backup()
			call("sessions", store.Request{Client: client, Host: "host", SessionID: "S", Cwd: "/repo", Source: "hook", Generation: 1})
			call("end", store.Request{ExecutionID: original, Generation: 1})
			second := call("sessions", req)["execution_id"].(string)
			call("end", store.Request{ExecutionID: second, Generation: 2})
			third := call("sessions", req)["execution_id"].(string)

			r := tickResolver(&hubclient.Client{Config: config{h.srv.URL}}, client, sessionS, proc.Ancestor{})
			if mode == "watch" {
				r.HeartbeatSource = "watch"
			}
			// A stale disk entry must not override the higher generation the process retained.
			r.Recall = func(string, string) (int, int, error) { return 1, 0, nil }
			resolve := func(r *Resolver) (Bound, error) {
				if mode == "codex" {
					b, primary, err := r.ResolveCall(ctx, "", "S", "S")
					if !primary {
						t.Fatal("root lost its binding mode")
					}
					return b, err
				}
				return r.Resolve(ctx, "")
			}
			if b, err := resolve(r); err != nil || b.Generation != 3 || b.ExecutionID != third {
				t.Fatalf("generation 3 binding = %+v, %v", b, err)
			}
			h.restore(backup)
			if b, err := resolve(r); b != (Bound{}) || !IsCode(err, "lost") || !r.Lost() {
				t.Fatalf("recovering generation 1 used stale disk proof: %+v, %v", b, err)
			}
			call("heartbeat", store.Request{ExecutionID: original, Generation: 1, Source: "shim"})
			if b, err := resolve(r); b != (Bound{}) || !errors.Is(err, errGenerationFloor) || !r.Lost() {
				t.Fatalf("live generation 1 displaced generation 3: %+v, %v", b, err)
			}

			fresh := tickResolver(r.Hub, client, sessionS, proc.Ancestor{})
			fresh.HeartbeatSource = r.HeartbeatSource
			fresh.Recall = func(string, string) (int, int, error) { return 3, 0, nil }
			if b, err := resolve(fresh); b != (Bound{}) || !errors.Is(err, errGenerationFloor) {
				t.Fatalf("live generation 1 bypassed fresh client's disk proof: %+v, %v", b, err)
			}
			if requests := h.sessions(); len(requests) != 0 {
				t.Fatalf("client registered with stale proof: %+v", requests)
			}
			if beats := h.beats(); len(beats) != 1 || beats[0] != "/v1/executions/"+third+"/heartbeat" {
				t.Fatalf("client renewed a restored generation: %v", beats)
			}
		})
	}
}

func TestNewDiskProofStopsCachedGenerationRenewal(t *testing.T) {
	for _, mode := range []string{"shim", "watch"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			h := &tickHub{reply: func(*http.Request, map[string]any) (int, any) {
				return found("ex_restored", 1, "S")
			}}
			r := tickResolver(h.client(), "claude", sessionS, proc.Ancestor{})
			gen := 1
			r.Recall = func(string, string) (int, int, error) { return gen, 0, nil }
			if b, err := r.Resolve(ctx, ""); err != nil || b.ExecutionID != "ex_restored" || b.Generation != 1 {
				t.Fatalf("initial binding = %+v, %v", b, err)
			}
			gen = 3
			if mode == "watch" {
				r.HeartbeatSource = "watch"
				if err := r.Heartbeat(ctx, "watch"); !errors.Is(err, errGenerationFloor) {
					t.Fatalf("watch renewed generation 1: %v", err)
				}
				h.expect(t, "bind session_id=S")
			} else {
				if _, err := r.Resolve(ctx, ""); !errors.Is(err, errGenerationFloor) {
					t.Fatalf("shim refreshed generation 1: %v", err)
				}
				h.expect(t, "bind session_id=S", "bind execution=ex_restored", "bind session_id=S")
			}
			if r.bound != (Bound{}) || !r.Lost() {
				t.Fatalf("generation 1 stayed bound: %+v, lost=%v", r.bound, r.Lost())
			}
		})
	}
}

func TestEndedThroughVetoesCachedHeartbeat(t *testing.T) {
	for _, source := range []string{"shim", "watch"} {
		t.Run(source, func(t *testing.T) {
			ended := 0
			h := &tickHub{reply: func(_ *http.Request, body map[string]any) (int, any) {
				if body["execution"] != nil {
					return refused("lost")
				}
				return found("ex_restored", 3, "S")
			}}
			r := tickResolver(h.client(), "claude", sessionS, proc.Ancestor{})
			r.HeartbeatSource = source
			r.Recall = func(string, string) (int, int, error) { return 0, ended, nil }
			if b, err := r.Resolve(t.Context(), ""); err != nil || b.Generation != 3 {
				t.Fatalf("initial bind: %+v, %v", b, err)
			}
			ended = 3
			if source == "shim" {
				if b, err := r.Resolve(t.Context(), ""); b != (Bound{}) || !errors.Is(err, errGenerationFloor) {
					t.Fatalf("refresh used an ended generation: %+v, %v", b, err)
				}
				h.expect(t, "bind session_id=S", "bind execution=ex_restored", "bind session_id=S")
			} else {
				if err := r.Heartbeat(t.Context(), source); !errors.Is(err, errGenerationFloor) {
					t.Fatalf("heartbeat used an ended generation: %v", err)
				}
				h.expect(t, "bind session_id=S")
			}
			if !r.Lost() || r.bound != (Bound{}) {
				t.Fatalf("ended binding was retained: lost=%t, bound=%+v", r.Lost(), r.bound)
			}
		})
	}
}

func TestEndedThroughVetoesExplicitAndForeignSessionBindings(t *testing.T) {
	h := &tickHub{reply: func(_ *http.Request, body map[string]any) (int, any) {
		if body["session_id"] == "native" {
			return refused("unbound")
		}
		return found("ex_restored", 2, "ended")
	}}
	recall := func(client, session string) (int, int, error) {
		if client != "codex" || session != "ended" {
			t.Errorf("checked wrong end marker: %s/%s", client, session)
		}
		return 0, 3, nil
	}
	override := tickResolver(h.client(), "codex", map[string]string{"IRIS_EXECUTION": "ex_restored"}, proc.Ancestor{})
	override.Recall = recall
	if b, _, err := override.ResolveCall(t.Context(), "", "", ""); b != (Bound{}) || !errors.Is(err, errGenerationFloor) {
		t.Fatalf("explicit override used a generation below the end: %+v, %v", b, err)
	}
	lookup := tickResolver(h.client(), "codex", nil, proc.Ancestor{})
	lookup.Recall = recall
	if _, _, err := lookup.ResolveCall(t.Context(), "", "native", "child"); !errors.Is(err, errUnusable) {
		t.Fatalf("unbound native lookup did not enter lookup-only mode: %v", err)
	}
	if b, _, err := lookup.ResolveCall(t.Context(), "", "ended", "child"); b != (Bound{}) || !errors.Is(err, errGenerationFloor) {
		t.Fatalf("foreign lookup used a generation below the end: %+v, %v", b, err)
	}
	h.expect(t, "bind execution=ex_restored", "bind session_id=native", "bind session_id=ended")
}

func TestLiveBindingRejectsUnreadableGenerationProof(t *testing.T) {
	for _, tc := range []struct {
		name, client, source, thread string
		primary                      bool
	}{
		{"shim", "claude", "shim", "", true},
		{"watch", "claude", "watch", "", true},
		{"Codex root", "codex", "shim", "S", true},
		{"Codex native sub-agent", "codex", "shim", "child", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &tickHub{reply: func(*http.Request, map[string]any) (int, any) {
				return found("ex_restored", 1, "S")
			}}
			r := tickResolver(h.client(), tc.client, sessionS, proc.Ancestor{})
			r.HeartbeatSource = tc.source
			proofErr := errors.New("cannot read generation proof")
			r.Recall = func(client, session string) (int, int, error) {
				if client != tc.client || session != "S" {
					t.Errorf("read unrelated proof for %s/%s", client, session)
				}
				return 0, 0, proofErr
			}
			primary := true
			var b Bound
			var err error
			if tc.client == "codex" {
				b, primary, err = r.ResolveCall(t.Context(), "", "S", tc.thread)
			} else {
				b, err = r.Resolve(t.Context(), "")
			}
			if b != (Bound{}) || primary != tc.primary || !errors.Is(err, errMalformedGeneration) {
				t.Fatalf("unreadable proof allowed a binding: %+v, primary=%v, err=%v", b, primary, err)
			}
			if r.bound != (Bound{}) || r.last != (Bound{}) {
				t.Fatalf("unreadable proof left a cached binding: bound=%+v last=%+v", r.bound, r.last)
			}
			h.expect(t, "bind session_id=S")
		})
	}
}

func TestCachedBindingCanReattachAfterATransientProofReadError(t *testing.T) {
	for _, source := range []string{"shim", "watch"} {
		t.Run(source, func(t *testing.T) {
			recovering := false
			h := &tickHub{reply: func(req *http.Request, _ map[string]any) (int, any) {
				if req.URL.Path == "/v1/bind" {
					if recovering {
						return refused("lost")
					}
					return found("ex_live", 3, "S")
				}
				recovering = false
				return leaseGranted()
			}}
			r := tickResolver(h.client(), "claude", sessionS, proc.Ancestor{})
			r.HeartbeatSource = source
			var readErr error
			r.Recall = func(string, string) (int, int, error) { return 3, 0, readErr }
			b, err := r.Resolve(t.Context(), "")
			if err != nil || b.ExecutionID != "ex_live" || b.Generation != 3 {
				t.Fatalf("initial binding = %+v, %v", b, err)
			}

			recovering = true
			readErr = errors.New("temporary read error")
			renew := func() error {
				if source == "watch" {
					return r.Heartbeat(t.Context(), source)
				}
				_, err := r.Resolve(t.Context(), "")
				return err
			}
			if err := renew(); !errors.Is(err, errMalformedGeneration) || !recovering || r.Lost() || r.gone[b.ExecutionID] {
				t.Fatalf("unverifiable proof renewed or lost the Execution: recovering=%v, lost=%v, gone=%v, err=%v", recovering, r.Lost(), r.gone, err)
			}
			readErr = nil
			if err := renew(); err != nil || recovering || r.bound != b {
				t.Fatalf("valid proof could not reattach the exact Execution: recovering=%v, bound=%+v, err=%v", recovering, r.bound, err)
			}
			if source == "watch" {
				h.expect(t, "bind session_id=S", "beat ex_live 3 watch")
			} else {
				h.expect(t, "bind session_id=S", "bind execution=ex_live", "bind execution=ex_live", "beat ex_live 3 shim", "bind execution=ex_live")
			}
		})
	}
}

func TestFirstFallbackChecksGenerationProofAfterGrace(t *testing.T) {
	for _, mode := range []string{"shim", "watch"} {
		for _, proof := range []string{"absent", "remembered", "ended", "malformed"} {
			t.Run(mode+"/"+proof, func(t *testing.T) {
				cwd := t.TempDir()
				synctest.Test(t, func(t *testing.T) {
					var lookups int
					h := &tickHub{reply: func(req *http.Request, body map[string]any) (int, any) {
						if req.URL.Path == "/v1/bind" {
							lookups++
							return refused("unbound")
						}
						if body["session_id"] != "S" || body["source"] != "mcp" || body["generation"] != float64(0) {
							t.Errorf("unexpected fallback request: %+v", body)
						}
						return found("ex_new", 1, "S")
					}}
					r := tickResolver(h.client(), "claude", sessionS, proc.Ancestor{})
					r.CWD, r.Grace = cwd, 100*time.Millisecond
					if mode == "watch" {
						r.HeartbeatSource = "watch"
					}
					var refusals []bool
					r.ObserveBindingRefusal = func(client, session string, ended bool) {
						if client != "claude" || session != "S" {
							t.Errorf("reported a refusal for %s/%s", client, session)
						}
						refusals = append(refusals, ended)
					}
					r.Recall = func(client, session string) (int, int, error) {
						if client != "claude" || session != "S" {
							t.Errorf("recall for %s/%s", client, session)
						}
						if lookups == 2 {
							switch proof {
							case "remembered":
								return 3, 0, nil
							case "ended":
								return 0, 3, nil
							case "malformed":
								return 0, 0, errors.New("corrupt file")
							}
						}
						return 0, 0, nil
					}
					b, err := r.Resolve(context.Background(), "")
					switch proof {
					case "absent":
						if err != nil || b.ExecutionID != "ex_new" || b.Generation != 1 {
							t.Fatalf("fresh session could not bootstrap: %+v, %v", b, err)
						}
						h.expect(t, "bind session_id=S", "bind session_id=S", "/v1/sessions/outside")
					case "remembered", "ended", "malformed":
						if b != (Bound{}) || !IsCode(err, "lost") {
							t.Fatalf("session with %s proof bootstrapped: %+v, %v", proof, b, err)
						}
						h.expect(t, "bind session_id=S", "bind session_id=S")
					}
					if proof == "remembered" || proof == "ended" {
						if len(refusals) != 1 || refusals[0] != (proof == "ended") {
							t.Fatalf("fallback did not report %s proof: ended refusals %v", proof, refusals)
						}
					} else if len(refusals) != 0 {
						t.Fatalf("fallback reported a readable proof refusal for %s proof: %v", proof, refusals)
					}
				})
			})
		}
	}
}

func TestWatchDoesNotRegisterToReattachFromDisk(t *testing.T) {
	h := newRestartableBindingHub(t)
	ctx := context.Background()
	reg, err := h.store.Call(ctx, "sessions", hookS)
	if err != nil {
		t.Fatal(err)
	}
	h.restart()
	r := tickResolver(&hubclient.Client{Config: config{h.srv.URL}}, "claude", sessionS, proc.Ancestor{})
	r.HeartbeatSource = "watch"
	r.Recall = func(string, string) (int, int, error) { return 1, 0, nil }
	if b, err := r.Resolve(ctx, ""); b != (Bound{}) || !errors.Is(err, errGenerationFloor) {
		t.Fatalf("watch reattached from disk: %+v, %v", b, err)
	}
	if len(h.sessions()) != 0 || len(h.beats()) != 0 {
		t.Fatalf("watch tried to reattach from disk: sessions=%+v, heartbeats=%v", h.sessions(), h.beats())
	}
	if _, err := h.store.Call(ctx, "sessions", store.Request{Client: "claude", Host: "host", SessionID: "S", Cwd: "/repo", Source: "hook", Generation: 1}); err != nil {
		t.Fatal(err)
	}
	if b, err := r.Resolve(ctx, ""); err != nil || b.ExecutionID != reg["execution_id"] {
		t.Fatalf("watch did not follow the hook's reattachment: %+v, %v", b, err)
	}
}

func TestBindByKnownSessionChecksProofWhenOlderHubOmitsSessionID(t *testing.T) {
	h := &tickHub{reply: func(*http.Request, map[string]any) (int, any) {
		return http.StatusOK, json.RawMessage(`{"execution_id":"ex_old","generation":1}`)
	}}
	for _, client := range []string{"claude", "codex"} {
		r := tickResolver(h.client(), client, sessionS, proc.Ancestor{})
		r.Recall = func(client, session string) (int, int, error) {
			if session != "S" {
				t.Fatalf("hub omitted session id and lookup forgot it: %s/%s", client, session)
			}
			return 3, 0, nil
		}
		var b Bound
		var err error
		if client == "codex" {
			b, _, err = r.ResolveCall(context.Background(), "", "S", "S")
		} else {
			b, err = r.Resolve(context.Background(), "")
		}
		if b != (Bound{}) || !errors.Is(err, errGenerationFloor) {
			t.Fatalf("%s bound lower generation: %+v, %v", client, b, err)
		}
	}
}

func TestCodexSubagentChecksNativeSessionProofWithOneLookup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		generation := 1
		h := &tickHub{reply: func(_ *http.Request, body map[string]any) (int, any) {
			if body["session_id"] == "foreign" {
				return found("ex_foreign", 1, "foreign")
			}
			// An older hub can omit the session id. The metadata still identifies the file.
			return http.StatusOK, map[string]any{"execution_id": "ex_native", "generation": generation}
		}}
		r := tickResolver(h.client(), "codex", nil, proc.Ancestor{})
		r.Recall = func(client, session string) (int, int, error) {
			if client != "codex" || (session != "native" && session != "foreign") {
				t.Errorf("read unrelated proof for %s/%s", client, session)
			}
			return 3, 0, nil
		}
		if b, primary, err := r.ResolveCall(ctx, "ignored", "native", "child"); b != (Bound{}) || primary || !errors.Is(err, errGenerationFloor) {
			t.Fatalf("native lookup used generation below disk proof: %+v, primary=%v, err=%v", b, primary, err)
		}
		if b, primary, err := r.ResolveCall(ctx, "ignored", "foreign", "child"); b.ExecutionID != "ex_foreign" || primary || err != nil {
			t.Fatalf("foreign lookup changed: %+v, primary=%v, err=%v", b, primary, err)
		}

		generation = 3
		r.mu.Lock()
		defer r.mu.Unlock()
		done := make(chan struct{})
		go func() {
			defer close(done)
			if b, primary, err := r.ResolveCall(ctx, "ignored", "native", "child"); b.ExecutionID != "ex_native" || b.Generation != 3 || primary || err != nil {
				t.Errorf("eligible native lookup: %+v, primary=%v, err=%v", b, primary, err)
			}
		}()
		synctest.Wait()
		select {
		case <-done:
		default:
			t.Fatal("native lookup waited for the binding lock")
		}
		if r.bound != (Bound{}) || r.last != (Bound{}) || r.lost || len(r.gone) != 0 {
			t.Fatalf("sub-agent lookup changed cached binding: bound=%+v last=%+v lost=%v gone=%v", r.bound, r.last, r.lost, r.gone)
		}
		h.expect(t, "bind session_id=native", "bind session_id=foreign", "bind session_id=native")
	})
}

func TestAnExplicitOverrideCannotBindItsOwnLowerGeneration(t *testing.T) {
	h := &tickHub{reply: func(*http.Request, map[string]any) (int, any) {
		return found("ex_restored", 1, "S")
	}}
	r := tickResolver(h.client(), "claude", map[string]string{"IRIS_EXECUTION": "ex_restored", "CLAUDE_CODE_SESSION_ID": "S"}, proc.Ancestor{})
	r.Recall = func(string, string) (int, int, error) { return 3, 0, nil }
	if b, primary, err := r.ResolveCall(context.Background(), "", "", ""); b != (Bound{}) || !primary || !errors.Is(err, errGenerationFloor) {
		t.Fatalf("override bound its own lower generation: %+v, %v", b, err)
	}
	h.expect(t, "bind execution=ex_restored")
}

func TestExplicitBindingRetainsProofForTheProcessesOwnSession(t *testing.T) {
	ctx := context.Background()
	rolledBack := false
	h := &tickHub{reply: func(req *http.Request, body map[string]any) (int, any) {
		if !rolledBack {
			return found("ex_current", 3, "S")
		}
		if req.URL.Path != "/v1/bind" || body["execution"] != nil {
			return refused("lost")
		}
		return found("ex_restored", 1, "S")
	}}
	r := tickResolver(h.client(), "claude", map[string]string{"IRIS_EXECUTION": "ex_current", "CLAUDE_CODE_SESSION_ID": "S"}, proc.Ancestor{})
	if b, primary, err := r.ResolveCall(ctx, "", "", ""); err != nil || !primary || b.Generation != 3 || b.Rung != 1 {
		t.Fatalf("initial override: %+v, %v", b, err)
	}
	rolledBack = true
	if b, _, err := r.ResolveCall(ctx, "", "", ""); b != (Bound{}) || !errors.Is(err, errGenerationFloor) || !r.Lost() {
		t.Fatalf("loss forgot process generation: %+v, %v", b, err)
	}
	h.expect(t, "bind execution=ex_current", "bind execution=ex_current", "beat ex_current 3 shim", "bind session_id=S")
}

func TestCachedHeartbeatsCheckProofAgainAfterAReply(t *testing.T) {
	for _, mode := range []string{"shim refresh", "watch heartbeat", "shim refuses recovery"} {
		t.Run(mode, func(t *testing.T) {
			gen := 1
			if mode == "shim refuses recovery" {
				gen = 3
			}
			h := &tickHub{reply: func(req *http.Request, body map[string]any) (int, any) {
				if req.URL.Path != "/v1/bind" {
					gen = 3
					return leaseGranted()
				}
				if body["execution"] == "ex_restored" {
					return refused("lost")
				}
				return found("ex_restored", 1, "S")
			}}
			r := tickResolver(h.client(), "claude", sessionS, proc.Ancestor{})
			r.bound = Bound{ExecutionID: "ex_restored", Generation: 1, SessionID: "S", Rung: 2, HookSeen: true}
			r.Recall = func(string, string) (int, int, error) { return gen, 0, nil }
			ctx := context.Background()
			if mode == "watch heartbeat" {
				if err := r.Heartbeat(ctx, "watch"); !errors.Is(err, errGenerationFloor) {
					t.Fatalf("watch accepted reply below delayed proof: %v", err)
				}
				h.expect(t, "beat ex_restored 1 watch")
			} else {
				if _, err := r.Resolve(ctx, ""); !errors.Is(err, errGenerationFloor) {
					t.Fatalf("shim accepted reply below delayed proof: %v", err)
				}
				if mode == "shim refresh" {
					h.expect(t, "bind execution=ex_restored", "beat ex_restored 1 shim", "bind session_id=S")
				} else {
					h.expect(t, "bind execution=ex_restored", "bind session_id=S")
				}
			}
			if r.bound != (Bound{}) || !r.Lost() {
				t.Fatalf("response installed an obsolete binding: %+v", r.bound)
			}
		})
	}
}

func TestDelayedRegistrationDoesNotPublishALowerBinding(t *testing.T) {
	for _, mode := range []string{"legacy route", "fallback reply", "reattach reply"} {
		t.Run(mode, func(t *testing.T) {
			gen := 0
			if mode == "reattach reply" {
				gen = 1
			}
			h := &tickHub{reply: func(req *http.Request, body map[string]any) (int, any) {
				if req.URL.Path == "/v1/bind" {
					return refused("unbound")
				}
				gen = 3
				if mode == "legacy route" {
					return http.StatusNotFound, map[string]any{"error": map[string]any{"code": "not_found", "message": "unknown API route"}}
				}
				return found("ex_restored", 1, "S")
			}}
			r := tickResolver(h.client(), "claude", sessionS, proc.Ancestor{})
			r.CWD = t.TempDir()
			r.Recall = func(string, string) (int, int, error) { return gen, 0, nil }
			if b, err := r.Resolve(context.Background(), ""); b != (Bound{}) || !errors.Is(err, errGenerationFloor) || r.own {
				t.Fatalf("delayed registration published a lower binding: %+v, %v", b, err)
			}
			want := "/v1/sessions/outside"
			if mode == "reattach reply" {
				want = "/v1/sessions"
			}
			h.expect(t, "bind session_id=S", want)
		})
	}
}

func TestAYoungerProofStopsPendingHandoverToOldTarget(t *testing.T) {
	ctx := context.Background()
	h := &tickHub{reply: func(req *http.Request, body map[string]any) (int, any) {
		if req.URL.Path == "/v1/bind" {
			return found("ex_hook", 1, "S")
		}
		return refused("unavailable")
	}}
	r := tickResolver(h.client(), "claude", sessionS, proc.Ancestor{})
	gen := 1
	r.Recall = func(string, string) (int, int, error) { return gen, 0, nil }
	r.bound, r.own = Bound{ExecutionID: "ex_fallback", Generation: 1, Rung: 3, SessionID: "fallback"}, true
	r.adopt(ctx)
	if r.pending == nil || r.bound.ExecutionID != "ex_hook" {
		t.Fatalf("handover did not become pending: %+v", r.bound)
	}
	gen = 3
	r.adopt(ctx)
	if r.pending != nil || r.bound != (Bound{}) || !r.Lost() {
		t.Fatalf("handover retained stale destination: %+v", r.bound)
	}
	h.expect(t, "bind session_id=S exclude=ex_fallback", "/v1/executions/ex_fallback/end")
}

func TestPendingHandoverWaitsForReadableProof(t *testing.T) {
	h := &tickHub{reply: func(req *http.Request, body map[string]any) (int, any) {
		if body["hand_over"] != "ex_hook" || body["generation"] != float64(1) {
			t.Errorf("handover lost its original source or destination: %s %+v", req.URL.Path, body)
		}
		return leaseGranted()
	}}
	r := tickResolver(h.client(), "claude", sessionS, proc.Ancestor{})
	readErr := errors.New("temporary read error")
	r.Recall = func(string, string) (int, int, error) { return 3, 0, readErr }
	r.bound = Bound{ExecutionID: "ex_hook", Generation: 3, Rung: 2, SessionID: "S"}
	pending := &handover{from: Bound{ExecutionID: "ex_fallback", Generation: 1}, to: r.bound}
	r.pending = pending

	r.adopt(t.Context())
	if r.pending != pending || r.bound != pending.to || r.Lost() {
		t.Fatalf("unreadable proof lost the pending handover: pending=%+v, bound=%+v, lost=%v", r.pending, r.bound, r.Lost())
	}
	h.expect(t)

	readErr = nil
	r.adopt(t.Context())
	if r.pending != nil || r.bound != pending.to || r.Lost() {
		t.Fatalf("readable proof did not settle the pending handover: pending=%+v, bound=%+v, lost=%v", r.pending, r.bound, r.Lost())
	}
	h.expect(t, "/v1/executions/ex_fallback/end")
}

func TestLookupOnlyWakeRejectsLowerProcessGeneration(t *testing.T) {
	ctx := context.Background()
	gen := 3
	h := &tickHub{reply: func(*http.Request, map[string]any) (int, any) {
		if gen == 3 {
			return found("ex_current", 3, "S")
		}
		return found("ex_restored", 1, "S")
	}}
	r := NewAntigravityWake(h.client(), "S", proc.Ancestor{PID: 42, StartMS: 7, Name: "agy"})
	first, ok := r.LookupWake(ctx)
	if !ok || first.Bound.Generation != 3 {
		t.Fatalf("wake initial binding: %v, %+v", ok, first)
	}
	r.markLostHeld(first.Bound, false)
	gen = 1
	if next, ok := r.LookupWake(ctx); ok || next != (WakeTarget{}) {
		t.Fatalf("wake bound a lower restored generation: %v, %+v", ok, next)
	}
	h.expect(t, "bind client_pid=42 client_pid_start_ms=7", "bind client_pid=42 client_pid_start_ms=7")
}

func TestCodexWakeChecksProofAgainBeforeStarting(t *testing.T) {
	ctx := context.Background()
	h := &tickHub{reply: func(*http.Request, map[string]any) (int, any) {
		return found("ex_restored", 1, "S")
	}}
	r := tickResolver(h.client(), "codex", nil, proc.Ancestor{})
	gen := 1
	r.Recall = func(string, string) (int, int, error) { return gen, 0, nil }
	if _, _, err := r.ResolveCall(ctx, "", "S", "S"); err != nil {
		t.Fatal(err)
	}
	target, ok := r.Primary()
	if !ok {
		t.Fatal("could not arm an eligible binding")
	}
	gen = 3
	if ok, err := r.StartIfPrimary(target, func() error { t.Fatal("stale generation started wake"); return nil }); ok || err != nil {
		t.Fatalf("wake admission with lower generation: %t, %v", ok, err)
	}
	h.expect(t, "bind session_id=S")
}
