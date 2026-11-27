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
		frozen := time.Unix(1_700_000_000, 0)
		m.mono = func() time.Time { return frozen }
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
			Find:  func(string) (gitinfo.Repo, error) { return gitinfo.Repo{Worktree: root}, nil },
			Now:   m.mono,
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
		frozen := time.Unix(1_700_000_000, 0)
		m.mono = func() time.Time { return frozen }
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
			Find:  func(string) (gitinfo.Repo, error) { return gitinfo.Repo{Worktree: root}, nil },
			Now:   m.mono,
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
			Find:  func(string) (gitinfo.Repo, error) { return gitinfo.Repo{Worktree: root}, nil },
			Now:   m.mono,
			after: never, logAfter: never,
		}.Run()
		if out2.Exit != 0 || !strings.Contains(string(out2.Stdout), m2cExecID(21)) {
			t.Fatalf("expected admitted handle in SessionStart output: %s", out2.Stdout)
		}
		if rec2, ok2 := m2cReadExact(t, m, "claude", "s2"); !ok2 || rec2.Origin != sessionproof.OriginHook || rec2.ExecutionID != m2cExecID(21) {
			t.Fatalf("legacy execution not published: %+v", rec2)
		}
	})

	t.Run("rejects unusable M1 3-key, hookless shim, and conflicting exact record", func(t *testing.T) {
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

	t.Run("admits stage 1a unclassified reply under ON without writing exact proof", func(t *testing.T) {
		root := gitRepo(t)
		getenv := failureEnv(t)
		m := sessionsFor(getenv)
		h := &m2cHub{
			regFunc: func(context.Context, hubclient.SessionRequest) (hubclient.SessionResponse, error) {
				return hubclient.SessionResponse{
					ExecutionID: m2cExecID(31), SessionID: "s1", Generation: 1, Worktree: "/repo",
					Capabilities: []string{"shell_scope", "file_claims"},
				}, nil
			},
		}
		out := Runner{
			ClientName: "claude", EventName: "PreToolUse",
			Stdin: strings.NewReader(editPayload(ClientClaude, root)),
			Hub:   h, Memory: m, Getenv: getenv, SuccessionEnabled: func(context.Context) bool { return true },
			after: never, logAfter: never,
		}.Run()
		assertAllow(t, ClientClaude, out)
		touched := false
		for _, tr := range h.trace {
			if strings.HasPrefix(tr, "touch") {
				touched = true
			}
		}
		if !touched {
			t.Fatalf("ordinary unclassified reply should reach Touch, got trace=%v", h.trace)
		}
		if got := m.Recall("claude", "s1"); got != 1 {
			t.Fatalf("ordinary unclassified reply wrote <hash>=%d, want 1", got)
		}
		if _, ok := m2cReadExact(t, m, "claude", "s1"); ok {
			t.Fatal("ordinary unclassified reply must not write .execution")
		}
		if fails, err := ReadRecentFailures(getenv("IRIS_TOKEN_FILE"), 10); err != nil || len(fails) != 0 {
			t.Fatalf("expected zero failures, got %+v err=%v", fails, err)
		}
	})

	t.Run("OFF rejects M1 incomplete 3-key reply with zero writes", func(t *testing.T) {
		root := gitRepo(t)
		getenv := failureEnv(t)
		m := sessionsFor(getenv)
		h := &m2cHub{
			regFunc: func(context.Context, hubclient.SessionRequest) (hubclient.SessionResponse, error) {
				return hubclient.SessionResponse{
					ExecutionID: m2cExecID(30), SessionID: "s1", Generation: 1, Worktree: "/repo",
					Capabilities: []string{hubclient.SuccessionProofCapability},
					Provenance: hubclient.Provenance{
						PriorExecutionID: hubclient.NullableString{Known: true},
						CreationSource:   hubclient.NullableString{Known: true, Valid: true, Value: "hook"},
						CreationProof:    hubclient.NullableString{Known: true, Valid: true, Value: "initial"},
					},
				}, nil
			},
		}
		out := Runner{
			ClientName: "claude", EventName: "PreToolUse",
			Stdin: strings.NewReader(editPayload(ClientClaude, root)),
			Hub:   h, Memory: m, Getenv: getenv, SuccessionEnabled: func(context.Context) bool { return false },
			after: never, logAfter: never,
		}.Run()
		assertAllow(t, ClientClaude, out)
		for _, tr := range h.trace {
			if strings.HasPrefix(tr, "touch") {
				t.Fatalf("rejected M1 reply under OFF still touched files: %v", h.trace)
			}
		}
		if got := m.Recall("claude", "s1"); got != 0 {
			t.Fatalf("rejected M1 reply under OFF wrote <hash> G%d", got)
		}
		if _, ok := m2cReadExact(t, m, "claude", "s1"); ok {
			t.Fatal("rejected M1 reply under OFF wrote .execution")
		}
		fails, err := ReadRecentFailures(getenv("IRIS_TOKEN_FILE"), 10)
		if err != nil || len(fails) == 0 || fails[0].Degraded() {
			t.Fatalf("expected non-degraded silent failure, got %+v err=%v", fails, err)
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
			frozen := time.Unix(1_700_000_000, 0)
			m.mono = func() time.Time { return frozen }
			if err := m.directory(); err != nil {
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
				Find:  func(string) (gitinfo.Repo, error) { return gitinfo.Repo{Worktree: root}, nil },
				Now:   m.mono,
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

	t.Run("OFF fresh SessionStart and PreToolUse lock contention hits acquireOnly and records silent", func(t *testing.T) {
		for _, tc := range []struct {
			name, event, payloadStage string
			makePayload               func(string) string
		}{
			{
				name: "SessionStart", event: "SessionStart", payloadStage: "session",
				makePayload: func(r string) string {
					return `{"session_id":"s1","cwd":` + quote(r) + `,"hook_event_name":"SessionStart"}`
				},
			},
			{
				name: "PreToolUse edit", event: "PreToolUse", payloadStage: "session",
				makePayload: func(r string) string {
					return editPayload(ClientClaude, r)
				},
			},
			{
				name: "PreToolUse shell", event: "PreToolUse", payloadStage: "shell session",
				makePayload: func(r string) string {
					return fmt.Sprintf(`{"session_id":"s1","cwd":%q,"hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":"echo hi"}}`, r)
				},
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
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
					ClientName: "claude", EventName: tc.event,
					Stdin: strings.NewReader(tc.makePayload(root)),
					Hub:   h, Memory: m, Getenv: getenv,
					Find: func(string) (gitinfo.Repo, error) { return gitinfo.Repo{Worktree: root}, nil },
					Status: func(context.Context, string, gitinfo.Scope, *gitinfo.Attempt) (map[string]string, error) {
						return map[string]string{}, nil
					},
					Deadline:      2 * time.Second,
					ShellDeadline: time.Second,
					after:         never,
					logAfter:      never,
				}.Run()
				elapsed := time.Since(start)
				assertAllow(t, ClientClaude, out)
				if elapsed > 200*time.Millisecond {
					t.Fatalf("OFF lock wait ignored bounded budget: took %s", elapsed)
				}
				if len(h.trace) != 0 {
					t.Fatalf("OFF lock-contended fresh registration called hub: %v", h.trace)
				}
				fails, err := ReadRecentFailures(getenv("IRIS_TOKEN_FILE"), 10)
				if err != nil || len(fails) == 0 {
					t.Fatalf("expected recorded failure, got %+v err=%v", fails, err)
				}
				matched := false
				for _, f := range fails {
					if f.Stage == tc.payloadStage && f.Error == string(failSessionsLock) && !f.Degraded() && !f.Skipped() {
						matched = true
					}
				}
				if !matched {
					t.Fatalf("expected non-degraded silent failure at %q, got %+v", tc.payloadStage, fails)
				}
			})
		}
	})

	t.Run("mandatoryActive watchdog firing first grades outcome silent", func(t *testing.T) {
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

		release, err := m.lock(time.Second)
		if err != nil {
			t.Fatal(err)
		}
		releaseOnce := sync.OnceFunc(release)
		defer releaseOnce()

		// Fire outer watchdog only once preInspectON is paused inside mandatory .lock read.
		fire := make(chan time.Time, 1)
		go func() {
			<-entered
			fire <- now
		}()
		h := &m2cHub{}
		out := Runner{
			ClientName: "claude", EventName: "PreToolUse",
			// Write skips toldRevision so preInspectON is the first .lock acquirer and enters mandatoryActive.
			Stdin: strings.NewReader(fmt.Sprintf(`{"session_id":"s1","cwd":%q,"hook_event_name":"PreToolUse","tool_name":"Write","tool_input":{"file_path":%q,"content":"x"}}`, root, filepath.Join(root, "a.go"))),
			Hub:   h, Memory: m, Getenv: getenv,
			SuccessionEnabled: func(context.Context) bool { return true },
			Now:               m.mono,
			Deadline:          time.Second,
			after:             func(time.Duration) <-chan time.Time { return fire },
			logAfter:          never,
		}.Run()
		unpause()
		releaseOnce()
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
			Find:  func(string) (gitinfo.Repo, error) { return gitinfo.Repo{Worktree: root}, nil },
			Now:   m.mono,
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

type m2cCustomMem struct {
	gen        int
	err        error
	remembered int
}

func (m *m2cCustomMem) RecallGeneration(string, string) (int, error) { return m.gen, m.err }
func (m *m2cCustomMem) Remember(_, _ string, gen int)                { m.remembered = gen }
func (m *m2cCustomMem) Forget(string, string, int)                   {}

func TestSuccessionAdmissionEdgeCasesAndMemoryBehaviors(t *testing.T) {
	t.Run("non-fileMemory handles RecallGeneration error and generation propagation", func(t *testing.T) {
		root := gitRepo(t)
		getenv := failureEnv(t)

		errMem := &m2cCustomMem{err: ErrInvalidGeneration}
		h := &m2cHub{}
		out := Runner{
			ClientName: "claude", EventName: "SessionStart",
			Stdin: strings.NewReader(`{"session_id":"s1","cwd":` + quote(root) + `,"hook_event_name":"SessionStart"}`),
			Hub:   h, Memory: errMem, Getenv: getenv,
			SuccessionEnabled: func(context.Context) bool { return true },
			after:             never, logAfter: never,
		}.Run()
		if out.Exit != 0 || len(h.trace) != 0 {
			t.Fatalf("expected non-fileMemory RecallGeneration err to veto hub calls: out=%+v trace=%v", out, h.trace)
		}

		okMem := &m2cCustomMem{gen: 2}
		h2 := &m2cHub{
			regFunc: func(context.Context, hubclient.SessionRequest) (hubclient.SessionResponse, error) {
				return m2cClassifiedSession(m2cExecID(2), 2, m2cExecID(1), "hook", "stale_retry", true), nil
			},
		}
		Runner{
			ClientName: "claude", EventName: "Prompt",
			Stdin: strings.NewReader(`{"session_id":"s1","cwd":` + quote(root) + `,"hook_event_name":"UserPromptSubmit","prompt":"hi"}`),
			Hub:   h2, Memory: okMem, Getenv: getenv,
			SuccessionEnabled: func(context.Context) bool { return true },
			after:             never, logAfter: never,
		}.Run()
		if len(h2.regReqs) != 1 || h2.regReqs[0].Generation != 2 || okMem.remembered != 2 {
			t.Fatalf("non-fileMemory regReqs=%+v remembered=%d", h2.regReqs, okMem.remembered)
		}
	})

	t.Run("preInspectON deadline after ended, damaged .ended, and ended session handling", func(t *testing.T) {
		origOpen := generationFileOpen
		t.Cleanup(func() { generationFileOpen = origOpen })

		// 1) Valid .ended=2 read, then generation read hits deadline inside inspectEvidenceDeadline.
		root := gitRepo(t)
		getenv := failureEnv(t)
		m := sessionsFor(getenv)
		now := time.Now()
		var clock atomic.Int64
		clock.Store(now.UnixNano())
		m.now = func() time.Time { return time.Unix(0, clock.Load()) }
		m.mono = m.now
		prefix := m.name("claude", "s1")
		if err := m.directory(); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(endedMarkerPath(prefix), []byte("2\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(prefix, []byte("2\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		generationFileOpen = func(path string) (*os.File, error) {
			if path == prefix {
				clock.Store(now.Add(time.Hour).UnixNano())
			}
			return OpenGenerationFile(path)
		}
		h := &m2cHub{}
		out := Runner{
			ClientName: "claude", EventName: "SessionStart",
			Stdin: strings.NewReader(`{"session_id":"s1","cwd":` + quote(root) + `,"hook_event_name":"SessionStart"}`),
			Hub:   h, Memory: m, Getenv: getenv, SuccessionEnabled: func(context.Context) bool { return true },
			Now: m.mono, Deadline: time.Second, after: never, logAfter: never,
		}.Run()
		assertAllow(t, ClientClaude, out)
		if len(h.trace) != 0 {
			t.Fatalf("expected zero RPCs on inspect deadline, got %v", h.trace)
		}
		if fails, _ := ReadRecentFailures(getenv("IRIS_TOKEN_FILE"), 10); len(fails) == 0 || fails[0].Degraded() {
			t.Fatalf("expected silent mandatory failure, got %+v", fails)
		}

		// 2) Damaged .ended in preInspectON vetoes without calling hub.
		generationFileOpen = origOpen
		getenv2 := failureEnv(t)
		m2 := sessionsFor(getenv2)
		if err := m2.directory(); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(endedMarkerPath(m2.name("claude", "s1")), []byte("bad\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		h2 := &m2cHub{}
		Runner{
			ClientName: "claude", EventName: "Prompt",
			Stdin: strings.NewReader(`{"session_id":"s1","cwd":` + quote(root) + `,"hook_event_name":"UserPromptSubmit","prompt":"hi"}`),
			Hub:   h2, Memory: m2, Getenv: getenv2, SuccessionEnabled: func(context.Context) bool { return true },
			after: never, logAfter: never,
		}.Run()
		if len(h2.trace) != 0 {
			t.Fatalf("damaged .ended preInspectON called hub: %v", h2.trace)
		}
		if obs, _ := ReadRecentObservations(getenv2("IRIS_TOKEN_FILE"), 10); len(obs) == 0 {
			t.Fatal("expected DamagedGenerationProof observation for damaged .ended")
		}

		// 3) Active .ended=2 + <hash>=2: SessionStart allowed with EndedThrough=2, Prompt refused locally.
		getenv3 := failureEnv(t)
		m3 := sessionsFor(getenv3)
		m3.Remember("claude", "s1", 2)
		m3.Forget("claude", "s1", 2)
		if err := os.WriteFile(m3.name("claude", "s1"), []byte("2\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		h3 := &m2cHub{
			regFunc: func(context.Context, hubclient.SessionRequest) (hubclient.SessionResponse, error) {
				return m2cClassifiedSession(m2cExecID(3), 3, "", "hook", "initial", true), nil
			},
		}
		Runner{
			ClientName: "claude", EventName: "SessionStart",
			Stdin: strings.NewReader(`{"session_id":"s1","cwd":` + quote(root) + `,"hook_event_name":"SessionStart"}`),
			Hub:   h3, Memory: m3, Getenv: getenv3, SuccessionEnabled: func(context.Context) bool { return true },
			after: never, logAfter: never,
		}.Run()
		if len(h3.regReqs) != 1 || h3.regReqs[0].Generation != 0 || h3.regReqs[0].EndedThrough != 2 {
			t.Fatalf("SessionStart over ended=2 reqs=%+v", h3.regReqs)
		}

		// 3b) admitReply rejects hub returning generation <= active endedFloor.
		geEndedReply := failureEnv(t)
		mfEndedReply := sessionsFor(geEndedReply)
		mfEndedReply.Remember("claude", "s1", 2)
		mfEndedReply.Forget("claude", "s1", 2)
		hEndedReply := &m2cHub{
			regFunc: func(context.Context, hubclient.SessionRequest) (hubclient.SessionResponse, error) {
				return m2cClassifiedSession(m2cExecID(3), 2, m2cExecID(2), "hook", "stale_retry", true), nil
			},
		}
		Runner{
			ClientName: "claude", EventName: "SessionStart",
			Stdin: strings.NewReader(`{"session_id":"s1","cwd":` + quote(root) + `,"hook_event_name":"SessionStart"}`),
			Hub:   hEndedReply, Memory: mfEndedReply, Getenv: geEndedReply, SuccessionEnabled: func(context.Context) bool { return true },
			Find:  func(string) (gitinfo.Repo, error) { return gitinfo.Repo{Worktree: root}, nil },
			after: never, logAfter: never,
		}.Run()
		if _, ok := m2cReadExact(t, mfEndedReply, "claude", "s1"); ok {
			t.Fatal("ended-floor reply unexpectedly published exact record")
		}
		if obs, _ := ReadRecentObservations(geEndedReply("IRIS_TOKEN_FILE"), 10); len(obs) == 0 {
			t.Fatal("expected ended observation when hub returned generation <= endedFloor")
		}

		getenv4 := failureEnv(t)
		m4 := sessionsFor(getenv4)
		m4.Remember("claude", "s1", 2)
		m4.Forget("claude", "s1", 2)
		if err := os.WriteFile(m4.name("claude", "s1"), []byte("2\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		h4 := &m2cHub{}
		Runner{
			ClientName: "claude", EventName: "Prompt",
			Stdin: strings.NewReader(`{"session_id":"s1","cwd":` + quote(root) + `,"hook_event_name":"UserPromptSubmit","prompt":"hi"}`),
			Hub:   h4, Memory: m4, Getenv: getenv4, SuccessionEnabled: func(context.Context) bool { return true },
			after: never, logAfter: never,
		}.Run()
		if len(h4.trace) != 0 {
			t.Fatalf("ended Prompt called hub: %v", h4.trace)
		}
	})

	t.Run("preInspectOFF, finalInspectOFF, and Remember expired cleanup / retainGeneration damage", func(t *testing.T) {
		root := gitRepo(t)

		// 1) D9 retry OFF when .ended becomes damaged during try 1 stops retry cleanly.
		getenv := failureEnv(t)
		m := sessionsFor(getenv)
		m.Remember("claude", "s1", 2)
		h := &m2cHub{
			regFunc: func(context.Context, hubclient.SessionRequest) (hubclient.SessionResponse, error) {
				_ = os.WriteFile(endedMarkerPath(m.name("claude", "s1")), []byte("bad\n"), 0o600)
				return hubclient.SessionResponse{}, &hubclient.Error{Code: "lost", Details: json.RawMessage(`{"highest_generation":2}`)}
			},
		}
		Runner{
			ClientName: "claude", EventName: "SessionStart",
			Stdin: strings.NewReader(`{"session_id":"s1","cwd":` + quote(root) + `,"hook_event_name":"SessionStart"}`),
			Hub:   h, Memory: m, Getenv: getenv, SuccessionEnabled: func(context.Context) bool { return false },
			after: never, logAfter: never,
		}.Run()
		if len(h.trace) != 1 {
			t.Fatalf("expected damaged .ended during D9 OFF retry to stop retry, got %v", h.trace)
		}

		// 1b) D9 retry OFF when <hash> becomes damaged during try 1 hits preInspectOFF + retainGeneration(proofErr != nil).
		ge1b := failureEnv(t)
		m1b := sessionsFor(ge1b)
		m1b.Remember("claude", "s1", 2)
		h1b := &m2cHub{
			regFunc: func(context.Context, hubclient.SessionRequest) (hubclient.SessionResponse, error) {
				if err := os.WriteFile(m1b.name("claude", "s1"), []byte("bad\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				return hubclient.SessionResponse{}, &hubclient.Error{Code: "lost", Details: json.RawMessage(`{"highest_generation":2}`)}
			},
		}
		Runner{
			ClientName: "claude", EventName: "SessionStart",
			Stdin: strings.NewReader(`{"session_id":"s1","cwd":` + quote(root) + `,"hook_event_name":"SessionStart"}`),
			Hub:   h1b, Memory: m1b, Getenv: ge1b, SuccessionEnabled: func(context.Context) bool { return false },
			after: never, logAfter: never,
		}.Run()
		if len(h1b.trace) != 1 {
			t.Fatalf("expected damaged <hash> on D9 retry to stop retry, got %v", h1b.trace)
		}
		if raw, err := os.ReadFile(m1b.name("claude", "s1")); err != nil || string(raw) != "bad\n" {
			t.Fatalf("retainGeneration overwrote damaged proof: %q err=%v", raw, err)
		}

		// 1c) Combined shell OFF returning lost below sentGen while <hash> became damaged preserves damaged proof in retainGeneration.
		ge1c := failureEnv(t)
		m1c := sessionsFor(ge1c)
		m1c.Remember("claude", "s1", 2)
		h1c := m2cCombinedHub{&m2cHub{
			combinedFn: func(context.Context, hubclient.ShellSessionRequest) (hubclient.ShellResponse, error) {
				if err := os.WriteFile(m1c.name("claude", "s1"), []byte("bad\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				return hubclient.ShellResponse{}, &hubclient.Error{Code: "lost", Details: json.RawMessage(`{"highest_generation":1}`)}
			},
		}}
		out1c := Runner{
			ClientName: "claude", EventName: "PreToolUse",
			Stdin: strings.NewReader(fmt.Sprintf(`{"session_id":"s1","cwd":%q,"tool_name":"Bash","tool_input":{"command":"echo hi"}}`, root)),
			Hub:   h1c, Memory: m1c, Getenv: ge1c, SuccessionEnabled: func(context.Context) bool { return false },
			Find: func(string) (gitinfo.Repo, error) { return gitinfo.Repo{Worktree: root}, nil },
			Status: func(context.Context, string, gitinfo.Scope, *gitinfo.Attempt) (map[string]string, error) {
				return map[string]string{}, nil
			},
			ShellDeadline: time.Second, after: never, logAfter: never,
		}.Run()
		assertAllow(t, ClientClaude, out1c)
		if raw, err := os.ReadFile(m1c.name("claude", "s1")); err != nil || string(raw) != "bad\n" {
			t.Fatalf("checkRestoreFloor retainGeneration overwrote damaged proof: %q err=%v", raw, err)
		}

		// 2) Row 2 OFF in preInspectOFF (confirmed .ended=2 + damaged <hash> -> silent + allow + zero RPCs).
		getenv2 := failureEnv(t)
		m2 := sessionsFor(getenv2)
		if err := m2.directory(); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(endedMarkerPath(m2.name("claude", "s1")), []byte("2\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(m2.name("claude", "s1"), []byte("bad\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		h2 := &m2cHub{}
		out2 := Runner{
			ClientName: "claude", EventName: "SessionStart",
			Stdin: strings.NewReader(`{"session_id":"s1","cwd":` + quote(root) + `,"hook_event_name":"SessionStart"}`),
			Hub:   h2, Memory: m2, Getenv: getenv2, SuccessionEnabled: func(context.Context) bool { return false },
			after: never, logAfter: never,
		}.Run()
		assertAllow(t, ClientClaude, out2)
		if len(h2.trace) != 0 {
			t.Fatalf("Row 2 OFF called hub: %v", h2.trace)
		}
		if fails, _ := ReadRecentFailures(getenv2("IRIS_TOKEN_FILE"), 10); len(fails) == 0 || fails[0].Degraded() {
			t.Fatalf("Row 2 OFF must grade silent, got %+v", fails)
		}

		// 3) Pre-end damaged <hash> under OFF exercises preInspectOFF + retainGeneration proofErr != nil.
		getenv3 := failureEnv(t)
		m3 := sessionsFor(getenv3)
		if err := m3.directory(); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(m3.name("claude", "s1"), []byte("bad\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		h3 := &m2cHub{}
		Runner{
			ClientName: "claude", EventName: "SessionStart",
			Stdin: strings.NewReader(`{"session_id":"s1","cwd":` + quote(root) + `,"hook_event_name":"SessionStart"}`),
			Hub:   h3, Memory: m3, Getenv: getenv3, SuccessionEnabled: func(context.Context) bool { return false },
			after: never, logAfter: never,
		}.Run()
		if len(h3.trace) != 0 {
			t.Fatalf("damaged <hash> OFF called hub: %v", h3.trace)
		}
		if obs, _ := ReadRecentObservations(getenv3("IRIS_TOKEN_FILE"), 10); len(obs) == 0 {
			t.Fatal("expected DamagedGenerationProof observation")
		}

		// 4) finalInspectOFF Row 2, non-ended damaged <hash>, and admittedGen < genFloor via fileGroups.
		parent := t.TempDir()
		mainDir := filepath.Join(parent, "main")
		sibDir := filepath.Join(parent, "sib")
		for _, d := range []string{mainDir, sibDir} {
			if err := os.MkdirAll(filepath.Join(d, ".git"), 0o700); err != nil {
				t.Fatal(err)
			}
		}
		for _, tc := range []struct {
			name     string
			mutate   func(m fileMemory)
			wantDeny bool
			wantSil  bool
		}{
			{
				name: "Row 2 in finalInspectOFF",
				mutate: func(mf fileMemory) {
					mf.Forget("claude", "s1", 2)
					_ = os.WriteFile(mf.name("claude", "s1"), []byte("bad\n"), 0o600)
				},
				wantSil: true,
			},
			{
				name: "damaged <hash> without .ended in finalInspectOFF",
				mutate: func(mf fileMemory) {
					_ = os.WriteFile(mf.name("claude", "s1"), []byte("bad\n"), 0o600)
				},
			},
			{
				name: "higher <hash>=5 after gen=3 in finalInspectOFF",
				mutate: func(mf fileMemory) {
					mf.Remember("claude", "s1", 5)
				},
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				ge := failureEnv(t)
				mf := sessionsFor(ge)
				mf.Remember("claude", "s1", 3)
				ht := &m2cHub{
					regFunc: func(context.Context, hubclient.SessionRequest) (hubclient.SessionResponse, error) {
						return hubclient.SessionResponse{ExecutionID: m2cExecID(10), Generation: 3, Capabilities: []string{"shell_scope", "file_claims"}}, nil
					},
				}
				Runner{
					ClientName: "claude", EventName: "PreToolUse",
					Stdin: strings.NewReader(fmt.Sprintf(`{"session_id":"s1","cwd":%q,"hook_event_name":"PreToolUse","tool_name":"move_file","tool_input":{"rename_from":%q,"rename_to":%q}}`, mainDir, filepath.Join(mainDir, "a.go"), filepath.Join(sibDir, "b.go"))),
					Hub:   ht, Memory: mf, Getenv: ge, SuccessionEnabled: func(context.Context) bool { return false },
					after: never, logAfter: never,
					Find: func(dir string) (gitinfo.Repo, error) {
						if dir == sibDir || strings.HasPrefix(dir, sibDir) {
							tc.mutate(mf)
							gd := filepath.Join(sibDir, ".git")
							return gitinfo.Repo{Worktree: sibDir, GitDir: gd, CommonDir: gd, WorktreeID: "2:2"}, nil
						}
						gd := filepath.Join(mainDir, ".git")
						return gitinfo.Repo{Worktree: mainDir, GitDir: gd, CommonDir: gd, WorktreeID: "1:1"}, nil
					},
				}.Run()
				if len(ht.trace) != 1 {
					t.Fatalf("%s: TouchFiles must not run after finalInspectOFF veto: %v", tc.name, ht.trace)
				}
				if tc.wantSil {
					if fails, _ := ReadRecentFailures(ge("IRIS_TOKEN_FILE"), 10); len(fails) == 0 || fails[0].Degraded() {
						t.Fatalf("%s: expected silent failure, got %+v", tc.name, fails)
					}
				}
			})
		}

		// 5) Direct Remember replaces expired .ended marker when current <= ended.
		ge5 := failureEnv(t)
		mf5 := sessionsFor(ge5)
		old := time.Now().Add(-memoryAge - time.Hour)
		if err := mf5.directory(); err != nil {
			t.Fatal(err)
		}
		p5 := mf5.name("claude", "s1")
		if err := os.WriteFile(p5, []byte("2\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(endedMarkerPath(p5), []byte("3\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(endedMarkerPath(p5), old, old); err != nil {
			t.Fatal(err)
		}
		mf5.Remember("claude", "s1", 4)
		if got := mf5.Recall("claude", "s1"); got != 4 {
			t.Fatalf("Remember over expired .ended wrote %d, want 4", got)
		}
	})

	t.Run("recheckEndedOnHubError ON paths cover deadline, damage, Row 2, and strictFloorRefusal", func(t *testing.T) {
		root := gitRepo(t)
		origOpen := generationFileOpen
		t.Cleanup(func() { generationFileOpen = origOpen })

		// 1) Deadline after reading valid .ended=2 on hub error.
		ge1 := failureEnv(t)
		mf1 := sessionsFor(ge1)
		now := time.Now()
		var clock atomic.Int64
		clock.Store(now.UnixNano())
		mf1.now = func() time.Time { return time.Unix(0, clock.Load()) }
		mf1.mono = mf1.now
		mf1.Remember("claude", "s1", 2)
		if err := os.WriteFile(endedMarkerPath(mf1.name("claude", "s1")), []byte("2\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		// Remove <hash> before preInspect? Wait: during SessionStart with valid .ended=2 and no <hash>, SessionStart sends to hub,
		// and during hub error we re-open <hash> inside recheckEndedOnHubError (2nd open of .ended / <hash>).
		_ = os.Remove(mf1.name("claude", "s1"))
		if err := os.WriteFile(mf1.name("claude", "s1"), []byte("3\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		reads := 0
		generationFileOpen = func(path string) (*os.File, error) {
			if path == mf1.name("claude", "s1") {
				reads++
				if reads == 2 {
					clock.Store(now.Add(time.Hour).UnixNano())
				}
			}
			return OpenGenerationFile(path)
		}
		h1 := &m2cHub{
			regFunc: func(context.Context, hubclient.SessionRequest) (hubclient.SessionResponse, error) {
				return hubclient.SessionResponse{}, &hubclient.Error{Code: "unavailable", Message: "down"}
			},
		}
		Runner{
			ClientName: "claude", EventName: "SessionStart",
			Stdin: strings.NewReader(`{"session_id":"s1","cwd":` + quote(root) + `,"hook_event_name":"SessionStart"}`),
			Hub:   h1, Memory: mf1, Getenv: ge1, SuccessionEnabled: func(context.Context) bool { return true },
			Now: mf1.mono, Deadline: time.Second, after: never, logAfter: never,
		}.Run()
		generationFileOpen = origOpen
		if fails, _ := ReadRecentFailures(ge1("IRIS_TOKEN_FILE"), 10); len(fails) == 0 || fails[0].Degraded() {
			t.Fatalf("expected silent deadline in recheckEndedOnHubError, got %+v", fails)
		}

		// 2) Damaged .ended, damaged proof without end, and Row 2 damaged proof during hub error.
		for _, tc := range []struct {
			name    string
			mutate  func(mf fileMemory)
			wantSil bool
		}{
			{
				name: "damaged .ended on hub error",
				mutate: func(mf fileMemory) {
					_ = os.WriteFile(endedMarkerPath(mf.name("claude", "s1")), []byte("bad\n"), 0o600)
				},
			},
			{
				name: "damaged <hash> without .ended on hub error",
				mutate: func(mf fileMemory) {
					_ = os.WriteFile(mf.name("claude", "s1"), []byte("bad\n"), 0o600)
				},
			},
			{
				name: "Row 2 damaged <hash> plus .ended on hub error",
				mutate: func(mf fileMemory) {
					mf.Forget("claude", "s1", 2)
					_ = os.WriteFile(mf.name("claude", "s1"), []byte("bad\n"), 0o600)
				},
				wantSil: true,
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				ge := failureEnv(t)
				mf := sessionsFor(ge)
				mf.Remember("claude", "s1", 3)
				h := &m2cHub{
					regFunc: func(context.Context, hubclient.SessionRequest) (hubclient.SessionResponse, error) {
						tc.mutate(mf)
						return hubclient.SessionResponse{}, &hubclient.Error{Code: "lost", Details: json.RawMessage(`{"highest_generation":3}`)}
					},
				}
				Runner{
					ClientName: "claude", EventName: "Prompt",
					Stdin: strings.NewReader(`{"session_id":"s1","cwd":` + quote(root) + `,"hook_event_name":"UserPromptSubmit","prompt":"hi"}`),
					Hub:   h, Memory: mf, Getenv: ge, SuccessionEnabled: func(context.Context) bool { return true },
					after: never, logAfter: never,
				}.Run()
				if len(h.trace) != 1 {
					t.Fatalf("%s: expected local veto to stop retry, got %v", tc.name, h.trace)
				}
				if tc.wantSil {
					if fails, _ := ReadRecentFailures(ge("IRIS_TOKEN_FILE"), 10); len(fails) == 0 || fails[0].Degraded() {
						t.Fatalf("%s: expected silent failure, got %+v", tc.name, fails)
					}
				}
			})
		}

		// 3) strictFloorRefusal under ON: proofless SessionStart over ended=2 receives lost highest_generation=1.
		ge3 := failureEnv(t)
		mf3 := sessionsFor(ge3)
		mf3.Remember("claude", "s1", 2)
		mf3.Forget("claude", "s1", 2)
		_ = os.Remove(mf3.name("claude", "s1"))
		h3 := &m2cHub{
			regFunc: func(context.Context, hubclient.SessionRequest) (hubclient.SessionResponse, error) {
				return hubclient.SessionResponse{}, &hubclient.Error{Code: "lost", Details: json.RawMessage(`{"highest_generation":1}`)}
			},
		}
		Runner{
			ClientName: "claude", EventName: "SessionStart",
			Stdin: strings.NewReader(`{"session_id":"s1","cwd":` + quote(root) + `,"hook_event_name":"SessionStart"}`),
			Hub:   h3, Memory: mf3, Getenv: ge3, SuccessionEnabled: func(context.Context) bool { return true },
			after: never, logAfter: never,
		}.Run()
		if len(h3.trace) != 1 {
			t.Fatalf("strictFloorRefusal trace=%v", h3.trace)
		}
		if obs, _ := ReadRecentObservations(ge3("IRIS_TOKEN_FILE"), 10); len(obs) == 0 {
			t.Fatal("expected ended observation on strictFloorRefusal")
		}
	})

	t.Run("finalInspectAndPublishON and writeEvidenceFile deadline, conflict, downgrade, and shim ended branches", func(t *testing.T) {
		parent := t.TempDir()
		mainDir := filepath.Join(parent, "main")
		sibDir := filepath.Join(parent, "sib")
		for _, d := range []string{mainDir, sibDir} {
			if err := os.MkdirAll(filepath.Join(d, ".git"), 0o700); err != nil {
				t.Fatal(err)
			}
		}
		origGenOpen := generationFileOpen
		origExactOpen := exactRecordOpen
		t.Cleanup(func() {
			generationFileOpen = origGenOpen
			exactRecordOpen = origExactOpen
		})

		// 1) CandidateShimSuccession where PriorGeneration=2 ended via Forget(2) before finalAdmitAndPublish.
		geShim := failureEnv(t)
		mfShim := sessionsFor(geShim)
		mfShim.Remember("claude", "s1", 3)
		m2cWriteExact(t, mfShim, "claude", "s1", sessionproof.Record{
			Version:          sessionproof.FormatVersion,
			Origin:           sessionproof.OriginShim,
			Generation:       3,
			ExecutionID:      m2cExecID(21),
			PriorExecutionID: m2cExecID(20),
			PriorGeneration:  2,
		})
		hShim := &m2cHub{
			regFunc: func(context.Context, hubclient.SessionRequest) (hubclient.SessionResponse, error) {
				return m2cClassifiedSession(m2cExecID(21), 3, m2cExecID(20), "shim", "shim_succession", true), nil
			},
		}
		outShim := Runner{
			ClientName: "claude", EventName: "PreToolUse",
			Stdin: strings.NewReader(fmt.Sprintf(`{"session_id":"s1","cwd":%q,"hook_event_name":"PreToolUse","tool_name":"move_file","tool_input":{"rename_from":%q,"rename_to":%q}}`, mainDir, filepath.Join(mainDir, "a.go"), filepath.Join(sibDir, "b.go"))),
			Hub:   hShim, Memory: mfShim, Getenv: geShim, SuccessionEnabled: func(context.Context) bool { return true },
			after: never, logAfter: never,
			Find: func(dir string) (gitinfo.Repo, error) {
				if dir == sibDir || strings.HasPrefix(dir, sibDir) {
					mfShim.Forget("claude", "s1", 2)
					gd := filepath.Join(sibDir, ".git")
					return gitinfo.Repo{Worktree: sibDir, GitDir: gd, CommonDir: gd, WorktreeID: "2:2"}, nil
				}
				gd := filepath.Join(mainDir, ".git")
				return gitinfo.Repo{Worktree: mainDir, GitDir: gd, CommonDir: gd, WorktreeID: "1:1"}, nil
			},
		}.Run()
		if len(hShim.trace) != 1 || !strings.Contains(string(outShim.Stdout), string(ActionDeny)) {
			t.Fatalf("shim recordEndedAt must veto and deny: trace=%v out=%s", hShim.trace, outShim.Stdout)
		}

		// 2) Intervening mutations in finalInspectAndPublishON: conflicting .execution, damaged .ended, Row 2, damaged <hash>, and genFloor downgrade.
		for _, tc := range []struct {
			name    string
			reply   hubclient.SessionResponse
			mutate  func(mf fileMemory)
			wantSil bool
		}{
			{
				name:  "non-shim conflicting exact record in finalInspectAndPublishON",
				reply: m2cClassifiedSession(m2cExecID(30), 3, m2cExecID(29), "hook", "stale_retry", true),
				mutate: func(mf fileMemory) {
					m2cWriteExact(t, mf, "claude", "s1", sessionproof.Record{Version: 1, Origin: sessionproof.OriginHook, Generation: 3, ExecutionID: m2cExecID(31)})
				},
				wantSil: true,
			},
			{
				name:  "damaged .ended in finalInspectAndPublishON",
				reply: m2cClassifiedSession(m2cExecID(30), 3, m2cExecID(29), "hook", "stale_retry", true),
				mutate: func(mf fileMemory) {
					_ = os.WriteFile(endedMarkerPath(mf.name("claude", "s1")), []byte("bad\n"), 0o600)
				},
			},
			{
				name:  "Row 2 damaged <hash> plus .ended in finalInspectAndPublishON",
				reply: m2cClassifiedSession(m2cExecID(30), 3, m2cExecID(29), "hook", "stale_retry", true),
				mutate: func(mf fileMemory) {
					mf.Forget("claude", "s1", 2)
					_ = os.WriteFile(mf.name("claude", "s1"), []byte("bad\n"), 0o600)
				},
				wantSil: true,
			},
			{
				name:  "damaged <hash> without .ended in finalInspectAndPublishON",
				reply: m2cClassifiedSession(m2cExecID(30), 3, m2cExecID(29), "hook", "stale_retry", true),
				mutate: func(mf fileMemory) {
					_ = os.WriteFile(mf.name("claude", "s1"), []byte("bad\n"), 0o600)
				},
			},
			{
				name:  "higher <hash>=5 after admitted gen=3 in finalInspectAndPublishON",
				reply: m2cClassifiedSession(m2cExecID(30), 3, m2cExecID(29), "hook", "stale_retry", true),
				mutate: func(mf fileMemory) {
					mf.Remember("claude", "s1", 5)
				},
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				ge := failureEnv(t)
				mf := sessionsFor(ge)
				mf.Remember("claude", "s1", 3)
				h := &m2cHub{
					regFunc: func(context.Context, hubclient.SessionRequest) (hubclient.SessionResponse, error) {
						return tc.reply, nil
					},
				}
				Runner{
					ClientName: "claude", EventName: "PreToolUse",
					Stdin: strings.NewReader(fmt.Sprintf(`{"session_id":"s1","cwd":%q,"hook_event_name":"PreToolUse","tool_name":"move_file","tool_input":{"rename_from":%q,"rename_to":%q}}`, mainDir, filepath.Join(mainDir, "a.go"), filepath.Join(sibDir, "b.go"))),
					Hub:   h, Memory: mf, Getenv: ge, SuccessionEnabled: func(context.Context) bool { return true },
					after: never, logAfter: never,
					Find: func(dir string) (gitinfo.Repo, error) {
						if dir == sibDir || strings.HasPrefix(dir, sibDir) {
							tc.mutate(mf)
							gd := filepath.Join(sibDir, ".git")
							return gitinfo.Repo{Worktree: sibDir, GitDir: gd, CommonDir: gd, WorktreeID: "2:2"}, nil
						}
						gd := filepath.Join(mainDir, ".git")
						return gitinfo.Repo{Worktree: mainDir, GitDir: gd, CommonDir: gd, WorktreeID: "1:1"}, nil
					},
				}.Run()
				if len(h.trace) != 1 {
					t.Fatalf("%s: TouchFiles ran unexpectedly: %v", tc.name, h.trace)
				}
				if tc.wantSil {
					if fails, _ := ReadRecentFailures(ge("IRIS_TOKEN_FILE"), 10); len(fails) == 0 || fails[0].Degraded() {
						t.Fatalf("%s: expected silent failure, got %+v", tc.name, fails)
					}
				}
			})
		}

		// 3) Deadlines inside finalInspectAndPublishON (lock, .ended, <hash>, .execution) and writeEvidenceFile replaceSessionFileDeadline.
		for _, step := range []string{"lock", "ended", "gen", "exec", "replace"} {
			t.Run("finalInspectON_deadline_"+step, func(t *testing.T) {
				generationFileOpen = origGenOpen
				exactRecordOpen = origExactOpen
				ge := failureEnv(t)
				mf := sessionsFor(ge)
				now := time.Now()
				var clock atomic.Int64
				clock.Store(now.UnixNano())
				mf.now = func() time.Time { return time.Unix(0, clock.Load()) }
				mf.mono = mf.now
				if err := mf.directory(); err != nil {
					t.Fatal(err)
				}
				prefix := mf.name("claude", "s1")
				mf.Remember("claude", "s1", 2)
				if err := os.WriteFile(endedMarkerPath(prefix), []byte("1\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				m2cWriteExact(t, mf, "claude", "s1", sessionproof.Record{Version: 1, Origin: sessionproof.OriginHook, Generation: 2, ExecutionID: m2cExecID(39)})

				var relOnce func()
				var releasePause chan struct{}
				endedOpens, genOpens, exactOpens := 0, 0, 0
				switch step {
				case "lock":
					releasePause = make(chan struct{})
					mf.pause = func(time.Duration) {
						clock.Store(now.Add(time.Hour).UnixNano())
					}
				case "ended":
					generationFileOpen = func(path string) (*os.File, error) {
						if strings.HasSuffix(path, ".ended") {
							endedOpens++
							if endedOpens == 2 {
								clock.Store(now.Add(time.Hour).UnixNano())
							}
						}
						return OpenGenerationFile(path)
					}
				case "gen":
					generationFileOpen = func(path string) (*os.File, error) {
						if path == prefix {
							genOpens++
							if genOpens == 2 {
								clock.Store(now.Add(time.Hour).UnixNano())
							}
						}
						return OpenGenerationFile(path)
					}
				case "exec":
					exactRecordOpen = func(path string, expected exactRecordID) (*os.File, error) {
						exactOpens++
						if exactOpens == 2 {
							clock.Store(now.Add(time.Hour).UnixNano())
						}
						return origExactOpen(path, expected)
					}
				case "replace":
					mf.restrict = func(p string) error {
						if p != mf.dir {
							clock.Store(now.Add(time.Hour).UnixNano())
						}
						return nil
					}
				}
				h := &m2cHub{
					regFunc: func(context.Context, hubclient.SessionRequest) (hubclient.SessionResponse, error) {
						return m2cClassifiedSession(m2cExecID(40), 3, m2cExecID(39), "hook", "stale_retry", true), nil
					},
				}
				out := Runner{
					ClientName: "claude", EventName: "PreToolUse",
					Stdin: strings.NewReader(fmt.Sprintf(`{"session_id":"s1","cwd":%q,"hook_event_name":"PreToolUse","tool_name":"move_file","tool_input":{"rename_from":%q,"rename_to":%q}}`, mainDir, filepath.Join(mainDir, "a.go"), filepath.Join(sibDir, "b.go"))),
					Hub:   h, Memory: mf, Getenv: ge, SuccessionEnabled: func(context.Context) bool { return true },
					Now: mf.mono, Deadline: time.Second, after: never, logAfter: never,
					Find: func(dir string) (gitinfo.Repo, error) {
						if step == "lock" && (dir == sibDir || strings.HasPrefix(dir, sibDir)) {
							rel, err := mf.lock(time.Second)
							if err != nil {
								t.Fatal(err)
							}
							relOnce = sync.OnceFunc(rel)
						}
						if dir == sibDir || strings.HasPrefix(dir, sibDir) {
							gd := filepath.Join(sibDir, ".git")
							return gitinfo.Repo{Worktree: sibDir, GitDir: gd, CommonDir: gd, WorktreeID: "2:2"}, nil
						}
						gd := filepath.Join(mainDir, ".git")
						return gitinfo.Repo{Worktree: mainDir, GitDir: gd, CommonDir: gd, WorktreeID: "1:1"}, nil
					},
				}.Run()
				if releasePause != nil {
					close(releasePause)
				}
				if relOnce != nil {
					relOnce()
				}
				assertAllow(t, ClientClaude, out)
				if step == "replace" {
					if !slices.Contains(h.trace, fmt.Sprintf("touch_files:id=%s:gen=3", m2cExecID(40))) {
						t.Fatalf("optional replace failure must not block TouchFiles: %v", h.trace)
					}
					if obs, _ := ReadRecentObservations(ge("IRIS_TOKEN_FILE"), 10); len(obs) == 0 {
						t.Fatal("expected SessionEvidenceWriteFailed on replace deadline overrun")
					}
				} else {
					if len(h.trace) != 1 {
						t.Fatalf("%s: expected mandatory deadline veto before TouchFiles, got %v", step, h.trace)
					}
					if fails, _ := ReadRecentFailures(ge("IRIS_TOKEN_FILE"), 10); len(fails) == 0 || fails[0].Degraded() {
						t.Fatalf("%s: expected silent failure, got %+v", step, fails)
					}
				}
			})
		}
	})

	t.Run("runner Prompt, SessionEnd, OutsideGit Edit, CursorStop finalAdmit vetos, and shell checkEndedRegistration", func(t *testing.T) {
		root := gitRepo(t)

		// 1) KindPrompt vetoed in finalAdmitAndPublish.
		geP := failureEnv(t)
		mfP := sessionsFor(geP)
		Runner{
			ClientName: "claude", EventName: "Prompt",
			Stdin: strings.NewReader(`{"session_id":"s1","cwd":` + quote(root) + `,"hook_event_name":"UserPromptSubmit","prompt":"hi"}`),
			Hub: &m2cHub{regFunc: func(context.Context, hubclient.SessionRequest) (hubclient.SessionResponse, error) {
				mfP.Forget("claude", "s1", 1)
				return m2cClassifiedSession(m2cExecID(1), 1, "", "hook", "initial", true), nil
			}},
			Memory: mfP, Getenv: geP, SuccessionEnabled: func(context.Context) bool { return true },
			after: never, logAfter: never,
		}.Run()
		if obs, _ := ReadRecentObservations(geP("IRIS_TOKEN_FILE"), 10); len(obs) == 0 {
			t.Fatal("expected Prompt veto observation")
		}

		// 2) KindSessionEnd vetoed in finalAdmitAndPublish before EndExecution.
		geE := failureEnv(t)
		mfE := sessionsFor(geE)
		hE := &m2cHub{regFunc: func(context.Context, hubclient.SessionRequest) (hubclient.SessionResponse, error) {
			mfE.Forget("claude", "s1", 1)
			return m2cClassifiedSession(m2cExecID(1), 1, "", "hook", "initial", true), nil
		}}
		Runner{
			ClientName: "claude", EventName: "SessionEnd",
			Stdin: strings.NewReader(`{"session_id":"s1","cwd":` + quote(root) + `,"hook_event_name":"SessionEnd"}`),
			Hub:   hE, Memory: mfE, Getenv: geE, SuccessionEnabled: func(context.Context) bool { return true },
			after: never, logAfter: never,
		}.Run()
		if len(hE.trace) != 1 {
			t.Fatalf("expected SessionEnd veto before EndExecution, got %v", hE.trace)
		}

		// 3) OutsideGit Edit vetoed in finalAdmitAndPublish (both errEndedGeneration Deny and non-ended veto Allow).
		for _, endedVeto := range []bool{true, false} {
			geO := failureEnv(t)
			mfO := sessionsFor(geO)
			if err := mfO.directory(); err != nil {
				t.Fatal(err)
			}
			hO := &m2cHub{regFunc: func(context.Context, hubclient.SessionRequest) (hubclient.SessionResponse, error) {
				if endedVeto {
					mfO.Forget("claude", "s1", 1)
				} else if err := os.WriteFile(endedMarkerPath(mfO.name("claude", "s1")), []byte("bad\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				resp := m2cClassifiedSession(m2cExecID(1), 1, "", "hook", "initial", true)
				resp.OutsideGit = true
				return resp, nil
			}}
			outO := Runner{
				ClientName: "claude", EventName: "PreToolUse",
				Stdin: strings.NewReader(fmt.Sprintf(`{"session_id":"s1","cwd":%q,"hook_event_name":"PreToolUse","tool_name":"Write","tool_input":{"file_path":%q,"content":"x"}}`, root, filepath.Join(root, "a.go"))),
				Hub:   hO, Memory: mfO, Getenv: geO, SuccessionEnabled: func(context.Context) bool { return true },
				after: never, logAfter: never,
			}.Run()
			if endedVeto && !strings.Contains(string(outO.Stdout), string(ActionDeny)) {
				t.Fatalf("expected OutsideGit ended veto to Deny, got %s", outO.Stdout)
			}
			if !endedVeto {
				assertAllow(t, ClientClaude, outO)
				if obs, _ := ReadRecentObservations(geO("IRIS_TOKEN_FILE"), 10); len(obs) == 0 {
					t.Fatal("expected OutsideGit damaged .ended observation")
				}
			}
		}

		// 4) CursorStop calls r.register(...) -> vetoed in finalAdmitAndPublish.
		interactiveCursor(t)
		geC := failureEnv(t)
		mfC := sessionsFor(geC)
		Runner{
			ClientName: "cursor", EventName: "stop",
			Stdin: strings.NewReader(cursorStopPayload(root, 0)),
			Hub: &m2cHub{regFunc: func(context.Context, hubclient.SessionRequest) (hubclient.SessionResponse, error) {
				mfC.Forget("cursor", "s", 1)
				return m2cClassifiedSession(m2cExecID(1), 1, "", "hook", "initial", true), nil
			}},
			Memory: mfC, Getenv: geC, Ancestor: cursorAncestor,
			SuccessionEnabled: func(context.Context) bool { return true },
			after:             never, logAfter: never,
		}.Run()
		if obs, _ := ReadRecentObservations(geC("IRIS_TOKEN_FILE"), 10); len(obs) == 0 {
			t.Fatal("expected CursorStop finalAdmit veto observation")
		}

		// 5) shell checkEndedRegistration (valid, expired, damaged) and combined ReconcileSession finalAdmit veto.
		for _, mode := range []string{"valid_ended", "expired_ended", "damaged_ended", "combined_veto"} {
			t.Run(mode, func(t *testing.T) {
				geS := failureEnv(t)
				mfS := sessionsFor(geS)
				mHub := &m2cHub{}
				var hub Hub = mHub
				switch mode {
				case "valid_ended":
					mHub.reconcileFn = func(context.Context, hubclient.ReconcileRequest) (hubclient.CheckResponse, error) {
						mfS.Forget("claude", "s1", 1)
						return hubclient.CheckResponse{}, nil
					}
				case "expired_ended":
					mHub.reconcileFn = func(context.Context, hubclient.ReconcileRequest) (hubclient.CheckResponse, error) {
						mfS.Forget("claude", "s1", 1)
						old := time.Now().Add(-memoryAge - time.Hour)
						_ = os.Chtimes(endedMarkerPath(mfS.name("claude", "s1")), old, old)
						return hubclient.CheckResponse{}, nil
					}
				case "damaged_ended":
					mHub.reconcileFn = func(context.Context, hubclient.ReconcileRequest) (hubclient.CheckResponse, error) {
						_ = os.WriteFile(endedMarkerPath(mfS.name("claude", "s1")), []byte("bad\n"), 0o600)
						return hubclient.CheckResponse{}, nil
					}
				case "combined_veto":
					mHub.combinedFn = func(context.Context, hubclient.ShellSessionRequest) (hubclient.ShellResponse, error) {
						mfS.Forget("claude", "s1", 1)
						return hubclient.ShellResponse{Session: m2cClassifiedSession(m2cExecID(1), 1, "", "hook", "initial", true)}, nil
					}
					hub = m2cCombinedHub{mHub}
				}
				outS := Runner{
					ClientName: "claude", EventName: "PreToolUse",
					Stdin: strings.NewReader(fmt.Sprintf(`{"session_id":"s1","cwd":%q,"tool_name":"Bash","tool_input":{"command":"echo hi"}}`, root)),
					Hub:   hub, Memory: mfS, Getenv: geS, SuccessionEnabled: func(context.Context) bool { return true },
					Find: func(string) (gitinfo.Repo, error) { return gitinfo.Repo{Worktree: root}, nil },
					Status: func(context.Context, string, gitinfo.Scope, *gitinfo.Attempt) (map[string]string, error) {
						return map[string]string{}, nil
					},
					ShellDeadline: time.Second, after: never, logAfter: never,
				}.Run()
				if (mode == "valid_ended" || mode == "combined_veto") && !strings.Contains(string(outS.Stdout), string(ActionDeny)) {
					t.Fatalf("%s: expected PreToolUse Deny, got %s", mode, outS.Stdout)
				}
				if mode == "expired_ended" {
					assertAllow(t, ClientClaude, outS)
				}
				if mode == "damaged_ended" {
					assertAllow(t, ClientClaude, outS)
					if obs, _ := ReadRecentObservations(geS("IRIS_TOKEN_FILE"), 10); len(obs) == 0 {
						t.Fatal("expected DamagedGenerationProof observation")
					}
				}
			})
		}
	})
}

func TestOFFExpiredEndCleanupUnderLockAndKeepsFloorOnError(t *testing.T) {
	t.Run("expired cleanup waits for lock holder before deleting .ended", func(t *testing.T) {
		root := gitRepo(t)
		getenv := failureEnv(t)
		m := sessionsFor(getenv)
		m.Remember("claude", "s1", 2)
		m.Forget("claude", "s1", 2)
		prefix := m.name("claude", "s1")
		marker := endedMarkerPath(prefix)
		if err := os.WriteFile(prefix, []byte("2\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		old := time.Now().Add(-memoryAge - time.Hour)
		if err := os.Chtimes(marker, old, old); err != nil {
			t.Fatal(err)
		}

		release, err := m.lock(time.Second)
		if err != nil {
			t.Fatal(err)
		}
		releaseOnce := sync.OnceFunc(release)
		defer releaseOnce()

		var sawContention atomic.Bool
		contendedMem := m
		contendedMem.pause = func(time.Duration) {
			if sawContention.CompareAndSwap(false, true) {
				if _, statErr := os.Stat(marker); statErr != nil {
					t.Errorf("expired .ended was touched before lock acquired: %v", statErr)
				}
				releaseOnce()
			}
		}

		h := &m2cHub{
			regFunc: func(context.Context, hubclient.SessionRequest) (hubclient.SessionResponse, error) {
				return hubclient.SessionResponse{
					ExecutionID: m2cExecID(40), SessionID: "s1", Generation: 1, Worktree: "/repo",
					Capabilities: []string{"shell_scope", "file_claims"},
				}, nil
			},
		}
		out := Runner{
			ClientName: "claude", EventName: "PreToolUse",
			Stdin:    strings.NewReader(editPayload(ClientClaude, root)),
			Hub:      h,
			Memory:   contendedMem,
			Getenv:   getenv,
			Deadline: 2 * time.Second,
			after:    never,
			logAfter: never,
		}.Run()
		assertAllow(t, ClientClaude, out)
		if !sawContention.Load() {
			t.Fatal("OFF expired cleanup did not contend on sessions lock")
		}
		if len(h.regReqs) != 1 || h.regReqs[0].EndedThrough != 0 || h.regReqs[0].Generation != 0 {
			t.Fatalf("unexpected register reqs after locked cleanup: %+v", h.regReqs)
		}
		if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("expected cleaned .ended to be deleted under lock, stat err=%v", statErr)
		}
	})

	t.Run("failed expired cleanup keeps ended floor", func(t *testing.T) {
		root := gitRepo(t)
		getenv := failureEnv(t)
		m := sessionsFor(getenv)
		m.Forget("claude", "s1", 2)
		prefix := m.name("claude", "s1")
		marker := endedMarkerPath(prefix)
		old := time.Now().Add(-memoryAge - time.Hour)
		if err := os.Chtimes(marker, old, old); err != nil {
			t.Fatal(err)
		}

		origRemove := removeSessionFile
		t.Cleanup(func() { removeSessionFile = origRemove })
		removeSessionFile = func(p string) error {
			if p == prefix || p == marker {
				return errors.New("unlink blocked")
			}
			return origRemove(p)
		}

		h := &m2cHub{
			regFunc: func(context.Context, hubclient.SessionRequest) (hubclient.SessionResponse, error) {
				return hubclient.SessionResponse{
					ExecutionID: m2cExecID(41), SessionID: "s1", Generation: 3, Worktree: "/repo",
					Capabilities: []string{"shell_scope", "file_claims"},
				}, nil
			},
		}
		outStart := Runner{
			ClientName: "claude", EventName: "SessionStart",
			Stdin:    strings.NewReader(`{"session_id":"s1","cwd":` + quote(root) + `,"hook_event_name":"SessionStart"}`),
			Hub:      h,
			Memory:   m,
			Getenv:   getenv,
			after:    never,
			logAfter: never,
		}.Run()
		if outStart.Exit != 0 || !strings.Contains(string(outStart.Stdout), m2cExecID(41)) {
			t.Fatalf("expected admitted SessionStart output, got exit=%d out=%s", outStart.Exit, outStart.Stdout)
		}
		if len(h.regReqs) != 1 || h.regReqs[0].EndedThrough != 2 {
			t.Fatalf("expected failed cleanup to preserve EndedThrough=2, got reqs=%+v", h.regReqs)
		}

		// With <hash>=2 preserved beside failing cleanup on expired .ended=2, PreToolUse must still deny locally.
		gePre := failureEnv(t)
		mPre := sessionsFor(gePre)
		mPre.Forget("claude", "s1", 2)
		prePrefix := mPre.name("claude", "s1")
		preMarker := endedMarkerPath(prePrefix)
		if err := os.WriteFile(prePrefix, []byte("2\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(preMarker, old, old); err != nil {
			t.Fatal(err)
		}
		removeSessionFile = func(p string) error {
			if p == prePrefix || p == preMarker {
				return errors.New("unlink blocked")
			}
			return origRemove(p)
		}
		hPre := &m2cHub{}
		outPre := Runner{
			ClientName: "claude", EventName: "PreToolUse",
			Stdin:    strings.NewReader(editPayload(ClientClaude, root)),
			Hub:      hPre,
			Memory:   mPre,
			Getenv:   gePre,
			after:    never,
			logAfter: never,
		}.Run()
		if outPre.Exit != 0 || !strings.Contains(string(outPre.Stdout), string(ActionDeny)) {
			t.Fatalf("expected PreToolUse Deny with kept floor, got %s", outPre.Stdout)
		}
		if len(hPre.trace) != 0 {
			t.Fatalf("expected zero hub RPCs under kept end floor, got %v", hPre.trace)
		}
	})
}

func TestSuccessionON_FullHoldExcludesDirPrepButStillBoundsHeldSection(t *testing.T) {
	t.Run("slow directory() and prepareSessionFile do not fail closed", func(t *testing.T) {
		base := newFileMemory(t)
		baseRestrict := base.restrict
		var slowDir, slowTmp atomic.Bool
		m := fileMemory{
			dir: base.dir,
			now: time.Now,
			restrict: func(p string) error {
				if p == base.dir && slowDir.CompareAndSwap(true, false) {
					time.Sleep(hookLockBudget + 20*time.Millisecond)
				} else if strings.HasPrefix(filepath.Base(p), ".tmp-") && slowTmp.CompareAndSwap(true, false) {
					time.Sleep(hookLockBudget + 20*time.Millisecond)
				}
				return baseRestrict(p)
			},
		}

		slowDir.Store(true)
		slowTmp.Store(true)
		m.rememberWithSuccession("claude", "s1", 2, time.Now().Add(DefaultDeadline), true)
		if slowDir.Load() || slowTmp.Load() {
			t.Fatalf("expected slow directory/tmp prep seams to run: dirPending=%t tmpPending=%t", slowDir.Load(), slowTmp.Load())
		}
		if got := base.Recall("claude", "s1"); got != 2 {
			t.Fatalf("rememberWithSuccession failed closed on slow prep: got G%d, want G2", got)
		}

		slowDir.Store(true)
		slowTmp.Store(true)
		if failed := m.forgetEvidence("claude", "s1", 2, time.Now().Add(DefaultDeadline), true); failed != "" {
			t.Fatalf("forgetEvidence failed closed on slow prep: failed=%q", failed)
		}
		if slowDir.Load() || slowTmp.Load() {
			t.Fatalf("expected slow directory/tmp prep seams to run in forgetEvidence: dirPending=%t tmpPending=%t", slowDir.Load(), slowTmp.Load())
		}
		if got := base.Recall("claude", "s1"); got != 0 {
			t.Fatalf("forgetEvidence left G%d behind", got)
		}
		if ended, err := readGeneration(endedMarkerPath(base.name("claude", "s1"))); err != nil || ended != 2 {
			t.Fatalf("forgetEvidence did not record .ended=2: ended=%d err=%v", ended, err)
		}
	})

	t.Run("slow held section still fails closed with real clock", func(t *testing.T) {
		base := newFileMemory(t)
		base.Remember("claude", "s1", 1)

		origOpen := generationFileOpen
		t.Cleanup(func() { generationFileOpen = origOpen })
		var slowHeld atomic.Bool
		generationFileOpen = func(p string) (*os.File, error) {
			if slowHeld.CompareAndSwap(true, false) {
				time.Sleep(hookLockBudget + 20*time.Millisecond)
			}
			return origOpen(p)
		}

		m := fileMemory{
			dir:      base.dir,
			now:      time.Now,
			restrict: base.restrict,
		}
		slowHeld.Store(true)
		m.rememberWithSuccession("claude", "s1", 2, time.Now().Add(DefaultDeadline), true)
		if slowHeld.Load() {
			t.Fatal("expected held-section seam to run in rememberWithSuccession")
		}
		if got := base.Recall("claude", "s1"); got != 1 {
			t.Fatalf("rememberWithSuccession modified G1 despite slow held section exceeding 50ms: got G%d", got)
		}

		slowHeld.Store(true)
		marker := endedMarkerPath(base.name("claude", "s1"))
		if failed := m.forgetEvidence("claude", "s1", 2, time.Now().Add(DefaultDeadline), true); failed != marker {
			t.Fatalf("forgetEvidence failed to fail closed on slow held section: failed=%q want=%q", failed, marker)
		}
		if slowHeld.Load() {
			t.Fatal("expected held-section seam to run in forgetEvidence")
		}
		if _, err := os.Lstat(marker); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("forgetEvidence wrote .ended despite slow held section: err=%v", err)
		}
		if got := base.Recall("claude", "s1"); got != 1 {
			t.Fatalf("forgetEvidence modified G1 despite slow held section: got G%d", got)
		}
	})
}
