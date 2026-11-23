package hook

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/mateusz-klatt/iris/internal/backup"
	"github.com/mateusz-klatt/iris/internal/hubclient"
)

func TestRegistrationRemembersReusedGenerationAfterRetention(t *testing.T) {
	for _, tc := range []struct {
		name, event string
		staleProof  bool
	}{
		{name: "SessionStart after the prune limit", event: "SessionStart"},
		{name: "prompt without a prune", event: "UserPromptSubmit"},
		{name: "SessionStart with stale live proof", event: "SessionStart", staleProof: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock := &hookClock{wall: time.Now()}
			h, live := newRestoredHook(t, clock)
			payload := `{"session_id":"s1","cwd":` + quote(h.root) + `,"prompt":"go"}`
			h.prompt()
			first := h.registered
			h.run("SessionEnd", payload)
			m := sessionsFor(h.getenv)
			marker := m.name("claude", "s1") + ".ended"
			if generation, err := readGeneration(marker); generation != 1 || err != nil {
				t.Fatalf("SessionEnd left marker G%d, %v", generation, err)
			}

			old := time.Now().Add(-memoryAge - time.Hour)
			if err := os.Chtimes(marker, old, old); err != nil {
				t.Fatal(err)
			}
			if tc.staleProof {
				if err := os.WriteFile(m.name("claude", "s1"), []byte("1"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			// These sort before the marker and exhaust the general cleanup's budget.
			for i := range pruneLimit {
				path := filepath.Join(m.dir, fmt.Sprintf("!stale-%d", i))
				if err := os.WriteFile(path, []byte("1"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Chtimes(path, old, old); err != nil {
					t.Fatal(err)
				}
			}
			clock.advance(memoryAge + time.Hour)
			removed, err := live.Sweep(t.Context())
			if err != nil || removed["executions"] != 1 {
				t.Fatalf("retention sweep = %v, %v", removed, err)
			}

			h.run(tc.event, payload)
			if h.registered.Generation != 1 || h.registered.ExecutionID == first.ExecutionID {
				t.Fatalf("registration after retention = %+v, prior = %+v", h.registered, first)
			}
			generation, ended, err := NewSessions(h.getenv("IRIS_TOKEN_FILE")).RecallBindingGeneration("claude", "s1")
			if generation != 1 || ended != 0 || err != nil {
				t.Fatalf("binding proof after G1 reuse = G%d, ended through G%d, %v", generation, ended, err)
			}
		})
	}
}

func TestSessionEndClearsProofForAnEarlierGeneration(t *testing.T) {
	h, _ := newRestoredHook(t, &hookClock{wall: time.Now()})
	payload := `{"session_id":"s1","cwd":` + quote(h.root) + `}`
	h.prompt()
	h.run("SessionEnd", payload)
	h.prompt()
	if h.registered.Generation != 2 {
		t.Fatalf("successor = %+v, want G2", h.registered)
	}
	proof := sessionsFor(h.getenv).name("claude", "s1")
	client := &hubclient.Client{Config: hubclient.EnvConfig{Getenv: h.getenv}}
	wrapped := &delayedSessionEndHub{Client: client, afterEnd: func() error {
		// An older process races the successful end with a delayed write of G1.
		return os.WriteFile(proof, []byte("1"), 0o600)
	}}
	out := h.runner("SessionEnd", payload, wrapped).Run()
	assertAllow(t, ClientClaude, out)
	if generation, err := readGeneration(proof); generation != 0 || err != nil {
		t.Fatalf("successful end left earlier live proof G%d, %v", generation, err)
	}
	if generation, err := readGeneration(proof + ".ended"); generation != 2 || err != nil {
		t.Fatalf("successful end left marker G%d, %v, want G2", generation, err)
	}
}

func TestSessionStartReadsIdleProofBeforePruning(t *testing.T) {
	h, live := newRestoredHook(t, &hookClock{wall: time.Now()})
	h.prompt()
	first := h.registered
	proof := sessionsFor(h.getenv).name("claude", "s1")
	old := time.Now().Add(-memoryAge - time.Hour)
	if err := os.Chtimes(proof, old, old); err != nil {
		t.Fatal(err)
	}
	saved := filepath.Join(t.TempDir(), "backup.db")
	if err := backup.Run(t.Context(), h.livePath, saved, io.Discard); err != nil {
		t.Fatal(err)
	}
	h.restore(live, saved, true)
	h.run("SessionStart", `{"session_id":"s1","cwd":`+quote(h.root)+`}`)
	if h.registered.ExecutionID != first.ExecutionID || h.registered.Generation != first.Generation {
		t.Fatalf("SessionStart lost idle proof for the recovering session: before %+v, after %+v", first, h.registered)
	}
}

func TestSessionStartRecoversFromStaleEndedProof(t *testing.T) {
	h, _ := newRestoredHook(t, &hookClock{wall: time.Now()})
	payload := `{"session_id":"s1","cwd":` + quote(h.root) + `}`
	h.prompt()
	h.run("SessionEnd", payload)
	h.prompt()
	second := h.registered
	h.run("SessionEnd", payload)
	proof := sessionsFor(h.getenv).name("claude", "s1")
	if err := os.WriteFile(proof, []byte("1"), 0o600); err != nil {
		t.Fatal(err)
	}
	var requests []floorRequest
	observeReviewRequests(h, func(req floorRequest) error {
		requests = append(requests, req)
		return nil
	})

	h.run("SessionStart", payload)
	if h.registered.Generation != 3 || h.registered.ExecutionID == second.ExecutionID {
		t.Fatalf("SessionStart with stale proof failed to obtain G3: %+v", h.registered)
	}
	if want := []floorRequest{{route: "/v1/sessions", endedThrough: 2}}; !slices.Equal(requests, want) {
		t.Fatalf("SessionStart requests = %+v, want %+v", requests, want)
	}
	if generation, err := readGeneration(proof); generation != 3 || err != nil {
		t.Fatalf("remembered successor = G%d, %v", generation, err)
	}
}

func TestStaleEndedProofRefusalsAreLocalObservations(t *testing.T) {
	h, _ := newRestoredHook(t, &hookClock{wall: time.Now()})
	h.root = t.TempDir()
	if out, err := exec.Command("git", "-C", h.root, "init").CombinedOutput(); err != nil {
		t.Fatalf("git init: %s, %v", out, err)
	}
	payload := `{"session_id":"s1","cwd":` + quote(h.root) + `}`
	h.prompt()
	h.run("SessionEnd", payload)
	proof := sessionsFor(h.getenv).name("claude", "s1")
	if err := os.WriteFile(proof, []byte("1"), 0o600); err != nil {
		t.Fatal(err)
	}
	observeReviewRequests(h, func(req floorRequest) error {
		t.Errorf("locally refused hook sent a request: %+v", req)
		return nil
	})
	if out := h.prompt(); out != string(FallbackAllow(ClientClaude).Stdout) {
		t.Fatalf("refused prompt output %q", out)
	}
	if out := h.run("PreToolUse", writePayload(h.root, "s1", "review.go")); !strings.Contains(out, `"permissionDecision":"deny"`) || !strings.Contains(out, "iris doctor") {
		t.Fatalf("refused edit output %q", out)
	}
	shell := `{"session_id":"s1","cwd":` + quote(h.root) + `,"tool_name":"Bash","tool_use_id":"shell","tool_input":{"command":"touch review.go"}}`
	if out := h.run("PreToolUse", shell); !strings.Contains(out, `"permissionDecision":"deny"`) || !strings.Contains(out, "iris doctor") {
		t.Fatalf("refused shell command output %q", out)
	}
	if out := h.run("PostToolUse", shell); !strings.Contains(out, "iris doctor") {
		t.Fatalf("unreconciled shell command output %q", out)
	}
	if out := h.run("SessionEnd", payload); out != string(FallbackAllow(ClientClaude).Stdout) {
		t.Fatalf("refused SessionEnd output %q", out)
	}
	log := h.getenv("IRIS_TOKEN_FILE")
	if failures, err := ReadRecentFailures(log, 10); len(failures) != 0 || err != nil {
		t.Fatalf("local vetoes appeared as failures: %+v, %v", failures, err)
	}
	observations, err := ReadRecentObservations(log, 10)
	if len(observations) != 5 || err != nil {
		t.Fatalf("observations = %+v, %v", observations, err)
	}
	for _, observation := range observations {
		if !observation.Observed() || observation.Error != "ended generation refused at "+proof+".ended" {
			t.Errorf("unexpected local observation: %+v", observation)
		}
	}
}

func TestSessionStartNamesDamagedEndMarkerBeforeRegistration(t *testing.T) {
	h, _ := newRestoredHook(t, &hookClock{wall: time.Now()})
	m := sessionsFor(h.getenv)
	if err := m.directory(); err != nil {
		t.Fatal(err)
	}
	marker := m.name("claude", "s1") + ".ended"
	if err := os.WriteFile(marker, []byte("damaged"), 0o600); err != nil {
		t.Fatal(err)
	}
	observeReviewRequests(h, func(req floorRequest) error {
		t.Errorf("SessionStart with damaged end marker made a request: %+v", req)
		return nil
	})
	out := h.run("SessionStart", `{"session_id":"s1","cwd":`+quote(h.root)+`}`)
	if out != string(FallbackAllow(ClientClaude).Stdout) {
		t.Fatalf("SessionStart with damaged end marker returned %q", out)
	}
	assertReviewObservation(t, h, "damaged generation proof at "+marker)
}

func TestSessionStartEndFloorStopsRestoredHistory(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		t.Run(fmt.Sprintf("terminal=%t", terminal), func(t *testing.T) {
			h, live := newRestoredHook(t, &hookClock{wall: time.Now()})
			payload := `{"session_id":"s1","cwd":` + quote(h.root) + `}`
			h.prompt()
			if terminal {
				h.run("SessionEnd", payload)
			}
			saved := filepath.Join(t.TempDir(), "backup.db")
			if err := backup.Run(t.Context(), h.livePath, saved, io.Discard); err != nil {
				t.Fatal(err)
			}
			if !terminal {
				h.run("SessionEnd", payload)
			}
			h.prompt()
			h.run("SessionEnd", payload)
			marker := sessionsFor(h.getenv).name("claude", "s1") + ".ended"
			if generation, err := readGeneration(marker); generation != 2 || err != nil {
				t.Fatalf("ended marker = G%d, %v, want G2", generation, err)
			}
			h.restore(live, saved, true)
			var requests []floorRequest
			observeReviewRequests(h, func(req floorRequest) error {
				requests = append(requests, req)
				return nil
			})
			out := h.run("SessionStart", payload)
			assertReviewRefusal(t, h, out, "ended generation refused at "+marker)
			if want := []floorRequest{{route: "/v1/sessions", endedThrough: 2}}; !slices.Equal(requests, want) {
				t.Fatalf("SessionStart requests = %+v, want %+v", requests, want)
			}
		})
	}
}

func TestSessionStartChecksEndMarkerAgainBeforeUsingItsReply(t *testing.T) {
	for _, tc := range []struct {
		name, observation string
		damage            bool
	}{
		{name: "end recorded", observation: "ended generation refused"},
		{name: "marker damaged", observation: "damaged generation proof", damage: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := newRestoredHook(t, &hookClock{wall: time.Now()})
			h.prompt()
			first := h.registered
			proof := sessionsFor(h.getenv).name("claude", "s1")
			client := &hubclient.Client{Config: hubclient.EnvConfig{Getenv: h.getenv}}
			payload := `{"session_id":"s1","cwd":` + quote(h.root) + `}`
			wrapped := interceptedRegistration{Client: client, register: func(ctx context.Context, req hubclient.SessionRequest) (hubclient.SessionResponse, error) {
				sess, err := client.RegisterSession(ctx, req)
				if err != nil || sess.Generation != first.Generation {
					t.Errorf("SessionStart reply = %+v, %v", sess, err)
				}
				// The end happens after the hub accepted this registration, before the hook sees it.
				h.run("SessionEnd", payload)
				if tc.damage {
					if writeErr := os.WriteFile(proof+".ended", []byte("damaged"), 0o600); writeErr != nil {
						t.Errorf("damage end marker: %v", writeErr)
					}
				}
				return sess, err
			}}
			out := h.runner("SessionStart", payload, wrapped).Run()
			assertAllow(t, ClientClaude, out)
			assertReviewObservation(t, h, tc.observation+" at "+proof+".ended")
			if generation, err := readGeneration(proof); generation != 0 || err != nil {
				t.Fatalf("late SessionStart revived G%d, %v", generation, err)
			}
		})
	}
}

func TestRememberedGenerationRefusesDamageWhileWaitingForTheLock(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := newFileMemory(t)
		m.Remember("claude", "s1", 1)
		unlock, err := m.lock(hookLockBudget)
		if err != nil {
			t.Fatal(err)
		}
		result := make(chan error, 1)
		go func() {
			_, err := m.RecallGeneration("claude", "s1")
			result <- err
		}()
		synctest.Wait()
		writeErr := os.WriteFile(m.name("claude", "s1"), []byte("damaged"), 0o600)
		unlock()
		if writeErr != nil {
			t.Fatal(writeErr)
		}
		if err := <-result; err != ErrInvalidGeneration {
			t.Fatalf("a read started at G1 accepted damaged proof: %v", err)
		}
	})
}

func TestRegistrationProofRefusesMarkerDamagedWhileWaitingForLock(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := newFileMemory(t)
		m.Remember("claude", "s1", 2)
		unlock, err := m.lock(hookLockBudget)
		if err != nil {
			t.Fatal(err)
		}
		type result struct {
			generation, endedThrough int
			err                      error
			observations             []string
		}
		finished := make(chan result, 1)
		state := &runState{}
		go func() {
			generation, endedThrough, err := registrationProof(state, Event{Client: ClientClaude, SessionID: "s1", Kind: KindPrompt}, m, 0)
			finished <- result{generation, endedThrough, err, state.observations()}
		}()
		synctest.Wait()
		marker := m.name("claude", "s1") + ".ended"
		if err := os.WriteFile(marker, []byte("damaged\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		unlock()
		got := <-finished
		wantObservation := "damaged generation proof at " + marker // gitleaks:allow (a test observation token and path, not a credential)
		if got.generation != 0 || got.endedThrough != 0 || got.err != errEndedGeneration || !slices.Equal(got.observations, []string{wantObservation}) {
			t.Fatalf("registration with newly damaged marker = %+v, want local refusal naming the marker", got)
		}
	})
}
