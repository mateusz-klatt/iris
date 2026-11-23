package hook

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/mateusz-klatt/iris/internal/backup"
	"github.com/mateusz-klatt/iris/internal/hubclient"
)

// reviewRestoredHook creates G3 on disk, then restores a backup with only G1 for this session.
func reviewRestoredHook(t *testing.T, route string, terminal bool) *restoredHook {
	t.Helper()
	h, live := newRestoredHook(t, &hookClock{wall: time.Unix(1800000000, 0)})
	if route == "/v1/sessions/outside" || strings.HasPrefix(route, "/v1/shell/") {
		h.root = t.TempDir()
	}
	if strings.HasPrefix(route, "/v1/shell/") {
		if err := os.WriteFile(filepath.Join(h.root, "base"), []byte("base"), 0o600); err != nil {
			t.Fatal(err)
		}
		for _, args := range [][]string{{"init"}, {"add", "base"}} {
			if out, err := exec.Command("git", append([]string{"-C", h.root}, args...)...).CombinedOutput(); err != nil {
				t.Fatalf("git %v: %s, %v", args, out, err)
			}
		}
	}
	ctx := context.Background()
	client := &hubclient.Client{Config: hubclient.EnvConfig{Getenv: h.getenv}}
	end := func() {
		t.Helper()
		if err := client.EndExecution(ctx, h.registered.ExecutionID, h.registered.Generation); err != nil {
			t.Fatal(err)
		}
	}
	h.prompt()
	first := h.registered
	if terminal {
		end()
	}
	saved := filepath.Join(t.TempDir(), "backup.db")
	if err := backup.Run(ctx, h.livePath, saved, io.Discard); err != nil {
		t.Fatal(err)
	}
	if !terminal {
		end()
	}
	h.prompt()
	end()
	h.prompt()
	if first.Generation != 1 || h.registered.Generation != 3 {
		t.Fatalf("fixture generations G%d and G%d, want G1 and G3", first.Generation, h.registered.Generation)
	}
	h.restore(live, saved, true)
	if !terminal {
		if err := client.Do(ctx, http.MethodPost, "/v1/executions/"+first.ExecutionID+"/heartbeat", map[string]any{"generation": 1, "source": "shim"}, nil); err != nil {
			t.Fatalf("reattach restored G1: %v", err)
		}
	}
	return h
}

func observeReviewRequests(h *restoredHook, observe func(floorRequest) error) {
	transport := h.server.Client().Transport
	h.http = &http.Client{Transport: restoreTransport(func(r *http.Request) (*http.Response, error) {
		body, err := r.GetBody()
		if err != nil {
			return nil, err
		}
		defer func() { _ = body.Close() }()
		var registration hubclient.SessionRequest
		if err := json.NewDecoder(body).Decode(&registration); err != nil {
			return nil, err
		}
		if err := observe(floorRequest{r.URL.Path, registration.Generation, registration.RetryGeneration}); err != nil {
			return nil, err
		}
		return transport.RoundTrip(r)
	})}
}

func runReviewHook(h *restoredHook, route string) string {
	switch route {
	case "/v1/sessions/outside":
		return h.prompt()
	case "/v1/shell/reconcile", "/v1/shell/scoped":
		return h.run("PreToolUse", `{"session_id":"s1","cwd":`+quote(h.root)+`,"tool_name":"Bash","tool_use_id":"shell","tool_input":{"command":"touch review.go"}}`)
	default:
		return h.run("PreToolUse", writePayload(h.root, "s1", "review.go"))
	}
}

func assertReviewRefusal(t *testing.T, h *restoredHook, out, observation string) {
	t.Helper()
	if out != string(FallbackAllow(ClientClaude).Stdout) {
		t.Errorf("refused hook output %q", out)
	}
	assertReviewObservation(t, h, observation)
	db, err := sql.Open("sqlite", h.restoredPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var generations, highest, claims, snapshots int
	err = db.QueryRow(`SELECT
		(SELECT COUNT(*) FROM Execution WHERE client='claude' AND session_id='s1'),
		(SELECT MAX(generation) FROM Execution WHERE client='claude' AND session_id='s1'),
		(SELECT COUNT(*) FROM Claim WHERE path='review.go'),
		(SELECT COUNT(*) FROM ShellSnapshot)+(SELECT COUNT(*) FROM ReducedShellSnapshot)`).Scan(&generations, &highest, &claims, &snapshots)
	if err != nil || generations != 1 || highest != 1 || claims != 0 || snapshots != 0 {
		t.Errorf("restored authority changed: %d executions, highest G%d, %d claims, %d baselines, err %v", generations, highest, claims, snapshots, err)
	}
}

func assertReviewObservation(t *testing.T, h *restoredHook, want string) {
	t.Helper()
	notes, err := ReadRecentObservations(h.getenv("IRIS_TOKEN_FILE"), 1)
	if err != nil || len(notes) != 1 || notes[0].Error != want {
		t.Errorf("restore refusal observation: %+v, %v, want %q", notes, err, want)
	}
}

func TestARegistrationWithDamagedGenerationProofUsesNoRestoredAuthority(t *testing.T) {
	for _, test := range []struct {
		name, route          string
		terminal, unreadable bool
	}{
		{"live edit with malformed proof", "/v1/sessions", false, false},
		{"terminal edit with unreadable proof", "/v1/sessions", true, true},
		{"outside git with unreadable proof", "/v1/sessions/outside", false, true},
		{"full shell with malformed proof", "/v1/shell/reconcile", false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			checkDamagedReviewProof(t, test.route, test.terminal, test.unreadable, nil)
		})
	}
}

func checkDamagedReviewProof(t *testing.T, route string, terminal, unreadable bool, prepare func(*restoredHook)) {
	t.Helper()
	h := reviewRestoredHook(t, route, terminal)
	if prepare != nil {
		prepare(h)
	}
	path := sessionsFor(h.getenv).name("claude", "s1")
	if unreadable {
		// Reading a directory produces a genuine read error even under a privileged test user.
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	} else if err := os.WriteFile(path, []byte("damaged\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var seen []floorRequest
	observeReviewRequests(h, func(req floorRequest) error {
		seen = append(seen, req)
		return nil
	})
	out := runReviewHook(h, route)
	if len(seen) != 0 {
		t.Errorf("the hook used damaged proof as a new session: %+v", seen)
	}
	if unreadable {
		if info, err := os.Stat(path); err != nil || !info.IsDir() {
			t.Errorf("the hook replaced unreadable proof: %v, %v", info, err)
		}
	} else if raw, err := os.ReadFile(path); err != nil || string(raw) != "damaged\n" {
		t.Errorf("the hook replaced malformed proof with %q, %v", raw, err)
	}
	assertReviewRefusal(t, h, out, "damaged generation proof at "+path)
}

func TestPruneDuringARefusedRegistrationKeepsItsGenerationProof(t *testing.T) {
	const route = "/v1/sessions"
	for _, test := range []struct {
		name      string
		pruneSkew time.Duration
		absent    bool
	}{
		{"touch before the round trip", 0, false},
		{"restore proof removed before refusal", memoryAge + time.Hour, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := reviewRestoredHook(t, route, false)
			m := sessionsFor(h.getenv)
			path := m.name("claude", "s1")
			idle := time.Now().Add(-memoryAge - time.Hour)
			if err := os.Chtimes(path, idle, idle); err != nil {
				t.Fatal(err)
			}
			// The first request pauses after reading G3. Run the same prune that another
			// SessionStart runs while this hook is still waiting for the hub's reply.
			var seen []floorRequest
			observeReviewRequests(h, func(req floorRequest) error {
				seen = append(seen, req)
				if len(seen) != 1 {
					return nil
				}
				if req.route != route || req.generation != 3 {
					return fmt.Errorf("unexpected first request: %+v", req)
				}
				pruner := m
				pruner.now = func() time.Time { return time.Now().Add(test.pruneSkew) }
				if err := pruner.prune(); err != nil {
					t.Errorf("prune: %v", err)
					return err
				}
				gen, present, err := NewSessions(h.getenv("IRIS_TOKEN_FILE")).RecallGeneration("claude", "s1")
				if err != nil || present == test.absent || (present && gen != 3) {
					t.Errorf("proof during registration after prune = G%d, present %t, %v", gen, present, err)
				}
				return nil
			})
			out := runReviewHook(h, route)
			gen, present, err := NewSessions(h.getenv("IRIS_TOKEN_FILE")).RecallGeneration("claude", "s1")
			if err != nil || !present || gen != 3 {
				t.Errorf("proof after refusal = G%d, present %t, %v", gen, present, err)
			}
			if next := runReviewHook(h, route); next != string(FallbackAllow(ClientClaude).Stdout) {
				t.Errorf("next hook output %q", next)
			}
			if want := []floorRequest{{route: route, generation: 3}, {route: route, generation: 3}}; !slices.Equal(seen, want) {
				t.Errorf("requests across both hooks = %+v, want %+v", seen, want)
			}
			assertReviewRefusal(t, h, out, RestoreFloorRefused)
		})
	}
}

func TestRefusalKeepsSessionEndProofCleared(t *testing.T) {
	for _, tc := range []struct {
		name       string
		generation int
	}{
		{"ended generation stays cleared", 0},
		{"newer proof survives", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := newRestoredHook(t, &hookClock{wall: time.Unix(1800000000, 0)})
			client := &hubclient.Client{Config: hubclient.EnvConfig{Getenv: h.getenv}}
			h.prompt()
			if h.registered.Generation != 1 {
				t.Fatalf("initial binding = %+v", h.registered)
			}
			m := NewSessions(h.getenv("IRIS_TOKEN_FILE"))
			wrapped := interceptedRegistration{Client: client, register: func(_ context.Context, req hubclient.SessionRequest) (hubclient.SessionResponse, error) {
				if req.Generation != 1 || req.RetryGeneration != 0 {
					t.Errorf("in-flight edit sent G%d with floor G%d", req.Generation, req.RetryGeneration)
				}
				// The edit already holds G1 while another hook ends that Execution.
				payload := `{"session_id":"s1","cwd":` + quote(h.root) + `}`
				assertAllow(t, ClientClaude, h.runner("SessionEnd", payload, client).Run())
				if got, present, err := m.RecallGeneration("claude", "s1"); got != 0 || present || err != nil {
					t.Errorf("SessionEnd left proof = G%d, present %t, %v", got, present, err)
				}
				if tc.generation > 0 {
					h.prompt()
					if h.registered.Generation != tc.generation {
						t.Errorf("replacement binding = %+v, want G%d", h.registered, tc.generation)
					}
				}
				return hubclient.SessionResponse{}, &hubclient.Error{Code: "lost"}
			}}
			out := h.runner("PreToolUse", writePayload(h.root, "s1", "review.go"), wrapped).Run()
			assertAllow(t, ClientClaude, out)
			if got, present, err := m.RecallGeneration("claude", "s1"); got != tc.generation || present != (tc.generation > 0) || err != nil {
				t.Errorf("refused edit left proof = G%d, present %t, %v, want G%d", got, present, err, tc.generation)
			}
			marker := sessionsFor(h.getenv).name("claude", "s1") + ".ended"
			notes, err := ReadRecentObservations(h.getenv("IRIS_TOKEN_FILE"), 1)
			if err != nil || len(notes) != 1 || notes[0].Error != RestoreFloorHubTooOld+" at "+marker {
				t.Errorf("legacy refusal observation = %+v, %v", notes, err)
			}
		})
	}
}

func TestFloorRetryBeyondDeadlineFailsOpen(t *testing.T) {
	const client = ClientAntigravity
	r := editRunner(t, client, "")
	sessionsFor(r.Getenv).Remember(string(client), "s1", 3)
	synctest.Test(t, func(t *testing.T) {
		type retry struct {
			request hubclient.SessionRequest
			ctx     context.Context
		}
		entered := make(chan retry, 1)
		release := make(chan struct{})
		defer close(release)
		r.Hub = interceptedRegistration{Client: r.Hub.(*hubclient.Client), register: func(ctx context.Context, req hubclient.SessionRequest) (hubclient.SessionResponse, error) {
			if req.Generation == 3 && req.RetryGeneration == 0 {
				return hubclient.SessionResponse{}, lostWithHighest(3)
			}
			entered <- retry{req, ctx}
			// Simulate a transport that stays blocked even after the hook cancels it.
			<-release
			return hubclient.SessionResponse{}, ctx.Err()
		}}
		deadline := make(chan time.Time)
		r.after = func(time.Duration) <-chan time.Time { return deadline }
		done := make(chan Output, 1)
		go func() { done <- r.Run() }()
		var pending retry
		select {
		case pending = <-entered:
		case out := <-done:
			t.Fatalf("hook returned before retrying: %+v", out)
		case <-time.After(5 * time.Second):
			t.Fatal("hook did not enter its retry")
		}
		if pending.request.Generation != 0 || pending.request.RetryGeneration != 3 {
			t.Fatalf("retry = %+v, want G0 with floor G3", pending.request)
		}
		close(deadline)
		select {
		case out := <-done:
			assertAllow(t, client, out)
		case <-time.After(5 * time.Second):
			t.Fatal("blocked retry kept the hook from answering at its deadline")
		}
		if pending.ctx.Err() != context.Canceled {
			t.Errorf("retry context after deadline = %v", pending.ctx.Err())
		}
		if f := waitForFailure(t, r.Getenv("IRIS_TOKEN_FILE")); f.Stage != "session" || f.Error != "hook deadline exceeded" {
			t.Errorf("deadline failure = %+v", f)
		}
	})
}

func TestGenerationProofDamagedDuringRegistrationPreventsAuthority(t *testing.T) {
	for _, test := range []struct {
		name       string
		afterRetry bool
	}{
		{"damaged before an eligible G0 retry", false},
		{"damaged after a successful G0 retry", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			h, _ := newRestoredHook(t, &hookClock{wall: time.Unix(1800000000, 0)})
			client := &hubclient.Client{Config: hubclient.EnvConfig{Getenv: h.getenv}}
			h.prompt()
			if err := client.EndExecution(t.Context(), h.registered.ExecutionID, h.registered.Generation); err != nil {
				t.Fatal(err)
			}
			proof := sessionsFor(h.getenv).name("claude", "s1")
			const corrupt = "damaged during registration\n"
			var seen []floorRequest
			var touches int
			observeReviewRequests(h, func(req floorRequest) error {
				if strings.HasPrefix(req.route, "/v1/claims/touch") {
					touches++
				} else {
					seen = append(seen, req)
				}
				return nil
			})
			client.HTTP = h.http
			wrapped := interceptedRegistration{Client: client, register: func(ctx context.Context, req hubclient.SessionRequest) (hubclient.SessionResponse, error) {
				sess, err := client.RegisterSession(ctx, req)
				if (req.Generation == 0) == test.afterRetry {
					if test.afterRetry && (err != nil || sess.Generation != 2) {
						t.Errorf("eligible retry did not succeed as G2: %+v, %v", sess, err)
					}
					if writeErr := os.WriteFile(proof, []byte(corrupt), 0o600); writeErr != nil {
						t.Errorf("corrupt proof during request: %v", writeErr)
					}
				}
				return sess, err
			}}
			out := h.runner("PreToolUse", writePayload(h.root, "s1", "review.go"), wrapped).Run()
			assertAllow(t, ClientClaude, out)
			want := []floorRequest{{route: "/v1/sessions", generation: 1}}
			if test.afterRetry {
				want = append(want, floorRequest{route: "/v1/sessions", floor: 1})
			}
			if !slices.Equal(seen, want) {
				t.Errorf("requests = %+v, want %+v", seen, want)
			}
			if touches != 0 {
				t.Errorf("damaged proof led to %d touches, want zero", touches)
			}
			db, err := sql.Open("sqlite", h.livePath)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = db.Close() }()
			var claims int
			if err := db.QueryRow(`SELECT COUNT(*) FROM Claim`).Scan(&claims); err != nil || claims != 0 {
				t.Errorf("Claim rows after proof damage = %d, %v, want zero", claims, err)
			}
			if raw, err := os.ReadFile(proof); err != nil || string(raw) != corrupt {
				t.Errorf("proof after refusal = %q, %v", raw, err)
			}
			assertReviewObservation(t, h, "damaged generation proof at "+proof)
		})
	}
}

func TestSessionEndWithDamagedEndMarkerKeepsProofAndRecordsObservation(t *testing.T) {
	for _, unreadable := range []bool{false, true} {
		name := "malformed marker"
		if unreadable {
			name = "unreadable marker"
		}
		t.Run(name, func(t *testing.T) {
			h, _ := newRestoredHook(t, &hookClock{wall: time.Unix(1800000000, 0)})
			h.prompt()
			if h.registered.Generation != 1 {
				t.Fatalf("initial registration = %+v", h.registered)
			}
			proof := sessionsFor(h.getenv).name("claude", "s1")
			marker := proof + ".ended"
			if unreadable {
				if err := os.Mkdir(marker, 0o700); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(marker, []byte("damaged\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			out := h.run("SessionEnd", `{"session_id":"s1","cwd":`+quote(h.root)+`}`)
			if out != string(FallbackAllow(ClientClaude).Stdout) {
				t.Fatalf("SessionEnd output %q", out)
			}
			if gen, err := readGeneration(proof); gen != 1 || err != nil {
				t.Fatalf("SessionEnd removed or altered proof when marker was damaged: G%d, %v", gen, err)
			}
			if unreadable {
				if info, err := os.Stat(marker); err != nil || !info.IsDir() {
					t.Fatalf("SessionEnd replaced unreadable marker: %v, %v", info, err)
				}
			} else if raw, err := os.ReadFile(marker); err != nil || string(raw) != "damaged\n" {
				t.Fatalf("SessionEnd replaced malformed marker with %q, %v", raw, err)
			}
			failures, err := ReadRecentFailures(h.getenv("IRIS_TOKEN_FILE"), 10)
			if err != nil || len(failures) != 0 {
				t.Fatalf("SessionEnd failures = %+v, %v", failures, err)
			}
			assertReviewObservation(t, h, "damaged generation proof at "+marker)
		})
	}
}

func TestSessionEndThatCannotRecordTheMarkerKeepsProofAndNamesIt(t *testing.T) {
	for _, tc := range []struct {
		name      string
		failWrite bool
		hold      func(dir string) (func(), error)
	}{
		{
			// A directory mode of 0555 does not stop CreateTemp on Windows, so the
			// marker write is refused through fileMemory.restrict. replace checks
			// that before the rename, on every OS, and leaves no marker behind.
			name:      "marker write fails",
			failWrite: true,
		},
		{
			name: "sessions lock missed",
			hold: func(dir string) (func(), error) {
				return (&fileMemory{dir: dir, now: time.Now, restrict: func(string) error { return nil }}).lock(time.Second)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := newRestoredHook(t, &hookClock{wall: time.Unix(1800000000, 0)})
			h.prompt()
			if h.registered.Generation != 1 {
				t.Fatalf("initial registration = %+v", h.registered)
			}
			mem := sessionsFor(h.getenv)
			proof := mem.name("claude", "s1")
			marker := proof + ".ended"
			var block atomic.Bool
			if tc.failWrite {
				previous := mem.restrict
				mem.restrict = func(path string) error {
					if block.Load() {
						return errors.New("marker write")
					}
					return previous(path)
				}
				tc.hold = func(string) (func(), error) {
					block.Store(true)
					return func() { block.Store(false) }, nil
				}
			}
			var mu sync.Mutex
			var release func()
			finish := func() {
				mu.Lock()
				unlock := release
				release = nil
				mu.Unlock()
				if unlock != nil {
					unlock()
				}
			}
			t.Cleanup(finish)
			transport := h.server.Client().Transport
			h.http = &http.Client{Transport: restoreTransport(func(r *http.Request) (*http.Response, error) {
				if strings.HasSuffix(r.URL.Path, "/end") {
					// Remember has finished. Hold the failure across Forget, which runs
					// after this response is read.
					unlock, err := tc.hold(mem.dir)
					if err != nil {
						return nil, err
					}
					mu.Lock()
					release = unlock
					mu.Unlock()
				}
				return transport.RoundTrip(r)
			})}
			payload := `{"session_id":"s1","cwd":` + quote(h.root) + `}`
			client := lastSession{Client: &hubclient.Client{Config: hubclient.EnvConfig{Getenv: h.getenv}, HTTP: h.http}, last: &h.registered}
			runner := h.runner("SessionEnd", payload, client)
			if tc.failWrite {
				runner.Memory = mem
			}
			ran := runner.Run()
			finish()
			if ran.Exit != 0 {
				t.Fatalf("exit %d: %s", ran.Exit, ran.Stderr)
			}
			out := string(ran.Stdout)
			if out != string(FallbackAllow(ClientClaude).Stdout) {
				t.Fatalf("SessionEnd output %q", out)
			}
			if gen, err := readGeneration(proof); gen != 1 || err != nil {
				t.Fatalf("SessionEnd removed proof it could not mark ended: G%d, %v", gen, err)
			}
			if _, err := os.Lstat(marker); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("failed SessionEnd left a marker: %v", err)
			}
			assertReviewObservation(t, h, "damaged generation proof at "+marker)
		})
	}
}

func TestRefusalNamesAContributingEndMarker(t *testing.T) {
	for _, tc := range []struct {
		name         string
		ended        string
		prune        bool
		endsInFlight bool
		wantProof    string
	}{
		{name: "pruned proof stays below the marker", ended: "5\n", prune: true, endsInFlight: true, wantProof: ""},
		{name: "present proof names the marker written during registration", ended: "3\n", endsInFlight: true, wantProof: "3"},
		{name: "pruned proof is restored above the marker", ended: "1\n", prune: true, wantProof: "3"},
		{name: "proof already present still names the marker", ended: "2\n", wantProof: "3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const route = "/v1/sessions"
			h := reviewRestoredHook(t, route, false)
			m := sessionsFor(h.getenv)
			proof := m.name("claude", "s1")
			marker := proof + ".ended"
			writeMarker := func() error {
				if err := os.WriteFile(marker, []byte(tc.ended), 0o600); err != nil || !tc.prune {
					return err
				}
				newer := time.Now().Add(2 * time.Hour)
				return os.Chtimes(marker, newer, newer)
			}
			if !tc.endsInFlight {
				if err := writeMarker(); err != nil {
					t.Fatal(err)
				}
			}
			observeReviewRequests(h, func(req floorRequest) error {
				if req.route != route || req.generation != 3 {
					return nil
				}
				if tc.endsInFlight {
					if err := writeMarker(); err != nil {
						return err
					}
				}
				if tc.prune {
					pruner := m
					pruner.now = func() time.Time { return time.Now().Add(memoryAge + time.Hour) }
					return pruner.prune()
				}
				return nil
			})
			out := runReviewHook(h, route)
			raw, err := os.ReadFile(proof)
			switch {
			case tc.wantProof == "" && !errors.Is(err, os.ErrNotExist):
				t.Fatalf("refusal restored ended proof: %q, %v", raw, err)
			case tc.wantProof != "" && (err != nil || strings.TrimSpace(string(raw)) != tc.wantProof):
				t.Fatalf("proof after refusal = %q, %v, want %q", raw, err, tc.wantProof)
			}
			if got, err := os.ReadFile(marker); err != nil || string(got) != tc.ended {
				t.Fatalf("marker after refusal = %q, %v", got, err)
			}
			assertReviewObservation(t, h, RestoreFloorRefused+" at "+marker)
			if out != string(FallbackAllow(ClientClaude).Stdout) {
				t.Fatalf("refused hook output %q", out)
			}
		})
	}
}

func TestRefusalWithPrunedProofAndDamagedEndMarkerKeepsAbsentProofAndRecordsBoth(t *testing.T) {
	for _, unreadable := range []bool{false, true} {
		name := "malformed marker"
		if unreadable {
			name = "unreadable marker"
		}
		t.Run(name, func(t *testing.T) {
			const route = "/v1/sessions"
			h := reviewRestoredHook(t, route, false)
			m := sessionsFor(h.getenv)
			proof := m.name("claude", "s1")
			marker := proof + ".ended"
			if unreadable {
				if err := os.Mkdir(marker, 0o700); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(marker, []byte("damaged\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			// Keep marker newer than the prune cutoff while aging proof past it via pruner.now,
			// so prune removes only proof after RecallGeneration refreshes proof's mtime.
			newer := time.Now().Add(2 * time.Hour)
			if err := os.Chtimes(marker, newer, newer); err != nil {
				t.Fatal(err)
			}
			observeReviewRequests(h, func(req floorRequest) error {
				if req.route == route && req.generation == 3 {
					pruner := m
					pruner.now = func() time.Time { return time.Now().Add(memoryAge + time.Hour) }
					if err := pruner.prune(); err != nil {
						return err
					}
				}
				return nil
			})
			out := runReviewHook(h, route)
			if gen, present, err := NewSessions(h.getenv("IRIS_TOKEN_FILE")).RecallGeneration("claude", "s1"); gen != 0 || present || err != nil {
				t.Fatalf("proof after refusal with damaged marker = G%d, present %t, %v", gen, present, err)
			}
			if unreadable {
				if info, err := os.Stat(marker); err != nil || !info.IsDir() {
					t.Fatalf("refusal replaced unreadable marker: %v, %v", info, err)
				}
			} else if raw, err := os.ReadFile(marker); err != nil || string(raw) != "damaged\n" {
				t.Fatalf("refusal replaced malformed marker with %q, %v", raw, err)
			}
			notes, err := ReadRecentObservations(h.getenv("IRIS_TOKEN_FILE"), 10)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, note := range notes {
				got = append(got, note.Error)
			}
			want := []string{RestoreFloorRefused + " at " + marker, "damaged generation proof at " + marker}
			if !slices.Equal(got, want) {
				t.Fatalf("observations = %v, want %v", got, want)
			}
			if out != string(FallbackAllow(ClientClaude).Stdout) {
				t.Fatalf("refused hook output %q", out)
			}
		})
	}
}
