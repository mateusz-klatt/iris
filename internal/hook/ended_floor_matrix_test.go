package hook

import (
	"database/sql"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/backup"
	"github.com/mateusz-klatt/iris/internal/binding"
	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/proc"
	"github.com/mateusz-klatt/iris/internal/store"
)

type endedFloorExecutionState struct {
	id                 string
	generation         int64
	state              string
	deadline, renewed  int64
	lastSeen, hookSeen int64
}

func endedFloorExecutionStates(t *testing.T, h *restoredHook) []endedFloorExecutionState {
	t.Helper()
	path := h.livePath
	if h.restoredPath != "" {
		path = h.restoredPath
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	rows, err := db.Query(`SELECT id,generation,state,deadline_tick_ms,lease_renewed_tick_ms,COALESCE(last_seen_ms,0),COALESCE(hook_seen_ms,0) FROM Execution WHERE client='claude' AND session_id='s1' ORDER BY generation,id`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []endedFloorExecutionState
	for rows.Next() {
		var state endedFloorExecutionState
		if err := rows.Scan(&state.id, &state.generation, &state.state, &state.deadline, &state.renewed, &state.lastSeen, &state.hookSeen); err != nil {
			t.Fatal(err)
		}
		out = append(out, state)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func endedFloorMatrixFixture(t *testing.T, scenario string, bindingClient, shell bool) (*restoredHook, *hookClock, string) {
	t.Helper()
	clock := &hookClock{wall: time.Unix(1800000000, 0)}
	h, live := newRestoredHook(t, clock)
	if shell {
		h.root = t.TempDir()
		if out, err := exec.Command("git", "-C", h.root, "init").CombinedOutput(); err != nil {
			t.Fatalf("git init: %s, %v", out, err)
		}
	}
	payload := `{"session_id":"s1","cwd":` + quote(h.root) + `}`
	h.prompt()
	h.run("SessionEnd", payload)
	restoredBackup := filepath.Join(t.TempDir(), "terminal-g1.db")
	if err := backup.Run(t.Context(), h.livePath, restoredBackup, io.Discard); err != nil {
		t.Fatal(err)
	}
	h.prompt()
	if h.registered.Generation != 2 {
		t.Fatalf("fixture registration = %+v, want G2", h.registered)
	}
	marker := sessionsFor(h.getenv).name("claude", "s1") + ".ended"
	switch scenario {
	case "restored history at G1":
		h.run("SessionEnd", payload)
		h.restore(live, restoredBackup, true)
	case "equal-generation active G2":
		activeBackup := filepath.Join(t.TempDir(), "active-g2.db")
		if err := backup.Run(t.Context(), h.livePath, activeBackup, io.Discard); err != nil {
			t.Fatal(err)
		}
		h.run("SessionEnd", payload)
		restored := h.restore(live, activeBackup, true)
		storeCall(t, restored, "sessions", store.Request{Client: "claude", Host: "testhost", SessionID: "s1", Cwd: h.root, Worktree: h.root, Generation: 2, Source: "hook"})
	case "damaged .ended":
		if err := os.WriteFile(marker, []byte("damaged\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	case "fresh start above marker":
		h.run("SessionEnd", payload)
		if bindingClient {
			h.run("SessionStart", payload)
			if h.registered.Generation != 3 {
				t.Fatalf("fresh binding fixture = %+v, want G3", h.registered)
			}
		}
	default:
		t.Fatalf("unknown scenario %q", scenario)
	}
	return h, clock, marker
}

func runEndedFloorMatrixOperation(t *testing.T, h *restoredHook, operation string) (string, binding.Bound, error) {
	t.Helper()
	switch operation {
	case "ordinary hook":
		return h.prompt(), binding.Bound{}, nil
	case "shell reconcile":
		payload := `{"session_id":"s1","cwd":` + quote(h.root) + `,"tool_name":"Bash","tool_use_id":"shell","tool_input":{"command":"true"}}`
		return h.run("PreToolUse", payload), binding.Bound{}, nil
	case "SessionStart":
		return h.run("SessionStart", `{"session_id":"s1","cwd":`+quote(h.root)+`}`), binding.Bound{}, nil
	case "MCP bind", "watch":
		client := &hubclient.Client{Config: hubclient.EnvConfig{Getenv: h.getenv}, HTTP: h.http}
		resolver := binding.New(client, "claude")
		resolver.Host, resolver.CWD = "testhost", h.root
		resolver.Getenv = func(key string) string {
			if key == "CLAUDE_CODE_SESSION_ID" {
				return "s1"
			}
			return h.getenv(key)
		}
		resolver.Ancestor = func() (proc.Ancestor, error) { return proc.Ancestor{}, nil }
		sessions := NewSessions(h.getenv("IRIS_TOKEN_FILE")) // gitleaks:allow (a test observation token and path, not a credential)
		resolver.Recall = sessions.RecallBindingGeneration
		resolver.ObserveBindingRefusal = sessions.ObserveBindingRefusal
		source := "shim"
		if operation == "watch" {
			source = "watch"
		}
		resolver.HeartbeatSource = source
		resolver.Grace = 0
		bound, err := resolver.Resolve(t.Context(), "")
		if err == nil {
			err = resolver.Heartbeat(t.Context(), source)
		}
		return "", bound, err
	default:
		t.Fatalf("unknown operation %q", operation)
		return "", binding.Bound{}, nil
	}
}

func assertEndedFloorRowsUnchanged(t *testing.T, before, after []endedFloorExecutionState, floor int64) {
	t.Helper()
	for _, previous := range before {
		if previous.generation > floor {
			continue
		}
		at := slices.IndexFunc(after, func(current endedFloorExecutionState) bool { return current.id == previous.id })
		if at < 0 || after[at] != previous {
			t.Errorf("Execution at or below G%d was created, ended or renewed: before %+v, after %+v", floor, previous, after)
		}
	}
	for _, current := range after {
		if current.generation <= floor && !slices.ContainsFunc(before, func(previous endedFloorExecutionState) bool { return previous.id == current.id }) {
			t.Errorf("new Execution at or below G%d: %+v", floor, current)
		}
	}
}

func TestEndedThroughFloorAcrossRegistrationAndBinding(t *testing.T) {
	operations := []struct {
		name, route string
	}{
		{name: "ordinary hook", route: "/v1/sessions"},
		{name: "shell reconcile", route: "/v1/shell/reconcile"},
		{name: "SessionStart", route: "/v1/sessions"},
		{name: "MCP bind"},
		{name: "watch"},
	}
	scenarios := []struct {
		name           string
		damaged, fresh bool
	}{
		{name: "restored history at G1"},
		{name: "equal-generation active G2"},
		{name: "damaged .ended", damaged: true},
		{name: "fresh start above marker", fresh: true},
	}
	for _, operation := range operations {
		for _, scenario := range scenarios {
			t.Run(operation.name+"/"+scenario.name, func(t *testing.T) {
				bindingClient := operation.name == "MCP bind" || operation.name == "watch"
				h, clock, marker := endedFloorMatrixFixture(t, scenario.name, bindingClient, operation.name == "shell reconcile")
				var requests []floorRequest
				observeReviewRequests(h, func(req floorRequest) error {
					requests = append(requests, req)
					return nil
				})
				clock.advance(time.Second)
				before := endedFloorExecutionStates(t, h)
				out, bound, err := runEndedFloorMatrixOperation(t, h, operation.name)
				after := endedFloorExecutionStates(t, h)
				assertEndedFloorRowsUnchanged(t, before, after, 2)

				if scenario.fresh {
					if !slices.ContainsFunc(after, func(current endedFloorExecutionState) bool {
						return current.generation == 3 && current.state == "active"
					}) {
						t.Errorf("fresh start did not produce active G3: %+v", after)
					}
				} else if !slices.Equal(after, before) {
					t.Errorf("refused operation changed retained Executions: before %+v, after %+v", before, after)
				}
				if bindingClient {
					if scenario.fresh {
						if err != nil || bound.Generation != 3 {
							t.Errorf("fresh binding = %+v, %v, want G3", bound, err)
						}
					} else if bound != (binding.Bound{}) || !binding.IsCode(err, "lost") {
						t.Errorf("refused binding = %+v, %v", bound, err)
					}
				} else if scenario.fresh {
					if strings.Contains(out, `"permissionDecision":"deny"`) {
						t.Errorf("fresh hook was denied: %s", out)
					}
				} else if operation.name == "shell reconcile" {
					if !strings.Contains(out, `"permissionDecision":"deny"`) || !strings.Contains(out, "iris doctor") {
						t.Errorf("refused shell output %q", out)
					}
				} else if out != string(FallbackAllow(ClientClaude).Stdout) {
					t.Errorf("refused hook output %q", out)
				}

				if operation.route != "" {
					var registrations []floorRequest
					for _, req := range requests {
						if req.route == operation.route {
							registrations = append(registrations, req)
						}
					}
					want := []floorRequest{{route: operation.route, endedThrough: 2}}
					if scenario.damaged {
						want = nil
					}
					if !slices.Equal(registrations, want) {
						t.Errorf("registration requests = %+v, want %+v", registrations, want)
					}
				}
				proof := strings.TrimSuffix(marker, ".ended")
				generation, readErr := readGeneration(proof)
				wantProof := 0
				if scenario.damaged {
					wantProof = 2
				} else if scenario.fresh {
					wantProof = 3
				}
				if generation != wantProof || readErr != nil {
					t.Errorf("proof after operation = G%d, %v, want G%d", generation, readErr, wantProof)
				}
				tokenPath := h.getenv("IRIS_TOKEN_FILE")
				observations, observeErr := ReadRecentObservations(tokenPath, 1) // gitleaks:allow (a test observation token and path, not a credential)
				if observeErr != nil {
					t.Fatal(observeErr)
				}
				if scenario.fresh {
					if len(observations) != 0 {
						t.Errorf("fresh operation recorded observations: %+v", observations)
					}
				} else {
					wantObservation := "ended generation refused at " + marker // gitleaks:allow (a test observation token and path, not a credential)
					if scenario.damaged {
						wantObservation = "damaged generation proof at " + marker // gitleaks:allow (a test observation token and path, not a credential)
					}
					if len(observations) != 1 || observations[0].Error != wantObservation {
						t.Errorf("observations = %+v, want %q", observations, wantObservation)
					}
				}
				failures, failureErr := ReadRecentFailures(tokenPath, 10)
				if failureErr != nil || len(failures) != 0 {
					t.Errorf("local refusal became a hook failure: %+v, %v", failures, failureErr)
				}
			})
		}
	}
}
