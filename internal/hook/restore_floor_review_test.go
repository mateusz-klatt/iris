package hook

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
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

func assertReviewRefusal(t *testing.T, h *restoredHook, out string) {
	t.Helper()
	if out != string(FallbackAllow(ClientClaude).Stdout) {
		t.Errorf("refused hook output %q", out)
	}
	assertReviewObservation(t, h)
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

func assertReviewObservation(t *testing.T, h *restoredHook) {
	t.Helper()
	notes, err := ReadRecentObservations(h.getenv("IRIS_TOKEN_FILE"), 1)
	if err != nil || len(notes) != 1 || notes[0].Error != RestoreFloorRefused {
		t.Errorf("restore refusal observation: %+v, %v", notes, err)
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
	assertReviewRefusal(t, h, out)
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
			assertReviewRefusal(t, h, out)
		})
	}
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
			observeReviewRequests(h, func(req floorRequest) error {
				seen = append(seen, req)
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
			if raw, err := os.ReadFile(proof); err != nil || string(raw) != corrupt {
				t.Errorf("proof after refusal = %q, %v", raw, err)
			}
			assertReviewObservation(t, h)
		})
	}
}
