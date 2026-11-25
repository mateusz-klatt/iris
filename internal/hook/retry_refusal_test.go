package hook

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func runBounded(t *testing.T, r Runner) Output {
	t.Helper()
	done := make(chan Output, 1)
	go func() {
		done <- r.Run()
	}()
	select {
	case out := <-done:
		return out
	case <-time.After(10 * time.Second):
		t.Fatal("Runner.Run did not finish within 10s")
		return Output{}
	}
}

func TestTouchRetryRefusalDeniesAndObserves(t *testing.T) {
	const hubSecret = "SECRET HUB MESSAGE"
	type wantOutput struct {
		stdout, stderr string
		exit           int
	}
	wantFor := func(c Client, reason string) wantOutput {
		switch c {
		case ClientClaude, ClientCodex:
			return wantOutput{
				stdout: fmt.Sprintf(`{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":%q}}`+"\n", reason),
			}
		case ClientCopilot:
			return wantOutput{
				stdout: fmt.Sprintf(`{"permissionDecision":"deny","permissionDecisionReason":%q}`+"\n", reason),
			}
		case ClientCursor:
			return wantOutput{
				stdout: fmt.Sprintf(`{"permission":"deny","user_message":%q,"agent_message":%q}`+"\n", reason, reason),
			}
		case ClientAntigravity:
			return wantOutput{
				stdout: fmt.Sprintf(`{"decision":"deny","reason":%q}`+"\n", reason),
			}
		case ClientGrok:
			return wantOutput{
				stdout: fmt.Sprintf(`{"decision":"deny","reason":%q}`+"\n", reason),
				exit:   2,
			}
		case ClientKimi:
			return wantOutput{
				stderr: reason + "\n",
				exit:   2,
			}
		default:
			t.Fatalf("unexpected client %q", c)
			return wantOutput{}
		}
	}

	routes := []struct {
		name         string
		capabilities string
		wantPath     string
	}{
		{name: "touch", capabilities: `["shell_scope"]`, wantPath: "/v1/claims/touch"},
		{name: "touch_files", capabilities: `["shell_scope","file_claims"]`, wantPath: "/v1/claims/touch/files"},
	}
	codes := []struct {
		code, wantReason, wantObs string
	}{
		{
			code:       "key_reused",
			wantReason: "Iris refused this edit because this tool call ID was already used for a different claim request. Retry the edit as a new tool call.",
			wantObs:    "pre-edit claim refused (key_reused)",
		},
		{
			code:       "lost",
			wantReason: "Iris refused this edit because the session or its earlier claim is no longer current. Retry the edit as a new tool call. Run `iris doctor` if this repeats.",
			wantObs:    "pre-edit claim refused (lost)",
		},
	}

	for _, route := range routes {
		for _, tc := range codes {
			for _, c := range Clients {
				t.Run(route.name+"/"+tc.code+"/"+string(c), func(t *testing.T) {
					var mu sync.Mutex
					var touchPaths []string
					srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						switch r.URL.Path {
						case "/v1/sessions":
							_, _ = fmt.Fprintf(w, `{"execution_id":"ex_1","generation":1,"handle":"ex_1","capabilities":%s}`, route.capabilities)
						case "/v1/claims/touch", "/v1/claims/touch/files":
							mu.Lock()
							touchPaths = append(touchPaths, r.URL.Path)
							mu.Unlock()
							w.WriteHeader(http.StatusConflict)
							_, _ = fmt.Fprintf(w, `{"error":{"code":%q,"message":%q}}`, tc.code, hubSecret)
						default:
							t.Errorf("unexpected path %s", r.URL.Path)
							w.WriteHeader(http.StatusNotFound)
						}
					}))
					r := editRunner(t, c, srv.URL)
					t.Cleanup(srv.Close)
					token := r.Getenv("IRIS_TOKEN_FILE")
					out := runBounded(t, r)

					mu.Lock()
					gotPaths := append([]string(nil), touchPaths...)
					mu.Unlock()
					if len(gotPaths) != 1 || gotPaths[0] != route.wantPath {
						t.Fatalf("touch paths = %v, want [%s]", gotPaths, route.wantPath)
					}

					want := wantFor(c, tc.wantReason)
					if out.Exit != want.exit || string(out.Stdout) != want.stdout || string(out.Stderr) != want.stderr {
						t.Fatalf("got stdout=%q stderr=%q exit=%d; want stdout=%q stderr=%q exit=%d",
							out.Stdout, out.Stderr, out.Exit, want.stdout, want.stderr, want.exit)
					}
					if strings.Contains(string(out.Stdout), hubSecret) || strings.Contains(string(out.Stderr), hubSecret) {
						t.Fatalf("hub message leaked: stdout=%q stderr=%q", out.Stdout, out.Stderr)
					}

					obs := observedOnly(t, token)
					if !obs.Observed() || obs.Degraded() || obs.Error != tc.wantObs || obs.Client != string(c) || obs.Event != "PreToolUse" {
						t.Fatalf("unexpected observation %+v", obs)
					}
					if strings.Contains(obs.Error, hubSecret) {
						t.Fatalf("hub message leaked to observation: %+v", obs)
					}
				})
			}
		}
	}
}

func TestTouchRefusalWithUnverifiedNamespace(t *testing.T) {
	const hubSecret = "SECRET HUB MESSAGE"
	for _, tc := range []struct {
		name, code string
		missing    bool
		deny       bool
	}{
		{name: "key_reused/mismatched", code: "key_reused"},
		{name: "key_reused/missing", code: "key_reused", missing: true},
		{name: "lost/mismatched", code: "lost", deny: true},
		{name: "lost/missing", code: "lost", missing: true, deny: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v1/sessions":
					_, _ = w.Write([]byte(`{"execution_id":"ex_1","generation":1,"handle":"ex_1"}`))
				case "/v1/claims/touch":
					w.WriteHeader(http.StatusConflict)
					_, _ = fmt.Fprintf(w, `{"error":{"code":%q,"message":%q}}`, tc.code, hubSecret)
				default:
					t.Errorf("unexpected path %s", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			t.Cleanup(srv.Close)

			payload := editPayload(ClientCursor, gitRepo(t))
			if tc.missing {
				payload = strings.Replace(payload, `,"conversation_id":"s1"`, "", 1)
			} else {
				payload = strings.Replace(payload, `"conversation_id":"s1"`, `"conversation_id":"different"`, 1)
			}
			r := editRunner(t, ClientCursor, srv.URL)
			r.Stdin = strings.NewReader(payload)
			token := r.Getenv("IRIS_TOKEN_FILE")
			out := runBounded(t, r)
			if strings.Contains(string(out.Stdout), hubSecret) || strings.Contains(string(out.Stderr), hubSecret) {
				t.Fatalf("hub message leaked: stdout=%q stderr=%q", out.Stdout, out.Stderr)
			}
			if tc.deny {
				want := Encode(Event{Client: ClientCursor, Kind: KindPreTool}, Decision{Action: ActionDeny, Reason: touchLostRefused})
				if out.Exit != want.Exit || string(out.Stdout) != string(want.Stdout) || string(out.Stderr) != string(want.Stderr) {
					t.Fatalf("lost output stdout=%q stderr=%q exit=%d, want %q and exit %d", out.Stdout, out.Stderr, out.Exit, want.Stdout, want.Exit)
				}
				obs := observedOnly(t, token)
				if obs.Error != obsTouchLost || strings.Contains(obs.Error, hubSecret) {
					t.Fatalf("observation = %+v", obs)
				}
				return
			}

			assertAllow(t, ClientCursor, out)
			failure := waitForFailure(t, token)
			if failure.Stage != "touch" || failure.Error != "hub request failed: key_reused" || strings.Contains(failure.Error, hubSecret) {
				t.Fatalf("failure = %+v", failure)
			}
			observations, err := ReadRecentObservations(token, 10)
			if err != nil || len(observations) != 1 || observations[0].Error != obsTouchKeyReused || strings.Contains(observations[0].Error, hubSecret) {
				t.Fatalf("observations = %+v, err = %v", observations, err)
			}
		})
	}
}

func TestTouchFailOpenControlsAndDeadline(t *testing.T) {
	const hubSecret = "SECRET HUB MESSAGE"
	cases := []struct {
		name      string
		wantError string
		handle    func(t *testing.T, w http.ResponseWriter)
	}{
		{
			name:      "bare-409",
			wantError: "hub answered HTTP 409",
			handle: func(_ *testing.T, w http.ResponseWriter) {
				w.WriteHeader(http.StatusConflict)
			},
		},
		{
			name:      "malformed-409",
			wantError: "hub answered HTTP 409",
			handle: func(_ *testing.T, w http.ResponseWriter) {
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte(`{"error":`))
			},
		},
		{
			name:      "typed-409-conflict",
			wantError: "hub request failed: conflict",
			handle: func(_ *testing.T, w http.ResponseWriter) {
				w.WriteHeader(http.StatusConflict)
				_, _ = fmt.Fprintf(w, `{"error":{"code":"conflict","message":%q}}`, hubSecret)
			},
		},
		{
			name:      "503-lost-envelope",
			wantError: "hub request failed: lost",
			handle: func(_ *testing.T, w http.ResponseWriter) {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = fmt.Fprintf(w, `{"error":{"code":"lost","message":%q}}`, hubSecret)
			},
		},
		{
			name:      "503-key-reused-envelope",
			wantError: "hub request failed: key_reused",
			handle: func(_ *testing.T, w http.ResponseWriter) {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = fmt.Fprintf(w, `{"error":{"code":"key_reused","message":%q}}`, hubSecret)
			},
		},
		{
			name:      "transport-error",
			wantError: "incomplete hook input or hub response",
			handle: func(t *testing.T, w http.ResponseWriter) {
				hj, ok := w.(http.Hijacker)
				if !ok {
					t.Fatal("ResponseWriter does not support Hijack")
				}
				conn, _, err := hj.Hijack()
				if err != nil {
					t.Fatal(err)
				}
				_ = conn.Close()
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v1/sessions":
					_, _ = w.Write([]byte(`{"execution_id":"ex_1","generation":1,"handle":"ex_1"}`))
				case "/v1/claims/touch":
					tc.handle(t, w)
				default:
					t.Errorf("unexpected path %s", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			r := editRunner(t, ClientClaude, srv.URL)
			t.Cleanup(srv.Close)
			token := r.Getenv("IRIS_TOKEN_FILE")
			out := runBounded(t, r)
			assertAllow(t, ClientClaude, out)

			got := waitForFailure(t, token)
			if got.Stage != "touch" || got.Error != tc.wantError {
				t.Fatalf("failure = %+v, want stage=touch error=%q", got, tc.wantError)
			}
			obs, err := ReadRecentObservations(token, 10)
			if err != nil || len(obs) != 0 {
				t.Fatalf("observations = %v, err = %v", obs, err)
			}
		})
	}

	t.Run("401-unauthorized-warns-and-allows", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/v1/sessions":
				_, _ = w.Write([]byte(`{"execution_id":"ex_1","generation":1,"handle":"ex_1"}`))
			case "/v1/claims/touch":
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = fmt.Fprintf(w, `{"error":{"code":"unauthorized","message":%q}}`, hubSecret)
			default:
				t.Errorf("unexpected path %s", r.URL.Path)
				w.WriteHeader(http.StatusNotFound)
			}
		}))
		r := editRunner(t, ClientClaude, srv.URL)
		t.Cleanup(srv.Close)
		token := r.Getenv("IRIS_TOKEN_FILE")
		out := runBounded(t, r)
		if out.Exit != 0 || !strings.Contains(string(out.Stdout), "rejected this host's token") || strings.Contains(string(out.Stdout), "deny") {
			t.Fatalf("unexpected 401 output: stdout=%q stderr=%q exit=%d", out.Stdout, out.Stderr, out.Exit)
		}
		if strings.Contains(string(out.Stdout), hubSecret) {
			t.Fatalf("hub message leaked: %q", out.Stdout)
		}
		got := waitForFailure(t, token)
		if got.Stage != "touch" || got.Error != "hub request failed: unauthorized" {
			t.Fatalf("failure = %+v", got)
		}
		if obs, err := ReadRecentObservations(token, 10); err != nil || len(obs) != 0 {
			t.Fatalf("observations = %v, err = %v", obs, err)
		}
	})

	t.Run("deadline-before-late-409", func(t *testing.T) {
		entered := make(chan struct{})
		release := make(chan struct{})
		handlerDone := make(chan struct{})
		stopAfter := make(chan struct{})
		var enterOnce, releaseOnce, handlerDoneOnce, stopAfterOnce sync.Once
		releaseHandler := func() {
			releaseOnce.Do(func() { close(release) })
		}
		stopWait := func() {
			stopAfterOnce.Do(func() { close(stopAfter) })
		}

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/v1/sessions":
				_, _ = w.Write([]byte(`{"execution_id":"ex_1","generation":1,"handle":"ex_1"}`))
			case "/v1/claims/touch":
				enterOnce.Do(func() { close(entered) })
				<-release
				w.WriteHeader(http.StatusConflict)
				_, _ = fmt.Fprintf(w, `{"error":{"code":"lost","message":%q}}`, hubSecret)
				handlerDoneOnce.Do(func() { close(handlerDone) })
			default:
				t.Errorf("unexpected path %s", r.URL.Path)
				w.WriteHeader(http.StatusNotFound)
			}
		}))

		r := editRunner(t, ClientClaude, srv.URL)
		// Register srv.Close before releaseHandler so LIFO cleanup unblocks the handler first.
		t.Cleanup(srv.Close)
		t.Cleanup(stopWait)
		t.Cleanup(releaseHandler)

		token := r.Getenv("IRIS_TOKEN_FILE")
		r.after = func(time.Duration) <-chan time.Time {
			fired := make(chan time.Time)
			go func() {
				select {
				case <-entered:
					close(fired)
				case <-stopAfter:
				}
			}()
			return fired
		}

		out := runBounded(t, r)
		assertAllow(t, ClientClaude, out)

		got := waitForFailure(t, token)
		if got.Stage != "touch" || got.Error != "hook deadline exceeded" {
			t.Fatalf("failure = %+v, want stage=touch error=%q", got, "hook deadline exceeded")
		}
		if obs, err := ReadRecentObservations(token, 10); err != nil || len(obs) != 0 {
			t.Fatalf("observations = %v, err = %v", obs, err)
		}

		releaseHandler()
		select {
		case <-handlerDone:
		case <-time.After(10 * time.Second):
			t.Fatal("touch handler did not finish after release")
		}
		if obs, err := ReadRecentObservations(token, 10); err != nil || len(obs) != 0 {
			t.Fatalf("late 409 recorded observations = %v, err = %v", obs, err)
		}
	})
}
