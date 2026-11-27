package hook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/gitinfo"
	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/sessionproof"
)

func m2cExecID(n int) string {
	return fmt.Sprintf("ex_%032x", n)
}

func m2cClassifiedSession(execID string, gen int, priorID, source, proof string, hookSeen bool, caps ...string) hubclient.SessionResponse {
	if len(caps) == 0 {
		caps = []string{"shell_scope", "file_claims", hubclient.SuccessionProofCapability}
	}
	var prior hubclient.NullableString
	prior.Known = true
	if priorID != "" {
		prior.Valid = true
		prior.Value = priorID
	}
	var src hubclient.NullableString
	src.Known = true
	if source != "" {
		src.Valid = true
		src.Value = source
	}
	var prf hubclient.NullableString
	prf.Known = true
	if proof != "" {
		prf.Valid = true
		prf.Value = proof
	}
	return hubclient.SessionResponse{
		ExecutionID:  execID,
		SessionID:    "s1",
		Generation:   gen,
		Worktree:     "/repo",
		Capabilities: caps,
		Provenance: hubclient.Provenance{
			PriorExecutionID: prior,
			CreationSource:   src,
			CreationProof:    prf,
			HookSeen:         hubclient.OptionalBool{Known: true, Value: hookSeen},
		},
	}
}

type m2cHub struct {
	mu           sync.Mutex
	trace        []string
	regReqs      []hubclient.SessionRequest
	regFunc      func(context.Context, hubclient.SessionRequest) (hubclient.SessionResponse, error)
	outsideFunc  func(context.Context, hubclient.SessionRequest) (hubclient.SessionResponse, error)
	touchFunc    func(context.Context, hubclient.TouchRequest) (hubclient.TouchResponse, error)
	filesFunc    func(context.Context, hubclient.TouchFilesRequest) (hubclient.TouchResponse, error)
	endFunc      func(context.Context, string, int) error
	reconcileFn  func(context.Context, hubclient.ReconcileRequest) (hubclient.CheckResponse, error)
	combinedFn   func(context.Context, hubclient.ShellSessionRequest) (hubclient.ShellResponse, error)
	scopedFn     func(context.Context, hubclient.ScopedRequest) (hubclient.CheckResponse, error)
	scopedCombFn func(context.Context, hubclient.ScopedSessionRequest) (hubclient.ShellResponse, error)
}

func (h *m2cHub) addTrace(entry string) {
	h.mu.Lock()
	h.trace = append(h.trace, entry)
	h.mu.Unlock()
}

func (h *m2cHub) RegisterSession(ctx context.Context, r hubclient.SessionRequest) (hubclient.SessionResponse, error) {
	h.mu.Lock()
	h.regReqs = append(h.regReqs, r)
	h.trace = append(h.trace, fmt.Sprintf("register:gen=%d:retry=%d:end=%d:opt=%t", r.Generation, r.RetryGeneration, r.EndedThrough, r.SuccessionProof))
	h.mu.Unlock()
	if h.regFunc != nil {
		return h.regFunc(ctx, r)
	}
	return m2cClassifiedSession(m2cExecID(1), 1, "", "hook", "initial", true), nil
}

func (h *m2cHub) RegisterOutsideSession(ctx context.Context, r hubclient.SessionRequest) (hubclient.SessionResponse, error) {
	h.mu.Lock()
	h.regReqs = append(h.regReqs, r)
	h.trace = append(h.trace, fmt.Sprintf("outside:gen=%d:retry=%d:end=%d:opt=%t", r.Generation, r.RetryGeneration, r.EndedThrough, r.SuccessionProof))
	h.mu.Unlock()
	if h.outsideFunc != nil {
		return h.outsideFunc(ctx, r)
	}
	return h.RegisterSession(ctx, r)
}

func (h *m2cHub) Touch(ctx context.Context, r hubclient.TouchRequest) (hubclient.TouchResponse, error) {
	h.addTrace(fmt.Sprintf("touch:id=%s:gen=%d", r.ExecutionID, r.Generation))
	if h.touchFunc != nil {
		return h.touchFunc(ctx, r)
	}
	return hubclient.TouchResponse{Action: "allow"}, nil
}

func (h *m2cHub) TouchFiles(ctx context.Context, r hubclient.TouchFilesRequest) (hubclient.TouchResponse, error) {
	h.addTrace(fmt.Sprintf("touch_files:id=%s:gen=%d", r.ExecutionID, r.Generation))
	if h.filesFunc != nil {
		return h.filesFunc(ctx, r)
	}
	return hubclient.TouchResponse{Action: "allow"}, nil
}

func (h *m2cHub) Claim(context.Context, hubclient.TouchRequest) (hubclient.TouchResponse, error) {
	return hubclient.TouchResponse{}, nil
}
func (h *m2cHub) Release(context.Context, string, int, string) error { return nil }
func (h *m2cHub) EndExecution(ctx context.Context, id string, gen int) error {
	h.addTrace(fmt.Sprintf("end:id=%s:gen=%d", id, gen))
	if h.endFunc != nil {
		return h.endFunc(ctx, id, gen)
	}
	return nil
}

func (h *m2cHub) Reconcile(ctx context.Context, r hubclient.ReconcileRequest) (hubclient.CheckResponse, error) {
	h.addTrace(fmt.Sprintf("reconcile:id=%s:gen=%d", r.ExecutionID, r.Generation))
	if h.reconcileFn != nil {
		return h.reconcileFn(ctx, r)
	}
	return hubclient.CheckResponse{}, nil
}

type m2cCombinedHub struct{ *m2cHub }

func (h m2cCombinedHub) ReconcileSession(ctx context.Context, r hubclient.ShellSessionRequest) (hubclient.ShellResponse, error) {
	h.addTrace(fmt.Sprintf("reconcile_session:gen=%d:retry=%d:end=%d:opt=%t", r.Generation, r.RetryGeneration, r.EndedThrough, r.SuccessionProof))
	if h.combinedFn != nil {
		return h.combinedFn(ctx, r)
	}
	return hubclient.ShellResponse{Session: m2cClassifiedSession(m2cExecID(1), 1, "", "hook", "initial", true)}, nil
}

type m2cScopedCombinedHub struct{ *m2cHub }

func (h m2cScopedCombinedHub) ReconcileScoped(ctx context.Context, r hubclient.ScopedRequest) (hubclient.CheckResponse, error) {
	h.addTrace(fmt.Sprintf("reconcile_scoped:id=%s:gen=%d", r.ExecutionID, r.Generation))
	if h.scopedFn != nil {
		return h.scopedFn(ctx, r)
	}
	return hubclient.CheckResponse{}, nil
}

func (h m2cScopedCombinedHub) ReconcileScopedSession(ctx context.Context, r hubclient.ScopedSessionRequest) (hubclient.ShellResponse, error) {
	h.addTrace(fmt.Sprintf("reconcile_scoped_session:gen=%d:retry=%d:end=%d:opt=%t", r.Generation, r.RetryGeneration, r.EndedThrough, r.SuccessionProof))
	if h.scopedCombFn != nil {
		return h.scopedCombFn(ctx, r)
	}
	return hubclient.ShellResponse{Session: m2cClassifiedSession(m2cExecID(1), 1, "", "hook", "initial", true)}, nil
}

func m2cReadExact(t *testing.T, m fileMemory, client, session string) (sessionproof.Record, bool) {
	t.Helper()
	raw, err := os.ReadFile(executionPath(m.name(client, session)))
	if errors.Is(err, os.ErrNotExist) {
		return sessionproof.Record{}, false
	}
	if err != nil {
		t.Fatal(err)
	}
	rec, err := sessionproof.DecodeRecord(raw)
	if err != nil {
		t.Fatal(err)
	}
	return rec, true
}

func m2cWriteExact(t *testing.T, m fileMemory, client, session string, rec sessionproof.Record) {
	t.Helper()
	if err := os.MkdirAll(m.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	raw, err := sessionproof.EncodeRecord(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(executionPath(m.name(client, session)), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestSuccessionON_AdmissionPublicationConflictAndDamage(t *testing.T) {
	t.Run("ordinary registration publishes .execution before <hash> and skips idempotent rewrite", func(t *testing.T) {
		root := gitRepo(t)
		getenv := failureEnv(t)
		m := sessionsFor(getenv)
		var events []string
		m.pause = func(time.Duration) {}
		// Intercept restrict only for tracking write order (.tmp-* inside sessionsDir -> target).
		origRestrict := m.restrict
		m.restrict = func(path string) error {
			raw, err := os.ReadFile(path)
			if err == nil {
				if strings.Contains(string(raw), `"origin":"hook"`) {
					events = append(events, "prepare:exec")
				} else if strings.TrimSpace(string(raw)) == "2" {
					events = append(events, "prepare:gen")
				}
			}
			if origRestrict != nil {
				return origRestrict(path)
			}
			return nil
		}
		h := &m2cHub{
			regFunc: func(context.Context, hubclient.SessionRequest) (hubclient.SessionResponse, error) {
				events = append(events, "hub:register")
				return m2cClassifiedSession(m2cExecID(10), 2, m2cExecID(9), "hook", "stale_retry", true), nil
			},
		}
		r := Runner{
			ClientName: "claude", EventName: "SessionStart",
			Stdin: strings.NewReader(`{"session_id":"s1","cwd":` + quote(root) + `,"hook_event_name":"SessionStart"}`),
			Hub:   h, Memory: m, Getenv: getenv, SuccessionEnabled: func(context.Context) bool { return true },
			after: never, logAfter: never,
		}
		out := r.Run()
		if fails, _ := ReadRecentFailures(getenv("IRIS_TOKEN_FILE"), 10); len(fails) > 0 {
			t.Fatalf("unexpected failures: %+v (out=%s)", fails, out.Stdout)
		}
		if out.Exit != 0 || !json.Valid(out.Stdout) {
			t.Fatalf("bad output: %+v", out)
		}
		if !slices.Equal(events, []string{"hub:register", "prepare:exec", "prepare:gen"}) {
			t.Fatalf("unexpected write ordering: %v", events)
		}
		rec, ok := m2cReadExact(t, m, "claude", "s1")
		if !ok || rec.Origin != sessionproof.OriginHook || rec.ExecutionID != m2cExecID(10) || rec.Generation != 2 {
			t.Fatalf("unexpected .execution record: ok=%t rec=%+v", ok, rec)
		}
		if got := m.Recall("claude", "s1"); got != 2 {
			t.Fatalf("Remembered generation=%d, want 2", got)
		}

		// Idempotent re-run must not rewrite identical .execution or <hash>.
		events = nil
		r.Stdin = strings.NewReader(editPayload(ClientClaude, root))
		r.EventName = "PreToolUse"
		assertAllow(t, ClientClaude, r.Run())
		if len(events) != 1 || events[0] != "hub:register" {
			t.Fatalf("expected identical evidence rewrite skipped, got %v", events)
		}
	})

	t.Run("candidate legacy admitted and shim succession verified against .execution", func(t *testing.T) {
		root := gitRepo(t)
		getenv := failureEnv(t)
		m := sessionsFor(getenv)
		// Seed shim_succession exact record at G2, no <hash> yet.
		shimRec := sessionproof.Record{
			Version: sessionproof.FormatVersion, Origin: sessionproof.OriginShim,
			Generation: 2, ExecutionID: m2cExecID(20),
			PriorGeneration: 1, PriorExecutionID: m2cExecID(19),
		}
		m2cWriteExact(t, m, "claude", "s1", shimRec)

		h := &m2cHub{
			regFunc: func(context.Context, hubclient.SessionRequest) (hubclient.SessionResponse, error) {
				return m2cClassifiedSession(m2cExecID(20), 2, m2cExecID(19), "shim", "shim_succession", true), nil
			},
		}
		out := Runner{
			ClientName: "claude", EventName: "SessionStart",
			Stdin: strings.NewReader(`{"session_id":"s1","cwd":` + quote(root) + `,"hook_event_name":"SessionStart"}`),
			Hub:   h, Memory: m, Getenv: getenv, SuccessionEnabled: func(context.Context) bool { return true },
			after: never, logAfter: never,
		}.Run()
		if out.Exit != 0 || !json.Valid(out.Stdout) {
			t.Fatalf("unexpected SessionStart output: %+v", out)
		}
		// Must preserve OriginShim .execution and write <hash>=2.
		rec, ok := m2cReadExact(t, m, "claude", "s1")
		if !ok || rec.Origin != sessionproof.OriginShim || rec.ExecutionID != m2cExecID(20) {
			t.Fatalf("shim .execution overwritten: %+v", rec)
		}
		if got := m.Recall("claude", "s1"); got != 2 {
			t.Fatalf("generation=%d want 2", got)
		}

		// Legacy NULL provenance row on stage-1b hub is admitted.
		var leg hubclient.SessionResponse
		leg.ExecutionID = m2cExecID(21)
		leg.SessionID = "s2"
		leg.Generation = 1
		leg.Worktree = root
		leg.Capabilities = []string{hubclient.SuccessionProofCapability}
		leg.PriorExecutionID.Known = true
		leg.CreationSource.Known = true
		leg.CreationProof.Known = true
		leg.HookSeen = hubclient.OptionalBool{Known: true, Value: true}
		h.regFunc = func(context.Context, hubclient.SessionRequest) (hubclient.SessionResponse, error) { return leg, nil }
		out2 := Runner{
			ClientName: "claude", EventName: "SessionStart",
			Stdin: strings.NewReader(`{"session_id":"s2","cwd":` + quote(root) + `,"hook_event_name":"SessionStart"}`),
			Hub:   h, Memory: m, Getenv: getenv, SuccessionEnabled: func(context.Context) bool { return true },
			after: never, logAfter: never,
		}.Run()
		if out2.Exit != 0 || !strings.Contains(string(out2.Stdout), m2cExecID(21)) {
			t.Fatalf("expected admitted handle in SessionStart output: %s", out2.Stdout)
		}
		if rec2, ok2 := m2cReadExact(t, m, "claude", "s2"); !ok2 || rec2.Origin != sessionproof.OriginHook || rec2.ExecutionID != m2cExecID(21) {
			t.Fatalf("legacy execution not published: %+v", rec2)
		}
	})

	t.Run("rejects unusable M1 3-key, stage 1a unclassified, hookless shim, and conflicting exact record", func(t *testing.T) {
		for _, tc := range []struct {
			name     string
			seedExec *sessionproof.Record
			reply    hubclient.SessionResponse
		}{
			{
				name: "m1 incomplete 3-key reply",
				reply: hubclient.SessionResponse{
					ExecutionID: m2cExecID(30), SessionID: "s1", Generation: 1, Worktree: "/repo",
					Capabilities: []string{hubclient.SuccessionProofCapability},
					Provenance: hubclient.Provenance{
						PriorExecutionID: hubclient.NullableString{Known: true},
						CreationSource:   hubclient.NullableString{Known: true, Valid: true, Value: "hook"},
						CreationProof:    hubclient.NullableString{Known: true, Valid: true, Value: "initial"},
					},
				},
			},
			{
				name: "stage 1a unclassified reply",
				reply: hubclient.SessionResponse{
					ExecutionID: m2cExecID(31), SessionID: "s1", Generation: 1, Worktree: "/repo",
					Capabilities: []string{"shell_scope", "file_claims"},
				},
			},
			{
				name:  "shim succession missing local exact record",
				reply: m2cClassifiedSession(m2cExecID(32), 2, m2cExecID(31), "shim", "shim_succession", true),
			},
			{
				name:  "shim succession with hook_seen false",
				reply: m2cClassifiedSession(m2cExecID(33), 2, m2cExecID(31), "shim", "shim_succession", false),
			},
			{
				name:  "invalid execution id format on ON",
				reply: m2cClassifiedSession("bad_exec", 1, "", "hook", "initial", true),
			},
			{
				name: "higher generation exact record preserved on hub conflict",
				seedExec: &sessionproof.Record{
					Version: sessionproof.FormatVersion, Origin: sessionproof.OriginHook,
					Generation: 4, ExecutionID: m2cExecID(34),
				},
				reply: m2cClassifiedSession(m2cExecID(35), 3, "", "hook", "initial", true),
			},
			{
				name: "same generation different execution id preserved",
				seedExec: &sessionproof.Record{
					Version: sessionproof.FormatVersion, Origin: sessionproof.OriginShim,
					Generation: 2, ExecutionID: m2cExecID(36), PriorGeneration: 1, PriorExecutionID: m2cExecID(35),
				},
				reply: m2cClassifiedSession(m2cExecID(37), 2, "", "hook", "initial", true),
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				root := gitRepo(t)
				getenv := failureEnv(t)
				m := sessionsFor(getenv)
				if tc.seedExec != nil {
					m2cWriteExact(t, m, "claude", "s1", *tc.seedExec)
				}
				h := &m2cHub{
					regFunc: func(context.Context, hubclient.SessionRequest) (hubclient.SessionResponse, error) {
						return tc.reply, nil
					},
				}
				out := Runner{
					ClientName: "claude", EventName: "PreToolUse",
					Stdin: strings.NewReader(editPayload(ClientClaude, root)),
					Hub:   h, Memory: m, Getenv: getenv, SuccessionEnabled: func(context.Context) bool { return true },
					after: never, logAfter: never,
				}.Run()
				assertAllow(t, ClientClaude, out)
				for _, tr := range h.trace {
					if strings.HasPrefix(tr, "touch") {
						t.Fatalf("vetoed reply still touched files: %v", h.trace)
					}
				}
				if tc.seedExec != nil {
					got, ok := m2cReadExact(t, m, "claude", "s1")
					if !ok || got != *tc.seedExec {
						t.Fatalf("seeded .execution altered: got=%+v want=%+v", got, *tc.seedExec)
					}
				} else if _, ok := m2cReadExact(t, m, "claude", "s1"); ok {
					t.Fatal("rejected reply wrote .execution")
				}
				if got := m.Recall("claude", "s1"); got != 0 {
					t.Fatalf("rejected reply wrote <hash> G%d", got)
				}
				fails, err := ReadRecentFailures(getenv("IRIS_TOKEN_FILE"), 10)
				if err != nil || len(fails) == 0 || fails[0].Degraded() {
					t.Fatalf("expected non-degraded silent failure, got %+v err=%v", fails, err)
				}
			})
		}
	})

	t.Run("Row 2 confirmed end plus damage emits observation, silent failure, zero RPCs, and vetoes write", func(t *testing.T) {
		for _, damageExec := range []bool{false, true} {
			name := "damaged_hash_after_end"
			if damageExec {
				name = "damaged_exec_after_end"
			}
			t.Run(name, func(t *testing.T) {
				root := gitRepo(t)
				getenv := failureEnv(t)
				m := sessionsFor(getenv)
				m.Forget("claude", "s1", 3)
				target := m.name("claude", "s1")
				if damageExec {
					target = executionPath(target)
				}
				if err := os.WriteFile(target, []byte("corrupt"), 0o600); err != nil {
					t.Fatal(err)
				}
				h := &m2cHub{}
				out := Runner{
					ClientName: "claude", EventName: "SessionStart",
					Stdin: strings.NewReader(`{"session_id":"s1","cwd":` + quote(root) + `,"hook_event_name":"SessionStart"}`),
					Hub:   h, Memory: m, Getenv: getenv, SuccessionEnabled: func(context.Context) bool { return true },
					after: never, logAfter: never,
				}.Run()
				assertAllow(t, ClientClaude, out)
				if len(h.trace) != 0 {
					t.Fatalf("Row 2 made hub RPCs: %v", h.trace)
				}
				obs, err := ReadRecentObservations(getenv("IRIS_TOKEN_FILE"), 10)
				if err != nil || len(obs) != 1 || obs[0].Error != DamagedGenerationProof+" at "+target {
					t.Fatalf("observations=%+v err=%v want %q", obs, err, DamagedGenerationProof+" at "+target)
				}
				fails, err := ReadRecentFailures(getenv("IRIS_TOKEN_FILE"), 10)
				if err != nil || len(fails) != 1 || fails[0].Degraded() || fails[0].Stage != "session" {
					t.Fatalf("Row 2 must record non-degraded silent failure: fails=%+v err=%v", fails, err)
				}
			})
		}
	})

	t.Run("ON proof damage without confirmed end observes and skips hub with zero RPCs", func(t *testing.T) {
		for _, execDamaged := range []bool{false, true} {
			root := gitRepo(t)
			getenv := failureEnv(t)
			m := sessionsFor(getenv)
			if err := os.MkdirAll(m.dir, 0o700); err != nil {
				t.Fatal(err)
			}
			target := m.name("claude", "s1")
			if execDamaged {
				target = executionPath(target)
			}
			if err := os.WriteFile(target, []byte("corrupt"), 0o600); err != nil {
				t.Fatal(err)
			}
			h := &m2cHub{}
			out := Runner{
				ClientName: "claude", EventName: "PreToolUse",
				Stdin: strings.NewReader(editPayload(ClientClaude, root)),
				Hub:   h, Memory: m, Getenv: getenv, SuccessionEnabled: func(context.Context) bool { return true },
				after: never, logAfter: never,
			}.Run()
			assertAllow(t, ClientClaude, out)
			if len(h.trace) != 0 {
				t.Fatalf("damaged proof called hub: %v", h.trace)
			}
			obs, err := ReadRecentObservations(getenv("IRIS_TOKEN_FILE"), 10)
			if err != nil || len(obs) != 1 || obs[0].Error != DamagedGenerationProof+" at "+target {
				t.Fatalf("observations=%+v err=%v", obs, err)
			}
		}
	})
}

func TestFreshRegistrationLockContentionWatchdogAndSettleON(t *testing.T) {
	t.Run("fresh registration lock contention hits min(ctxDeadline, 50ms) and records silent", func(t *testing.T) {
		root := gitRepo(t)
		getenv := failureEnv(t)
		m := sessionsFor(getenv)
		release, err := m.lock(time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer release()

		h := &m2cHub{}
		start := time.Now()
		out := Runner{
			ClientName: "claude", EventName: "SessionStart",
			Stdin: strings.NewReader(`{"session_id":"s1","cwd":` + quote(root) + `,"hook_event_name":"SessionStart"}`),
			Hub:   h, Memory: m, Getenv: getenv,
			SuccessionEnabled: func(context.Context) bool { return true },
			Deadline:          25 * time.Millisecond,
			logAfter:          never,
		}.Run()
		elapsed := time.Since(start)
		assertAllow(t, ClientClaude, out)
		if elapsed > 200*time.Millisecond {
			t.Fatalf("lock wait ignored bounded deadline: took %s", elapsed)
		}
		if len(h.trace) != 0 {
			t.Fatalf("lock-contended ON fresh registration called hub: %v", h.trace)
		}
		fails, err := ReadRecentFailures(getenv("IRIS_TOKEN_FILE"), 10)
		if err != nil || len(fails) == 0 || fails[0].Degraded() || fails[0].Stage != "session" {
			t.Fatalf("expected non-degraded mandatory silent failure, got %+v err=%v", fails, err)
		}
	})

	t.Run("mandatoryActive watchdog firing first grades outcome silent", func(t *testing.T) {
		root := gitRepo(t)
		getenv := failureEnv(t)
		m := sessionsFor(getenv)
		// Hold lock without calling lock inside test? Hold lock longer than watchdog channel.
		release, err := m.lock(time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer release()

		// Deterministic after channel fires while inspectEvidenceDeadline holds Wait inside .lock.
		fire := make(chan time.Time, 1)
		go func() {
			time.Sleep(15 * time.Millisecond)
			fire <- time.Now()
		}()
		h := &m2cHub{}
		out := Runner{
			ClientName: "claude", EventName: "SessionStart",
			Stdin: strings.NewReader(`{"session_id":"s1","cwd":` + quote(root) + `,"hook_event_name":"SessionStart"}`),
			Hub:   h, Memory: m, Getenv: getenv,
			SuccessionEnabled: func(context.Context) bool { return true },
			Deadline:          500 * time.Millisecond,
			after:             func(time.Duration) <-chan time.Time { return fire },
			logAfter:          never,
		}.Run()
		assertAllow(t, ClientClaude, out)
		fails, err := ReadRecentFailures(getenv("IRIS_TOKEN_FILE"), 10)
		if err != nil || len(fails) == 0 || fails[0].Stage != "session" || fails[0].Degraded() || fails[0].Error != "hook deadline exceeded" {
			t.Fatalf("watchdog while mandatoryActive did not grade silent: %+v err=%v", fails, err)
		}
	})

	t.Run("shell settle must not mask mandatoryActive or mandatoryErr as shellOverBudget", func(t *testing.T) {
		root := gitRepo(t)
		getenv := failureEnv(t)
		m := sessionsFor(getenv)

		now := time.Now()
		var clock atomic.Int64
		clock.Store(now.UnixNano())
		var once sync.Once
		entered := make(chan struct{})
		releasePause := make(chan struct{})
		var pauseOnce sync.Once
		unpause := func() { pauseOnce.Do(func() { close(releasePause) }) }
		defer unpause()
		m.mono = func() time.Time { return time.Unix(0, clock.Load()) }
		m.pause = func(time.Duration) {
			once.Do(func() { close(entered) })
			<-releasePause
			clock.Store(now.Add(time.Second).UnixNano())
		}
		var releaseOnce func()
		h := m2cCombinedHub{&m2cHub{}}
		timeoutFired := make(chan struct{})
		shellOut := Runner{
			ClientName: "claude", EventName: "PreToolUse",
			Stdin: strings.NewReader(fmt.Sprintf(`{"session_id":"s1","cwd":%q,"tool_name":"Bash","tool_input":{"command":"echo hi"}}`, root)),
			Hub:   h, Memory: m, Getenv: getenv,
			Find: func(string) (gitinfo.Repo, error) { return gitinfo.Repo{Worktree: root}, nil },
			Status: func(context.Context, string, gitinfo.Scope, *gitinfo.Attempt) (map[string]string, error) {
				// Lock AFTER toldRevision so the contended acquire happens inside preInspectON (mandatoryActive).
				rel, err := m.lock(time.Second)
				if err != nil {
					return nil, err
				}
				releaseOnce = sync.OnceFunc(rel)
				return map[string]string{}, nil
			},
			SuccessionEnabled: func(context.Context) bool { return true },
			Now:               m.mono,
			Deadline:          time.Second,
			ShellDeadline:     time.Second,
			after:             never,
			logAfter:          never,
			shellTimeout: func(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
				ctx, cancel := context.WithCancel(parent)
				go func() {
					<-entered
					close(timeoutFired)
					cancel()
				}()
				return ctx, cancel
			},
		}.Run()
		<-timeoutFired
		unpause()
		if releaseOnce != nil {
			defer releaseOnce()
			releaseOnce()
		}
		assertAllow(t, ClientClaude, shellOut)
		fails, err := ReadRecentFailures(getenv("IRIS_TOKEN_FILE"), 10)
		if err != nil || len(fails) == 0 || fails[0].Degraded() || fails[0].Stage != "shell session" {
			t.Fatalf("settle masked silent as degraded: %+v err=%v", fails, err)
		}
	})
}

func TestPreToolEditFileGroupsSeparationAndSessionEnd(t *testing.T) {
	t.Run("fileGroups sees unpublished proof and intervening end marker vetoes TouchFiles", func(t *testing.T) {
		parent := t.TempDir()
		root := filepath.Join(parent, "main")
		sibling := filepath.Join(parent, "sib")
		for _, dir := range []string{root, sibling} {
			if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o700); err != nil {
				t.Fatal(err)
			}
		}
		getenv := failureEnv(t)
		m := sessionsFor(getenv)
		m.Remember("claude", "s1", 2)
		h := &m2cHub{}

		r := Runner{
			ClientName: "claude", EventName: "PreToolUse",
			Stdin: strings.NewReader(fmt.Sprintf(`{"session_id":"s1","cwd":%q,"hook_event_name":"PreToolUse","tool_name":"move_file","tool_input":{"rename_from":%q,"rename_to":%q}}`, root, filepath.Join(root, "src", "a.go"), filepath.Join(sibling, "b.go"))),
			Hub:   h, Memory: m, Getenv: getenv,
			SuccessionEnabled: func(context.Context) bool { return true },
			after:             never, logAfter: never,
			Find: func(dir string) (gitinfo.Repo, error) {
				if dir == sibling || strings.HasPrefix(dir, sibling) {
					if _, ok := m2cReadExact(t, m, "claude", "s1"); ok {
						t.Fatal("proof published before fileGroups completed")
					}
					// An intervening SessionEnd writes .ended=3!
					m.Forget("claude", "s1", 3)
					gd := filepath.Join(sibling, ".git")
					return gitinfo.Repo{Worktree: sibling, GitDir: gd, CommonDir: gd, WorktreeID: "2:2"}, nil
				}
				gd := filepath.Join(root, ".git")
				return gitinfo.Repo{Worktree: root, GitDir: gd, CommonDir: gd, WorktreeID: "1:1"}, nil
			},
		}
		h.regFunc = func(context.Context, hubclient.SessionRequest) (hubclient.SessionResponse, error) {
			return m2cClassifiedSession(m2cExecID(40), 3, m2cExecID(39), "hook", "stale_retry", true), nil
		}
		out := r.Run()
		if len(h.trace) != 1 || !strings.HasPrefix(h.trace[0], "register") {
			t.Fatalf("expected only register (no TouchFiles after veto), got %v", h.trace)
		}
		if _, ok := m2cReadExact(t, m, "claude", "s1"); ok {
			t.Fatal("vetoed edit still published .execution")
		}
		var dec struct {
			HookSpecificOutput struct {
				PermissionDecision string `json:"permissionDecision"`
			} `json:"hookSpecificOutput"`
		}
		if err := json.Unmarshal(out.Stdout, &dec); err != nil || Action(dec.HookSpecificOutput.PermissionDecision) != ActionDeny {
			t.Fatalf("expected deny on ended session, got %s err=%v", out.Stdout, err)
		}
	})

	t.Run("exact SessionEnd ON publishes before EndExecution then forgets exact and cleans on success", func(t *testing.T) {
		root := gitRepo(t)
		getenv := failureEnv(t)
		m := sessionsFor(getenv)
		h := &m2cHub{}
		var recBeforeEnd sessionproof.Record
		var hadBeforeEnd bool
		h.regFunc = func(context.Context, hubclient.SessionRequest) (hubclient.SessionResponse, error) {
			return m2cClassifiedSession(m2cExecID(50), 2, m2cExecID(49), "hook", "stale_retry", true), nil
		}
		h.endFunc = func(context.Context, string, int) error {
			recBeforeEnd, hadBeforeEnd = m2cReadExact(t, m, "claude", "s1")
			return nil
		}
		out := Runner{
			ClientName: "claude", EventName: "SessionEnd",
			Stdin: strings.NewReader(`{"session_id":"s1","cwd":` + quote(root) + `,"hook_event_name":"SessionEnd"}`),
			Hub:   h, Memory: m, Getenv: getenv, SuccessionEnabled: func(context.Context) bool { return true },
			after: never, logAfter: never,
		}.Run()
		if fails, _ := ReadRecentFailures(getenv("IRIS_TOKEN_FILE"), 10); len(fails) > 0 {
			t.Fatalf("unexpected SessionEnd failures: %+v trace=%v", fails, h.trace)
		}
		assertAllow(t, ClientClaude, out)
		if !hadBeforeEnd || recBeforeEnd.ExecutionID != m2cExecID(50) {
			t.Fatalf("expected finalAdmitAndPublish before EndExecution, got ok=%t rec=%+v trace=%v", hadBeforeEnd, recBeforeEnd, h.trace)
		}
		if _, ok := m2cReadExact(t, m, "claude", "s1"); ok {
			t.Fatal("forgetEvidence did not remove .execution after EndExecution")
		}
		if got := m.Recall("claude", "s1"); got != 0 {
			t.Fatalf("forgetEvidence left generation=%d", got)
		}
		ended, expired, err := m.readEndMarker(endedMarkerPath(m.name("claude", "s1")))
		if err != nil || expired || ended != 2 {
			t.Fatalf("expected .ended=2, got %d expired=%t err=%v", ended, expired, err)
		}
	})

	t.Run("OFF SessionEnd fails EndExecution keeps <hash> and writes zero .ended or .execution", func(t *testing.T) {
		root := gitRepo(t)
		getenv := failureEnv(t)
		m := sessionsFor(getenv)
		h := &m2cHub{
			regFunc: func(context.Context, hubclient.SessionRequest) (hubclient.SessionResponse, error) {
				// Return un-opted v0.1.15-shaped session.
				return hubclient.SessionResponse{ExecutionID: "ex_v0115", SessionID: "s1", Generation: 3, Worktree: root}, nil
			},
			endFunc: func(context.Context, string, int) error {
				return errors.New("hub down during EndExecution")
			},
		}
		out := Runner{
			ClientName: "claude", EventName: "SessionEnd",
			Stdin: strings.NewReader(`{"session_id":"s1","cwd":` + quote(root) + `,"hook_event_name":"SessionEnd"}`),
			Hub:   h, Memory: m, Getenv: getenv, SuccessionEnabled: func(context.Context) bool { return false },
			after: never, logAfter: never,
		}.Run()
		assertAllow(t, ClientClaude, out)
		if got := m.Recall("claude", "s1"); got != 3 {
			t.Fatalf("OFF SessionEnd failure dropped retained gen: got %d want 3", got)
		}
		if _, err := os.Stat(endedMarkerPath(m.name("claude", "s1"))); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("OFF SessionEnd failure wrote .ended: %v", err)
		}
		if _, ok := m2cReadExact(t, m, "claude", "s1"); ok {
			t.Fatal("OFF SessionEnd wrote .execution")
		}
	})
}

func TestCombinedToSplitFallbackAndLocalVetoStopsRewriteAndRetry(t *testing.T) {
	t.Run("shellFull combined to split carries floors and stops second retry on local veto", func(t *testing.T) {
		root := gitRepo(t)
		getenv := failureEnv(t)
		m := sessionsFor(getenv)
		m.Remember("claude", "s1", 4)
		m2cWriteExact(t, m, "claude", "s1", sessionproof.Record{
			Version: sessionproof.FormatVersion, Origin: sessionproof.OriginHook, Generation: 4, ExecutionID: m2cExecID(60),
		})

		h := &m2cCombinedHub{&m2cHub{}}
		h.combinedFn = func(context.Context, hubclient.ShellSessionRequest) (hubclient.ShellResponse, error) {
			return hubclient.ShellResponse{}, &hubclient.Error{Code: "lost", Details: json.RawMessage(`{"highest_generation":4}`)}
		}
		calls := 0
		h.regFunc = func(_ context.Context, req hubclient.SessionRequest) (hubclient.SessionResponse, error) {
			calls++
			if calls == 1 {
				return hubclient.SessionResponse{}, &hubclient.Error{Code: "lost", Details: json.RawMessage(`{"highest_generation":4}`)}
			}
			// Retry succeeds at G5!
			return m2cClassifiedSession(m2cExecID(61), 5, m2cExecID(60), "hook", "stale_retry", true), nil
		}
		out := Runner{
			ClientName: "claude", EventName: "PreToolUse",
			Stdin: strings.NewReader(fmt.Sprintf(`{"session_id":"s1","cwd":%q,"tool_name":"Bash","tool_input":{"command":"echo ok"}}`, root)),
			Hub:   h, Memory: m, Getenv: getenv, SuccessionEnabled: func(context.Context) bool { return true },
			Find: func(string) (gitinfo.Repo, error) { return gitinfo.Repo{Worktree: root}, nil },
			Status: func(context.Context, string, gitinfo.Scope, *gitinfo.Attempt) (map[string]string, error) {
				return map[string]string{}, nil
			},
			ShellDeadline: time.Second, after: never, logAfter: never,
		}.Run()
		if fails, _ := ReadRecentFailures(getenv("IRIS_TOKEN_FILE"), 10); len(fails) > 0 {
			t.Fatalf("shellFull unexpected failures: %+v trace=%v", fails, h.trace)
		}
		assertAllow(t, ClientClaude, out)
		wantTrace := []string{
			"reconcile_session:gen=4:retry=0:end=0:opt=true",
			"register:gen=4:retry=0:end=0:opt=true",
			"register:gen=0:retry=4:end=0:opt=true",
			fmt.Sprintf("reconcile:id=%s:gen=5", m2cExecID(61)),
		}
		if !slices.Equal(h.trace, wantTrace) {
			t.Fatalf("trace=%v\nwant=%v", h.trace, wantTrace)
		}
	})

	t.Run("local veto stops restore-floor rewrite and any registration retry", func(t *testing.T) {
		root := gitRepo(t)
		getenv := failureEnv(t)
		m := sessionsFor(getenv)
		m.Remember("claude", "s1", 4)
		// Prune G4 and corrupt .ended during first RPC -> local veto must stop retry & retainGeneration!
		h := &m2cHub{
			regFunc: func(context.Context, hubclient.SessionRequest) (hubclient.SessionResponse, error) {
				_ = os.Remove(m.name("claude", "s1"))
				_ = os.WriteFile(endedMarkerPath(m.name("claude", "s1")), []byte("damaged"), 0o600)
				return hubclient.SessionResponse{}, &hubclient.Error{Code: "lost", Details: json.RawMessage(`{"highest_generation":4}`)}
			},
		}
		out := Runner{
			ClientName: "claude", EventName: "SessionStart",
			Stdin: strings.NewReader(`{"session_id":"s1","cwd":` + quote(root) + `,"hook_event_name":"SessionStart"}`),
			Hub:   h, Memory: m, Getenv: getenv, SuccessionEnabled: func(context.Context) bool { return true },
			after: never, logAfter: never,
		}.Run()
		if out.Exit != 0 || !json.Valid(out.Stdout) {
			t.Fatalf("unexpected SessionStart output: %+v", out)
		}
		if len(h.trace) != 1 {
			t.Fatalf("expected local veto to stop retry, got trace %v", h.trace)
		}
		if _, err := os.Stat(m.name("claude", "s1")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("local veto allowed restore-floor rewrite of <hash>: %v", err)
		}
	})
}
