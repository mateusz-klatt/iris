package hook

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/config"
	"github.com/mateusz-klatt/iris/internal/gitinfo"
	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/kimiwake"
	"github.com/mateusz-klatt/iris/internal/proc"
)

func successionSessionFiles(t *testing.T, client, session string, generation int, execution string) *Sessions {
	t.Helper()
	s := NewSessions(filepath.Join(t.TempDir(), "hub.token"))
	if err := s.Protect(); err != nil {
		t.Fatal(err)
	}
	s.memory.remember(client, session, generation, execution)
	return s
}

func TestExecutionProofRememberRecallAndForget(t *testing.T) {
	s := successionSessionFiles(t, "claude", "s", 1, "ex_one")
	path := s.memory.name("claude", "s")
	executionPath := path + ".execution"
	if got, err := readExecutionProof(executionPath); got != "ex_one" || err != nil {
		t.Fatalf("execution proof: %q, %v", got, err)
	}
	s.memory.remember("claude", "s", 2, "ex_two")
	s.memory.remember("claude", "s", 1, "ex_old")
	if generation, _ := readGeneration(path); generation != 2 {
		t.Fatalf("older Remember downgraded generation to %d", generation)
	}
	if got, _ := readExecutionProof(executionPath); got != "ex_two" {
		t.Fatalf("older Remember changed execution to %q", got)
	}
	// The interface-compatible generation-only form does not erase intact execution proof.
	s.memory.Remember("claude", "s", 2)
	if got, _ := readExecutionProof(executionPath); got != "ex_two" {
		t.Fatalf("generation-only Remember erased %q", got)
	}
	old := time.Now().Add(-time.Hour)
	for _, name := range []string{path, executionPath} {
		if err := os.Chtimes(name, old, old); err != nil {
			t.Fatal(err)
		}
	}
	if generation, err := s.memory.RecallGeneration("claude", "s"); generation != 2 || err != nil {
		t.Fatalf("RecallGeneration: %d, %v", generation, err)
	}
	for _, name := range []string{path, executionPath} {
		info, err := os.Stat(name)
		if err != nil || !info.ModTime().After(old) {
			t.Fatalf("%s was not refreshed: %v, %v", name, info, err)
		}
	}
	s.memory.Forget("claude", "s", 1)
	if generation, _ := readGeneration(path); generation != 2 {
		t.Fatal("an older SessionEnd removed current proof")
	}
	s.memory.Forget("claude", "s", 2)
	for _, name := range []string{path, executionPath} {
		if _, err := os.Lstat(name); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("SessionEnd left %s: %v", name, err)
		}
	}
	if ended, err := readGeneration(path + ".ended"); ended != 2 || err != nil {
		t.Fatalf("SessionEnd marker: %d, %v", ended, err)
	}
}

func TestForgetFailureKeepsGenerationAndExecutionProof(t *testing.T) {
	s := successionSessionFiles(t, "claude", "s", 1, "ex_one")
	path := s.memory.name("claude", "s")
	marker := path + ".ended"
	if err := os.Mkdir(marker, 0o700); err != nil {
		t.Fatal(err)
	}
	if damaged := s.memory.forget("claude", "s", 1); damaged != marker {
		t.Fatalf("damaged marker path: %q", damaged)
	}
	if generation, err := readGeneration(path); generation != 1 || err != nil {
		t.Fatalf("generation proof was removed: %d, %v", generation, err)
	}
	if execution, err := readExecutionProof(path + ".execution"); execution != "ex_one" || err != nil {
		t.Fatalf("execution proof was removed: %q, %v", execution, err)
	}
}

func TestRecallBindingGenerationRejectsDamagedExecutionProof(t *testing.T) {
	for _, tc := range []struct {
		name    string
		damage  func(string) error
		missing bool
	}{
		{"malformed", func(path string) error { return os.WriteFile(path, []byte("not-an-execution"), 0o600) }, false},
		{"dangling symlink", func(path string) error {
			if err := os.Remove(path); err != nil {
				return err
			}
			return os.Symlink("missing-target", path)
		}, false},
		{"missing", func(path string) error { return os.Remove(path) }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := successionSessionFiles(t, "claude", "s", 1, "ex_one")
			executionPath := s.memory.name("claude", "s") + ".execution"
			if err := tc.damage(executionPath); err != nil {
				t.Fatal(err)
			}
			generation, present, err := s.RecallBindingGeneration("claude", "s")
			if tc.missing {
				if generation != 1 || !present || err != nil {
					t.Fatalf("missing execution changed generation recall: %d %t %v", generation, present, err)
				}
				return
			}
			if generation != 1 || !present || !errors.Is(err, ErrInvalidGeneration) {
				t.Fatalf("damaged execution recall: %d %t %v", generation, present, err)
			}
			assertRecallObservation(t, s.tokenPath, DamagedGenerationProof+" at "+executionPath) // gitleaks:allow (a test observation token and path, not a credential)
		})
	}
}

func TestReadExecutionProofValidation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "proof")
	for _, tc := range []struct {
		body  string
		valid bool
	}{{"ex_one", true}, {"ex_OK-1_", true}, {"", false}, {"ex_", false}, {"other", false}, {"ex_bad space", false}, {"ex_bad\nline", false}} {
		if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := readExecutionProof(path)
		if tc.valid && (got != tc.body || err != nil) {
			t.Fatalf("%q: got %q, %v", tc.body, got, err)
		}
		if !tc.valid && !errors.Is(err, ErrInvalidGeneration) {
			t.Fatalf("%q: got %q, %v", tc.body, got, err)
		}
	}
}

func TestPrepareSuccessionProofBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name             string
		minimum          int
		last             string
		ended            string
		removeGeneration bool
		removeExecution  bool
		damage           string
		wantErr          error
		damagedPath      string
	}{
		{name: "valid", minimum: 3, last: "ex_three", ended: "2"},
		{name: "below process generation", minimum: 4, last: "ex_three", wantErr: ErrSuccessionProof},
		{name: "last execution mismatch", minimum: 3, last: "ex_other", wantErr: ErrSuccessionProof},
		{name: "missing generation", minimum: 0, removeGeneration: true, wantErr: ErrSuccessionProof},
		{name: "missing execution", minimum: 3, last: "ex_three", removeExecution: true, wantErr: ErrSuccessionProof},
		{name: "ended current generation", minimum: 3, last: "ex_three", ended: "3", wantErr: ErrSuccessionProof},
		{name: "damaged generation", minimum: 3, last: "ex_three", damage: "generation", wantErr: ErrInvalidGeneration, damagedPath: "generation"},
		{name: "damaged execution", minimum: 3, last: "ex_three", damage: "execution", wantErr: ErrInvalidGeneration, damagedPath: "execution"},
		{name: "damaged ended", minimum: 3, last: "ex_three", damage: "ended", wantErr: ErrInvalidGeneration, damagedPath: "ended"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := successionSessionFiles(t, "claude", "s", 3, "ex_three")
			path := s.memory.name("claude", "s")
			if tc.removeGeneration {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			if tc.removeExecution {
				if err := os.Remove(path + ".execution"); err != nil {
					t.Fatal(err)
				}
			}
			if tc.ended != "" {
				if err := os.WriteFile(path+".ended", []byte(tc.ended), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if tc.damage != "" {
				suffix := map[string]string{"generation": "", "execution": ".execution", "ended": ".ended"}[tc.damage]
				if err := os.WriteFile(path+suffix, []byte("damaged"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			generation, execution, err := s.PrepareSuccession("claude", "s", tc.minimum, tc.last, proc.Ancestor{})
			if tc.wantErr == nil {
				if generation != 3 || execution != "ex_three" || err != nil {
					t.Fatalf("PrepareSuccession: %d %q %v", generation, execution, err)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("got %v, want %v", err, tc.wantErr)
			}
			if tc.damagedPath != "" {
				suffix := map[string]string{"generation": "", "execution": ".execution", "ended": ".ended"}[tc.damagedPath]
				assertRecallObservation(t, s.tokenPath, DamagedGenerationProof+" at "+path+suffix) // gitleaks:allow (a test observation token and path, not a credential)
			}
		})
	}
}

func TestPrepareAndAdvanceFailWhenSessionFilesCannotBeLocked(t *testing.T) {
	s := successionSessionFiles(t, "claude", "s", 1, "ex_one")
	unlock, err := s.Lock(hookLockBudget)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.PrepareSuccession("claude", "s", 1, "ex_one", proc.Ancestor{}); !errors.Is(err, ErrSessionsLocked) {
		t.Fatalf("PrepareSuccession lock: %v", err)
	}
	if _, _, err := s.AdvanceGeneration("claude", "s", "ex_one", 1, 2, "ex_two"); !errors.Is(err, ErrSessionsLocked) {
		t.Fatalf("AdvanceGeneration lock: %v", err)
	}
	unlock()
	parent := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(parent, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	blocked := NewSessions(filepath.Join(parent, "hub.token"))
	if _, _, err := blocked.PrepareSuccession("claude", "s", 1, "ex_one", proc.Ancestor{}); err == nil {
		t.Fatal("PrepareSuccession protected an invalid directory")
	}
	if _, _, err := blocked.AdvanceGeneration("claude", "s", "ex_one", 1, 2, "ex_two"); err == nil {
		t.Fatal("AdvanceGeneration protected an invalid directory")
	}
}

func TestGenericProcessWitnessAndCursorIDEExclusion(t *testing.T) {
	agent := proc.Ancestor{PID: 42, StartMS: 100, Name: "cursor-agent"}
	s := successionSessionFiles(t, "cursor", "S1", 1, "ex_one")
	if err := s.memory.rememberProcessSession("cursor", "S1", agent); err != nil {
		t.Fatal(err)
	}
	if current, ok, err := s.CurrentSession("cursor", agent); current != "S1" || !ok || err != nil {
		t.Fatalf("CurrentSession: %q %t %v", current, ok, err)
	}
	if generation, execution, err := s.PrepareSuccession("cursor", "S1", 1, "ex_one", agent); generation != 1 || execution != "ex_one" || err != nil {
		t.Fatalf("process witness: %d %q %v", generation, execution, err)
	}
	if err := s.memory.rememberProcessSession("cursor", "S2", agent); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.PrepareSuccession("cursor", "S1", 1, "ex_one", agent); !errors.Is(err, ErrSuccessionProof) {
		t.Fatalf("superseded session passed its witness: %v", err)
	}
	if err := os.WriteFile(processWitnessPath(s.memory.dir, agent), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.PrepareSuccession("cursor", "S1", 1, "ex_one", agent); !errors.Is(err, ErrSuccessionProof) {
		t.Fatalf("empty witness passed: %v", err)
	}
	if err := os.Remove(processWitnessPath(s.memory.dir, agent)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.PrepareSuccession("cursor", "S1", 1, "ex_one", agent); !errors.Is(err, ErrSuccessionProof) {
		t.Fatalf("missing witness passed: %v", err)
	}
	ide := proc.Ancestor{PID: 42, StartMS: 100, Name: "cursor"}
	if err := s.memory.rememberProcessSession("cursor", "S1", ide); err != nil {
		t.Fatal(err)
	}
	if current, ok, err := s.CurrentSession("cursor", ide); current != "" || ok || err != nil {
		t.Fatalf("Cursor IDE supplied process evidence: %q %t %v", current, ok, err)
	}
	if _, err := os.Stat(processWitnessPath(s.memory.dir, ide)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Cursor IDE wrote a witness: %v", err)
	}
	if current, ok, err := s.CurrentSession("cursor", proc.Ancestor{}); current != "" || ok || err != nil {
		t.Fatalf("empty ancestor supplied process evidence: %q %t %v", current, ok, err)
	}
}

func TestAntigravityAndKimiCurrentSessionChecks(t *testing.T) {
	t.Run("antigravity", func(t *testing.T) {
		pinned := t.TempDir()
		t.Cleanup(config.SetPinnedForTest(pinned))
		anc := proc.Ancestor{PID: 42, StartMS: 100, Name: "agy"}
		other := antigravityHelpersMemory(pinned)
		if err := other.directory(); err != nil {
			t.Fatal(err)
		}
		if err := writeAntigravityJSON(other.dir, antigravityProcFile(other.dir, anc.PID, anc.StartMS), antigravityProcState{CurrentSessionID: "unrelated"}); err != nil {
			t.Fatal(err)
		}
		s := successionSessionFiles(t, "antigravity", "S1", 1, "ex_one")
		if current, ok, err := s.CurrentSession("antigravity", anc); current != "" || ok || err != nil {
			t.Fatalf("missing Antigravity state: %q %t %v", current, ok, err)
		}
		if _, _, err := s.PrepareSuccession("antigravity", "S1", 1, "ex_one", anc); !errors.Is(err, ErrSuccessionProof) {
			t.Fatalf("missing Antigravity state passed: %v", err)
		}
		m := antigravityHelpersMemory(filepath.Dir(s.tokenPath))
		if err := m.directory(); err != nil {
			t.Fatal(err)
		}
		if current, ok, err := s.CurrentSession("antigravity", anc); current != "" || ok || err != nil {
			t.Fatalf("existing helper directory supplied absent process state: %q %t %v", current, ok, err)
		}
		path := antigravityProcFile(m.dir, anc.PID, anc.StartMS)
		if err := writeAntigravityJSON(m.dir, path, antigravityProcState{CurrentSessionID: "S1"}); err != nil {
			t.Fatal(err)
		}
		if current, ok, err := s.CurrentSession("antigravity", anc); current != "S1" || !ok || err != nil {
			t.Fatalf("Antigravity CurrentSession: %q %t %v", current, ok, err)
		}
		if _, _, err := s.PrepareSuccession("antigravity", "S1", 1, "ex_one", anc); err != nil {
			t.Fatalf("Antigravity current state was refused: %v", err)
		}
		if err := writeAntigravityJSON(m.dir, path, antigravityProcState{CurrentSessionID: "S2", SupersededSessionIDs: []string{"S1"}}); err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.PrepareSuccession("antigravity", "S1", 1, "ex_one", anc); !errors.Is(err, ErrSuccessionProof) {
			t.Fatalf("Antigravity supersession was not refused: %v", err)
		}
		if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.CurrentSession("antigravity", anc); !errors.Is(err, ErrSuccessionProof) {
			t.Fatalf("damaged Antigravity state: %v", err)
		}
	})
	t.Run("kimi", func(t *testing.T) {
		restore := config.SetPinnedForTest(t.TempDir())
		t.Cleanup(restore)
		anc := proc.Ancestor{PID: 42, StartMS: 100, Name: "kimi"}
		const s1 = "session_00000000-0000-4000-8000-000000000001"
		const s2 = "session_00000000-0000-4000-8000-000000000002"
		s := successionSessionFiles(t, "kimi", s1, 1, "ex_one")
		if current, ok, err := s.CurrentSession("kimi", anc); current != "" || ok || err != nil {
			t.Fatalf("missing Kimi state: %q %t %v", current, ok, err)
		}
		dir, _ := config.PinnedDir()
		wake := &kimiwake.Store{Root: kimiwake.Root(dir)}
		if err := wake.MapSession(anc.PID, anc.StartMS, s1, time.Second); err != nil {
			t.Fatal(err)
		}
		if current, ok, err := s.CurrentSession("kimi", anc); current != s1 || !ok || err != nil {
			t.Fatalf("Kimi CurrentSession: %q %t %v", current, ok, err)
		}
		if _, _, err := s.PrepareSuccession("kimi", s1, 1, "ex_one", anc); err != nil {
			t.Fatalf("Kimi current state was refused: %v", err)
		}
		if err := wake.MapSession(anc.PID, anc.StartMS, s2, time.Second); err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.PrepareSuccession("kimi", s1, 1, "ex_one", anc); !errors.Is(err, ErrSuccessionProof) {
			t.Fatalf("Kimi supersession was not refused: %v", err)
		}
	})
}

func TestAdvanceGenerationCASAndConcurrentMemoryWriters(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		expectedExec, newExec string
		expectedOld, newGen   int
	}{
		{"old generation", "ex_one", "ex_two", 0, 1},
		{"old execution", "", "ex_two", 1, 2},
		{"new generation", "ex_one", "ex_two", 1, 3},
		{"new execution", "ex_one", "", 1, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := successionSessionFiles(t, "claude", "s", 1, "ex_one")
			if _, _, err := s.AdvanceGeneration("claude", "s", tc.expectedExec, tc.expectedOld, tc.newGen, tc.newExec); !errors.Is(err, ErrSuccessionProof) {
				t.Fatalf("invalid CAS: %v", err)
			}
		})
	}
	s := successionSessionFiles(t, "claude", "s", 1, "ex_one")
	current, updated, err := s.AdvanceGeneration("claude", "s", "ex_one", 1, 2, "ex_two")
	if current != 2 || !updated || err != nil {
		t.Fatalf("advance: %d %t %v", current, updated, err)
	}
	if generation, execution, _ := s.PrepareSuccession("claude", "s", 2, "ex_two", proc.Ancestor{}); generation != 2 || execution != "ex_two" {
		t.Fatalf("advanced proof: %d %q", generation, execution)
	}
	if current, updated, err := s.AdvanceGeneration("claude", "s", "ex_one", 1, 2, "ex_two"); current != 2 || updated || err != nil {
		t.Fatalf("stale advance won: %d %t %v", current, updated, err)
	}

	for range 12 {
		s := successionSessionFiles(t, "claude", "s", 1, "ex_one")
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, _, _ = s.AdvanceGeneration("claude", "s", "ex_one", 1, 2, "ex_two")
		}()
		go func() { defer wg.Done(); <-start; s.memory.remember("claude", "s", 3, "ex_three") }()
		close(start)
		wg.Wait()
		generation, execution, err := s.PrepareSuccession("claude", "s", 3, "ex_three", proc.Ancestor{})
		if generation != 3 || execution != "ex_three" || err != nil {
			t.Fatalf("advance downgraded a hook: %d %q %v", generation, execution, err)
		}
	}
	for range 12 {
		s := successionSessionFiles(t, "claude", "s", 1, "ex_one")
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, _, _ = s.AdvanceGeneration("claude", "s", "ex_one", 1, 2, "ex_two")
		}()
		go func() { defer wg.Done(); <-start; s.memory.Forget("claude", "s", 1) }()
		close(start)
		wg.Wait()
		generation, generationErr := readGeneration(s.memory.name("claude", "s"))
		execution, executionErr := readExecutionProof(s.memory.name("claude", "s") + ".execution")
		if generationErr != nil || executionErr != nil || generation == 0 && execution != "" || generation != 0 && generation != 2 {
			t.Fatalf("advance recreated or mismatched forgotten proof: generation=%d execution=%q errors=%v/%v", generation, execution, generationErr, executionErr)
		}
	}
}

func TestAdvanceGenerationMismatchAndDamage(t *testing.T) {
	t.Run("forgotten", func(t *testing.T) {
		s := successionSessionFiles(t, "claude", "s", 1, "ex_one")
		s.memory.Forget("claude", "s", 1)
		current, updated, err := s.AdvanceGeneration("claude", "s", "ex_one", 1, 2, "ex_two")
		if current != 0 || updated || err != nil {
			t.Fatalf("forgotten advance: %d %t %v", current, updated, err)
		}
		if _, err := os.Lstat(s.memory.name("claude", "s")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("advance recreated forgotten proof: %v", err)
		}
	})
	for _, tc := range []struct {
		name, body, suffix string
	}{
		{"damaged generation", "bad", ""},
		{"damaged execution", "bad", ".execution"},
		{"damaged ended", "bad", ".ended"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := successionSessionFiles(t, "claude", "s", 1, "ex_one")
			path := s.memory.name("claude", "s") + tc.suffix
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, _, err := s.AdvanceGeneration("claude", "s", "ex_one", 1, 2, "ex_two"); !errors.Is(err, ErrInvalidGeneration) {
				t.Fatalf("advance damage: %v", err)
			}
			assertRecallObservation(t, s.tokenPath, DamagedGenerationProof+" at "+path) // gitleaks:allow (a test observation token and path, not a credential)
		})
	}
	t.Run("older and mismatched current proof", func(t *testing.T) {
		s := successionSessionFiles(t, "claude", "s", 1, "ex_one")
		if current, updated, err := s.AdvanceGeneration("claude", "s", "ex_one", 2, 3, "ex_three"); current != 1 || updated || err != nil {
			t.Fatalf("older current: %d %t %v", current, updated, err)
		}
		if current, updated, err := s.AdvanceGeneration("claude", "s", "ex_other", 1, 2, "ex_two"); current != 1 || updated || err != nil {
			t.Fatalf("execution mismatch: %d %t %v", current, updated, err)
		}
		if err := os.WriteFile(s.memory.name("claude", "s")+".ended", []byte("1"), 0o600); err != nil {
			t.Fatal(err)
		}
		if current, updated, err := s.AdvanceGeneration("claude", "s", "ex_one", 1, 2, "ex_two"); current != 0 || updated || err != nil {
			t.Fatalf("ended proof: %d %t %v", current, updated, err)
		}
	})
}

type successionHookHub struct{ request hubclient.SessionRequest }

func (h *successionHookHub) RegisterSession(_ context.Context, req hubclient.SessionRequest) (hubclient.SessionResponse, error) {
	h.request = req
	return hubclient.SessionResponse{ExecutionID: "ex_one", Generation: 1}, nil
}
func (h *successionHookHub) Touch(context.Context, hubclient.TouchRequest) (hubclient.TouchResponse, error) {
	return hubclient.TouchResponse{Action: "allow"}, nil
}
func (h *successionHookHub) EndExecution(context.Context, string, int) error { return nil }

func TestSessionStartWritesExecutionProofAndProcessEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, ancestor string
		single         bool
	}{
		{"cursor-agent", "cursor-agent", true},
		{"Cursor IDE", "cursor", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newFileMemory(t)
			hub := &successionHookHub{}
			anc := proc.Ancestor{PID: 42, StartMS: 100, Name: tc.ancestor}
			out := (Runner{
				ClientName: "cursor", EventName: "sessionStart", Stdin: strings.NewReader(`{"session_id":"S","cwd":"/repo"}`),
				Hub: hub, Memory: m, Host: func() string { return "host" },
				Ancestor: func() (proc.Ancestor, error) { return anc, nil },
				Find:     func(string) (gitinfo.Repo, error) { return gitinfo.Repo{Worktree: "/repo"}, nil },
				after:    never, logAfter: never,
			}).Run()
			if out.Exit != 0 || hub.request.SingleSession != tc.single {
				t.Fatalf("SessionStart: %+v single=%t", out, hub.request.SingleSession)
			}
			execution, err := readExecutionProof(m.name("cursor", "S") + ".execution")
			if execution != "ex_one" || err != nil {
				t.Fatalf("SessionStart execution proof: %q %v", execution, err)
			}
			_, err = os.Stat(processWitnessPath(m.dir, anc))
			if tc.single && err != nil {
				t.Fatalf("SessionStart witness: %v", err)
			}
			if !tc.single && !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("Cursor IDE wrote process evidence: %v", err)
			}
		})
	}
}

func TestProcessWitnessReadAndDirectoryFailures(t *testing.T) {
	t.Run("witness is not a file", func(t *testing.T) {
		anc := proc.Ancestor{PID: 42, StartMS: 100, Name: "cursor-agent"}
		s := successionSessionFiles(t, "cursor", "S1", 1, "ex_one")
		if err := os.Mkdir(processWitnessPath(s.memory.dir, anc), 0o700); err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.CurrentSession("cursor", anc); err == nil {
			t.Fatal("a directory passed as a process witness")
		}
	})
	t.Run("session directory cannot be protected", func(t *testing.T) {
		parent := filepath.Join(t.TempDir(), "file")
		if err := os.WriteFile(parent, []byte("not a directory"), 0o600); err != nil {
			t.Fatal(err)
		}
		m := newFileMemory(t)
		m.dir = filepath.Join(parent, "sessions")
		anc := proc.Ancestor{PID: 42, StartMS: 100, Name: "cursor-agent"}
		if err := m.rememberProcessSession("cursor", "S1", anc); err == nil {
			t.Fatal("process witness write ignored an invalid session directory")
		}
	})
}

func TestProcessStateLookupFailures(t *testing.T) {
	t.Run("antigravity helper path is a file", func(t *testing.T) {
		s := successionSessionFiles(t, "antigravity", "S1", 1, "ex_one")
		anc := proc.Ancestor{PID: 42, StartMS: 100, Name: "agy"}
		m := antigravityHelpersMemory(filepath.Dir(s.tokenPath))
		if err := os.WriteFile(m.dir, []byte("invalid directory"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.CurrentSession("antigravity", anc); err == nil {
			t.Fatal("invalid helper directory was treated as hookless")
		}
	})
	t.Run("pinned directory", func(t *testing.T) {
		original := successionPinnedDir
		successionPinnedDir = func() (string, error) { return "", errors.New("pinned lookup failed") }
		t.Cleanup(func() { successionPinnedDir = original })
		for _, tc := range []struct {
			client, ancestor string
		}{{"kimi", "kimi"}} {
			s := successionSessionFiles(t, tc.client, "S1", 1, "ex_one")
			anc := proc.Ancestor{PID: 42, StartMS: 100, Name: tc.ancestor}
			if _, _, err := s.CurrentSession(tc.client, anc); err == nil || err.Error() != "pinned lookup failed" {
				t.Fatalf("%s ignored pinned lookup failure: %v", tc.client, err)
			}
		}
	})
	t.Run("antigravity lock", func(t *testing.T) {
		s := successionSessionFiles(t, "antigravity", "S1", 1, "ex_one")
		anc := proc.Ancestor{PID: 42, StartMS: 100, Name: "agy"}
		m := antigravityHelpersMemory(filepath.Dir(s.tokenPath))
		if err := m.directory(); err != nil {
			t.Fatal(err)
		}
		if err := writeAntigravityJSON(m.dir, antigravityProcFile(m.dir, anc.PID, anc.StartMS), antigravityProcState{CurrentSessionID: "S1"}); err != nil {
			t.Fatal(err)
		}
		unlock, err := m.lock(hookLockBudget)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.CurrentSession("antigravity", anc); !errors.Is(err, ErrSessionsLocked) {
			t.Fatalf("Antigravity lock was ignored: %v", err)
		}
		unlock()
	})
	t.Run("kimi lock", func(t *testing.T) {
		restore := config.SetPinnedForTest(t.TempDir())
		t.Cleanup(restore)
		dir, _ := config.PinnedDir()
		wake := &kimiwake.Store{Root: kimiwake.Root(dir)}
		const session = "session_00000000-0000-4000-8000-000000000001"
		anc := proc.Ancestor{PID: 42, StartMS: 100, Name: "kimi"}
		if err := wake.MapSession(anc.PID, anc.StartMS, session, time.Second); err != nil {
			t.Fatal(err)
		}
		file, err := os.OpenFile(filepath.Join(wake.Root, ".lock"), os.O_RDWR, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		if err := tryLockFile(file); err != nil {
			t.Fatal(err)
		}
		s := successionSessionFiles(t, "kimi", session, 1, "ex_one")
		if _, _, err := s.CurrentSession("kimi", anc); !errors.Is(err, kimiwake.ErrLocked) {
			t.Fatalf("Kimi lock was ignored: %v", err)
		}
		unlockFile(file)
		_ = file.Close()
	})
}

func TestAdvanceGenerationWriteFailures(t *testing.T) {
	for _, failAt := range []int{1, 2} {
		t.Run(fmt.Sprintf("write %d", failAt), func(t *testing.T) {
			s := successionSessionFiles(t, "claude", "s", 1, "ex_one")
			writes := 0
			restrict := s.memory.restrict
			s.memory.restrict = func(path string) error {
				if strings.HasPrefix(filepath.Base(path), ".tmp-") {
					writes++
					if writes == failAt {
						return errors.New("injected proof write failure")
					}
				}
				return restrict(path)
			}
			if _, _, err := s.AdvanceGeneration("claude", "s", "ex_one", 1, 2, "ex_two"); err == nil || err.Error() != "injected proof write failure" {
				t.Fatalf("write failure %d: %v", failAt, err)
			}
			generation, generationErr := readGeneration(s.memory.name("claude", "s"))
			execution, executionErr := readExecutionProof(s.memory.name("claude", "s") + ".execution")
			wantExecution := "ex_one"
			if failAt == 2 {
				wantExecution = "ex_two"
			}
			if generation != 1 || execution != wantExecution || generationErr != nil || executionErr != nil {
				t.Fatalf("write failure %d left generation=%d execution=%q errors=%v/%v", failAt, generation, execution, generationErr, executionErr)
			}
			if failAt != 2 {
				return
			}
			current, updated, err := s.AdvanceGeneration("claude", "s", "ex_one", 1, 2, "ex_two")
			if err != nil || updated || current != 1 {
				t.Fatalf("partial write did not fail closed: current=%d updated=%t err=%v", current, updated, err)
			}
			if generation, err = readGeneration(s.memory.name("claude", "s")); err != nil || generation != 1 {
				t.Fatalf("partial write published generation %d: %v", generation, err)
			}
			if _, _, err = s.PrepareSuccession("claude", "s", 1, "ex_one", proc.Ancestor{}); !errors.Is(err, ErrSuccessionProof) {
				t.Fatalf("stale execution proof was accepted after a partial write: %v", err)
			}
		})
	}
}

func TestPrepareSuccessionReadsProcessWitnessOutsideTheSessionsLock(t *testing.T) {
	agent := proc.Ancestor{PID: 42, StartMS: 100, Name: "cursor-agent"}
	original := processSessionLookup
	t.Cleanup(func() { processSessionLookup = original })
	for _, tc := range []struct {
		name    string
		reads   []witnessRead
		wantErr error
	}{
		{name: "change between reads", reads: []witnessRead{{current: "S1"}, {current: "S2", superseded: []string{"S1"}}}, wantErr: ErrSuccessionProof},
		{name: "first read fails", reads: []witnessRead{{err: errors.New("witness unavailable")}}, wantErr: errors.New("witness unavailable")},
		{name: "second read fails", reads: []witnessRead{{current: "S1"}, {err: errors.New("witness changed unreadable")}}, wantErr: errors.New("witness changed unreadable")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := successionSessionFiles(t, "cursor", "S1", 1, "ex_one")
			calls := 0
			processSessionLookup = func(string, proc.Ancestor, string) (string, []string, error) {
				calls++
				unlock, err := s.Lock(hookLockBudget)
				if err != nil {
					t.Errorf("witness read %d held the sessions lock: %v", calls, err)
					return "", nil, err
				}
				unlock()
				read := tc.reads[calls-1]
				return read.current, read.superseded, read.err
			}
			generation, execution, err := s.PrepareSuccession("cursor", "S1", 1, "ex_one", agent)
			if generation != 0 || execution != "" || err == nil || err.Error() != tc.wantErr.Error() {
				t.Fatalf("PrepareSuccession: %d %q %v", generation, execution, err)
			}
			if calls != len(tc.reads) {
				t.Fatalf("witness reads = %d, want %d", calls, len(tc.reads))
			}
			gotGeneration, generationErr := readGeneration(s.memory.name("cursor", "S1"))
			gotExecution, executionErr := readExecutionProof(s.memory.name("cursor", "S1") + ".execution")
			if gotGeneration != 1 || gotExecution != "ex_one" || generationErr != nil || executionErr != nil {
				t.Fatalf("proof changed: generation=%d execution=%q errors=%v/%v", gotGeneration, gotExecution, generationErr, executionErr)
			}
		})
	}
}

type witnessRead struct {
	current    string
	superseded []string
	err        error
}
