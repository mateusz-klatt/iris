package binding

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/gitinfo"
	"github.com/mateusz-klatt/iris/internal/hook"
	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/proc"
	"github.com/mateusz-klatt/iris/internal/store"
)

type successionBindingFixture struct {
	hub      *restartableBindingHub
	token    string
	cwd      string
	sessions *hook.Sessions
}

func newSuccessionBindingFixture(t *testing.T, clientName, eventName, session string, anc proc.Ancestor) successionBindingFixture {
	t.Helper()
	h := newRestartableBindingHub(t)
	cwd := t.TempDir()
	token := filepath.Join(t.TempDir(), "hub.token")
	if err := os.WriteFile(token, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	getenv := successionEnvironment(clientName, session, token)
	out := (hook.Runner{
		ClientName: clientName, EventName: eventName, Stdin: strings.NewReader(`{"session_id":` + quoteJSON(session) + `,"cwd":` + quoteJSON(cwd) + `}`),
		Hub: &hubclient.Client{Config: config{h.srv.URL}}, Getenv: getenv, Host: func() string { return "floor-host" },
		Ancestor: func() (proc.Ancestor, error) { return anc, nil },
		Find:     func(string) (gitinfo.Repo, error) { return gitinfo.Repo{}, gitinfo.ErrNotRepo },
	}).Run()
	if failures, err := hook.ReadRecentFailures(token, 10); err != nil || len(failures) != 0 {
		t.Fatalf("SessionStart failures: %v %v, stdout=%s stderr=%s", failures, err, out.Stdout, out.Stderr)
	}
	return successionBindingFixture{hub: h, token: token, cwd: cwd, sessions: hook.NewSessions(token)}
}

func successionEnvironment(clientName, session, token string) func(string) string {
	return func(key string) string {
		if key == "IRIS_TOKEN_FILE" {
			return token
		}
		if clientName == "claude" && key == "CLAUDE_CODE_SESSION_ID" || clientName == "grok" && key == "GROK_SESSION_ID" {
			return session
		}
		return ""
	}
}

func (f successionBindingFixture) resolver(clientName, session string, anc proc.Ancestor) *Resolver {
	return &Resolver{
		Hub: &hubclient.Client{Config: config{f.hub.srv.URL}}, Client: clientName, Host: "floor-host", CWD: f.cwd, Grace: 0,
		Getenv: successionEnvironment(clientName, session, f.token), Ancestor: func() (proc.Ancestor, error) { return anc, nil },
		Recall: f.sessions.RecallBindingGeneration, Succession: f.sessions, ParentAlive: func() bool { return true },
		ObserveBindingRefusal: f.sessions.ObserveBindingRefusal,
	}
}

func (f successionBindingFixture) generationPath(t *testing.T) string {
	t.Helper()
	proof, _ := sessionProof(t, filepath.Join(filepath.Dir(f.token), "sessions"))
	if proof == "" {
		t.Fatal("session proof is absent")
	}
	return filepath.Join(filepath.Dir(f.token), "sessions", proof)
}

func nonHookSessionRequests(requests []hubclient.SessionRequest) []hubclient.SessionRequest {
	out := []hubclient.SessionRequest{}
	for _, req := range requests {
		if req.Source != "hook" {
			out = append(out, req)
		}
	}
	return out
}

func assertNoGenerationZeroRegistration(t *testing.T, requests []hubclient.SessionRequest) {
	t.Helper()
	for _, req := range nonHookSessionRequests(requests) {
		if req.Generation == 0 || req.RetryGeneration != 0 {
			t.Fatalf("succession degraded to an unguarded registration: %+v", req)
		}
	}
}

func TestMCPPostWindowSuccessionRung2AndOneHopBound(t *testing.T) {
	f := newSuccessionBindingFixture(t, "claude", "SessionStart", "S", proc.Ancestor{})
	rows := executionRows(t, f.hub.file)
	if len(rows) != 1 {
		t.Fatalf("fixture rows: %+v", rows)
	}
	f.hub.clock.Advance(16 * time.Minute)
	r := f.resolver("claude", "S", proc.Ancestor{})
	b, err := r.Resolve(t.Context(), "")
	if err != nil || b.Generation != 2 || b.ExecutionID == rows[0].id || b.SessionID != "S" || b.Rung != 2 || b.HookSeen {
		t.Fatalf("Rung 2 succession: bound=%+v err=%v", b, err)
	}
	requests := f.hub.successions()
	if len(requests) != 1 || requests[0]["prior_generation"] != float64(1) || requests[0]["prior_execution_id"] != rows[0].id || requests[0]["ended_through"] != float64(0) || requests[0]["client_pid"] != float64(0) || requests[0]["client_pid_start_ms"] != float64(0) {
		t.Fatalf("succession proof request: %+v", requests)
	}
	assertNoGenerationZeroRegistration(t, f.hub.sessions())
	generation, execution, err := f.sessions.PrepareSuccession("claude", "S", 2, b.ExecutionID, proc.Ancestor{})
	if generation != 2 || execution != b.ExecutionID || err != nil {
		t.Fatalf("local proof was not advanced: %d %q %v", generation, execution, err)
	}
	rows = executionRows(t, f.hub.file)
	if len(rows) != 2 || rows[0].state != "lost" || rows[1].state != "active" || rows[1].hookSeen != 0 {
		t.Fatalf("successor rows: %+v", rows)
	}

	f.hub.clock.Advance(61 * time.Second)
	if _, err := f.hub.store.Call(t.Context(), "expire", store.Request{}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Resolve(t.Context(), ""); !IsCode(err, "lost") {
		t.Fatalf("second automatic hop was not refused: %v", err)
	}
	if len(f.hub.successions()) != 2 || len(executionRows(t, f.hub.file)) != 2 {
		t.Fatalf("second hop changed history: successions=%d rows=%+v", len(f.hub.successions()), executionRows(t, f.hub.file))
	}
	assertNoGenerationZeroRegistration(t, f.hub.sessions())
}

func TestMCPPostWindowSuccessionFreshRung3AndCursorIDEExclusion(t *testing.T) {
	anc := proc.Ancestor{PID: 42, StartMS: 100, Name: "cursor-agent"}
	f := newSuccessionBindingFixture(t, "cursor", "sessionStart", "real-session", anc)
	rows := executionRows(t, f.hub.file)
	f.hub.clock.Advance(16 * time.Minute)
	r := f.resolver("cursor", "", anc)
	b, err := r.Resolve(t.Context(), "")
	if err != nil || b.Generation != 2 || b.ExecutionID == rows[0].id || b.SessionID != "real-session" || b.Rung != 3 {
		t.Fatalf("fresh Rung 3 succession: bound=%+v err=%v", b, err)
	}
	requests := f.hub.successions()
	if len(requests) != 1 || requests[0]["client_pid"] != float64(42) || requests[0]["client_pid_start_ms"] != float64(100) {
		t.Fatalf("Rung 3 process proof: %+v", requests)
	}
	assertNoGenerationZeroRegistration(t, f.hub.sessions())

	ide := proc.Ancestor{PID: 42, StartMS: 100, Name: "cursor"}
	fIDE := newSuccessionBindingFixture(t, "cursor", "sessionStart", "ide-session", ide)
	fIDE.hub.clock.Advance(16 * time.Minute)
	rIDE := fIDE.resolver("cursor", "", ide)
	if _, err := rIDE.Resolve(t.Context(), ""); !IsCode(err, "unbound") {
		t.Fatalf("Cursor IDE unexpectedly succeeded: %v", err)
	}
	if len(fIDE.hub.successions()) != 0 || len(nonHookSessionRequests(fIDE.hub.sessions())) != 0 {
		t.Fatalf("Cursor IDE emitted registration or succession: %+v %+v", fIDE.hub.sessions(), fIDE.hub.successions())
	}
}

func TestMCPPostWindowSuccessionCarriesEndFloorAndHonorsVeto(t *testing.T) {
	secondGeneration := func(t *testing.T) successionBindingFixture {
		t.Helper()
		f := newSuccessionBindingFixture(t, "claude", "SessionStart", "S", proc.Ancestor{})
		for _, event := range []string{"SessionEnd", "SessionStart"} {
			out := (hook.Runner{
				ClientName: "claude", EventName: event, Stdin: strings.NewReader(`{"session_id":"S","cwd":` + quoteJSON(f.cwd) + `}`),
				Hub: &hubclient.Client{Config: config{f.hub.srv.URL}}, Getenv: successionEnvironment("claude", "S", f.token), Host: func() string { return "floor-host" },
				Ancestor: func() (proc.Ancestor, error) { return proc.Ancestor{}, nil },
				Find:     func(string) (gitinfo.Repo, error) { return gitinfo.Repo{}, gitinfo.ErrNotRepo },
			}).Run()
			if failures, err := hook.ReadRecentFailures(f.token, 10); err != nil || len(failures) != 0 {
				t.Fatalf("%s failures: %v %v, stdout=%s stderr=%s", event, failures, err, out.Stdout, out.Stderr)
			}
		}
		return f
	}
	t.Run("older marker is the strict floor", func(t *testing.T) {
		f := secondGeneration(t)
		f.hub.clock.Advance(16 * time.Minute)
		r := f.resolver("claude", "S", proc.Ancestor{})
		b, err := r.Resolve(t.Context(), "")
		if err != nil || b.Generation != 3 {
			t.Fatalf("succession above the end floor: bound=%+v err=%v", b, err)
		}
		requests := f.hub.successions()
		if len(requests) != 1 || requests[0]["prior_generation"] != float64(2) || requests[0]["ended_through"] != float64(1) {
			t.Fatalf("succession end floor: %+v", requests)
		}
	})
	t.Run("marker at the prior generation vetoes", func(t *testing.T) {
		f := secondGeneration(t)
		path := f.generationPath(t) + ".ended"
		if err := os.WriteFile(path, []byte("2"), 0o600); err != nil {
			t.Fatal(err)
		}
		f.hub.clock.Advance(16 * time.Minute)
		r := f.resolver("claude", "S", proc.Ancestor{})
		if b, err := r.Resolve(t.Context(), ""); b.ExecutionID != "" || !IsCode(err, "lost") {
			t.Fatalf("ended proof succeeded: bound=%+v err=%v", b, err)
		}
		if len(f.hub.successions()) != 0 {
			t.Fatalf("ended proof reached succession: %+v", f.hub.successions())
		}
		assertBindingObservation(t, f.token, hook.EndedGenerationRefused+" at "+path)
		assertNoGenerationZeroRegistration(t, f.hub.sessions())
	})
}

func TestChoiceAProoflessMCPAndWatchAfterLossAreRefused(t *testing.T) {
	for _, mode := range []string{"mcp", "watch"} {
		t.Run(mode, func(t *testing.T) {
			f := newSuccessionBindingFixture(t, "claude", "SessionStart", "S", proc.Ancestor{})
			path := f.generationPath(t)
			for _, proof := range []string{path, path + ".execution"} {
				if err := os.Remove(proof); err != nil {
					t.Fatal(err)
				}
			}
			f.hub.clock.Advance(16 * time.Minute)
			if _, err := f.hub.store.Call(t.Context(), "expire", store.Request{}); err != nil {
				t.Fatal(err)
			}
			before := executionRows(t, f.hub.file)
			r := f.resolver("claude", "S", proc.Ancestor{})
			if mode == "watch" {
				r.HeartbeatSource = "watch"
			}
			if b, err := r.Resolve(t.Context(), ""); b.ExecutionID != "" || !IsCode(err, "lost") {
				t.Fatalf("proofless %s recovered: bound=%+v err=%v", mode, b, err)
			}
			requests := nonHookSessionRequests(f.hub.sessions())
			if len(requests) != 1 || requests[0].Generation != 0 || requests[0].Source != "mcp" || requests[0].SessionID != "S" {
				t.Fatalf("proofless %s requests: %+v", mode, requests)
			}
			if len(f.hub.successions()) != 0 {
				t.Fatalf("proofless %s attempted succession: %+v", mode, f.hub.successions())
			}
			after := executionRows(t, f.hub.file)
			if fmt.Sprint(before) != fmt.Sprint(after) || len(after) != 1 || after[0].state != "lost" {
				t.Fatalf("proofless %s changed history: before=%+v after=%+v", mode, before, after)
			}
		})
	}
}

func TestSuccessionExecutionProofDamageAndRestoreMismatchStayLocalAndUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name, generation, execution string
		damaged                     bool
	}{
		{name: "missing execution", generation: "", execution: ""},
		{name: "damaged execution", generation: "", execution: "damaged", damaged: true},
		{name: "ahead local proof", generation: "3", execution: "ex_live"},
		{name: "equal-generation different history", generation: "1", execution: "ex_live"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSuccessionBindingFixture(t, "claude", "SessionStart", "S", proc.Ancestor{})
			path := f.generationPath(t)
			if tc.generation != "" {
				if err := os.WriteFile(path, []byte(tc.generation), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if tc.execution != "" {
				if err := os.WriteFile(path+".execution", []byte(tc.generation+" "+tc.execution), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if tc.name == "missing execution" {
				if err := os.Remove(path + ".execution"); err != nil {
					t.Fatal(err)
				}
			}
			beforeGeneration, _ := os.ReadFile(path)
			beforeExecution, _ := os.ReadFile(path + ".execution")
			f.hub.clock.Advance(16 * time.Minute)
			if _, err := f.hub.store.Call(t.Context(), "expire", store.Request{}); err != nil {
				t.Fatal(err)
			}
			r := f.resolver("claude", "S", proc.Ancestor{})
			if _, err := r.Resolve(t.Context(), ""); !IsCode(err, "lost") {
				t.Fatalf("invalid proof recovered: %v", err)
			}
			if tc.damaged {
				assertBindingObservation(t, f.token, hook.DamagedGenerationProof+" at "+path+".execution") // gitleaks:allow (a test observation token and path, not a credential)
				if len(nonHookSessionRequests(f.hub.sessions())) != 0 {
					t.Fatalf("damaged execution reached registration: %+v", f.hub.sessions())
				}
			}
			assertNoGenerationZeroRegistration(t, f.hub.sessions())
			if tc.name == "missing execution" || tc.damaged {
				if len(f.hub.successions()) != 0 {
					t.Fatalf("invalid execution proof reached succession: %+v", f.hub.successions())
				}
			} else if len(f.hub.successions()) != 1 {
				t.Fatalf("restore mismatch did not make exactly one guarded request: %+v", f.hub.successions())
			}
			afterGeneration, _ := os.ReadFile(path)
			afterExecution, _ := os.ReadFile(path + ".execution")
			if string(afterGeneration) != string(beforeGeneration) || string(afterExecution) != string(beforeExecution) {
				t.Fatalf("refusal changed proof: generation %q->%q execution %q->%q", beforeGeneration, afterGeneration, beforeExecution, afterExecution)
			}
			if _, err := os.Lstat(path + ".ended"); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("refusal created an end marker: %v", err)
			}
			if rows := executionRows(t, f.hub.file); len(rows) != 1 || rows[0].state != "lost" {
				t.Fatalf("refusal changed retained history: %+v", rows)
			}
		})
	}
}

func TestConcurrentShimsAndHookHaveOneWinnerAndAdoptIt(t *testing.T) {
	for _, opponent := range []string{"shim", "hook"} {
		t.Run(opponent, func(t *testing.T) {
			for range 4 {
				f := newSuccessionBindingFixture(t, "claude", "SessionStart", "S", proc.Ancestor{})
				f.hub.clock.Advance(16 * time.Minute)
				first, second := f.resolver("claude", "S", proc.Ancestor{}), f.resolver("claude", "S", proc.Ancestor{})
				start := make(chan struct{})
				type result struct {
					bound Bound
					err   error
				}
				left, right := make(chan result, 1), make(chan result, 1)
				go func() {
					<-start
					b, err := first.Resolve(t.Context(), "")
					left <- result{b, err}
				}()
				go func() {
					<-start
					if opponent == "shim" {
						b, err := second.Resolve(t.Context(), "")
						right <- result{b, err}
						return
					}
					out := (hook.Runner{
						ClientName: "claude", EventName: "SessionStart", Stdin: strings.NewReader(`{"session_id":"S","cwd":` + quoteJSON(f.cwd) + `}`),
						Hub: &hubclient.Client{Config: config{f.hub.srv.URL}}, Getenv: successionEnvironment("claude", "S", f.token), Host: func() string { return "floor-host" },
						Ancestor: func() (proc.Ancestor, error) { return proc.Ancestor{}, nil },
						Find:     func(string) (gitinfo.Repo, error) { return gitinfo.Repo{}, gitinfo.ErrNotRepo },
					}).Run()
					right <- result{err: errors.New(string(out.Stderr))}
				}()
				close(start)
				a, b := <-left, <-right
				if a.err != nil {
					var lost *LostError
					if errors.As(a.err, &lost) {
						generation, execution, proofErr := f.sessions.PrepareSuccession("claude", "S", 0, "", proc.Ancestor{})
						t.Fatalf("first resolver did not adopt the winner: bound=%+v err=%v cause=%T %v; proof=%d/%q/%v rows=%+v", a.bound, a.err, lost.Cause, lost.Cause, generation, execution, proofErr, executionRows(t, f.hub.file))
					}
					t.Fatalf("first resolver did not adopt the winner: bound=%+v err=%v", a.bound, a.err)
				}
				if opponent == "shim" {
					if b.err != nil || a.bound.ExecutionID != b.bound.ExecutionID || a.bound.Generation != 2 || b.bound.Generation != 2 {
						t.Fatalf("shims did not adopt one winner: %+v %+v", a, b)
					}
				} else {
					var failures []hook.Failure
					var err error
					failures, err = hook.ReadRecentFailures(f.token, 10)
					if err != nil || len(failures) != 0 {
						t.Fatalf("hook failed in succession race: %v %v (stderr result %+v)", failures, err, b)
					}
				}
				rows := executionRows(t, f.hub.file)
				if len(rows) != 2 || rows[0].state != "lost" || rows[1].state != "active" || rows[1].generation != 2 {
					t.Fatalf("race split generations: %+v", rows)
				}
			}
		})
	}
}

func TestSuccessionRequiresLiveParentAndExcludesWatch(t *testing.T) {
	f := newSuccessionBindingFixture(t, "claude", "SessionStart", "S", proc.Ancestor{})
	f.hub.clock.Advance(16 * time.Minute)
	r := f.resolver("claude", "S", proc.Ancestor{})
	r.ParentAlive = func() bool { return false }
	if _, err := r.Resolve(t.Context(), ""); !IsCode(err, "lost") {
		t.Fatalf("dead parent did not fail closed: %v", err)
	}
	if len(f.hub.successions()) != 0 {
		t.Fatalf("dead parent attempted succession: %+v", f.hub.successions())
	}
	r = f.resolver("claude", "S", proc.Ancestor{})
	r.HeartbeatSource = "watch"
	if _, err := r.Resolve(t.Context(), ""); !IsCode(err, "lost") {
		t.Fatalf("watch attempted succession: %v", err)
	}
	if len(f.hub.successions()) != 0 {
		t.Fatalf("watch attempted succession: %+v", f.hub.successions())
	}
}

func TestSuccessionPrefersCurrentRung3SessionAfterClear(t *testing.T) {
	anc := proc.Ancestor{PID: 42, StartMS: 100, Name: "cursor-agent"}
	f := newSuccessionBindingFixture(t, "cursor", "sessionStart", "S1", anc)
	r := f.resolver("cursor", "", anc)
	before, err := r.Resolve(t.Context(), "")
	if err != nil || before.SessionID != "S1" {
		t.Fatalf("initial S1 bind: %+v %v", before, err)
	}
	// /clear starts S2 on the same process and replaces the local current-session witness.
	out := (hook.Runner{
		ClientName: "cursor", EventName: "sessionStart", Stdin: strings.NewReader(`{"session_id":"S2","cwd":` + quoteJSON(f.cwd) + `}`),
		Hub: &hubclient.Client{Config: config{f.hub.srv.URL}}, Getenv: successionEnvironment("cursor", "", f.token), Host: func() string { return "floor-host" },
		Ancestor: func() (proc.Ancestor, error) { return anc, nil },
		Find:     func(string) (gitinfo.Repo, error) { return gitinfo.Repo{}, gitinfo.ErrNotRepo },
	}).Run()
	if failures, err := hook.ReadRecentFailures(f.token, 10); err != nil || len(failures) != 0 {
		t.Fatalf("S2 SessionStart: %v %v, stdout=%s", failures, err, out.Stdout)
	}
	f.hub.clock.Advance(16 * time.Minute)
	after, err := r.Resolve(t.Context(), "")
	if err != nil || after.SessionID != "S2" || after.Generation != 2 || after.Rung != 3 {
		t.Fatalf("resolver did not follow S2 after /clear: %+v %v", after, err)
	}
	if requests := f.hub.successions(); len(requests) != 1 || requests[0]["session_id"] != "S2" {
		t.Fatalf("local /clear witness did not pick only S2: %+v", requests)
	}
	assertNoGenerationZeroRegistration(t, f.hub.sessions())
}
