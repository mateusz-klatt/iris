package hook

import (
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/mateusz-klatt/iris/internal/config"
	"github.com/mateusz-klatt/iris/internal/gitinfo"
	"github.com/mateusz-klatt/iris/internal/proc"
)

func TestKimiClearDuringHubOutageWithdrawsOldSuccessionWitness(t *testing.T) {
	h := newKimiHarness(t)
	t.Cleanup(config.SetPinnedForTest(h.pinned))
	h.hub = &successionHookHub{}
	h.start(kimiSession)
	sessions := NewSessions(h.env("IRIS_TOKEN_FILE"))
	if generation, id, err := sessions.PrepareSuccession("kimi", kimiSession, 1, "ex_one", h.anc); generation != 1 || id != "ex_one" || err != nil {
		t.Fatalf("first session has no succession proof: generation=%d id=%q err=%v", generation, id, err)
	}
	var current string
	var present bool
	var witnessErr error
	h.hub = countingHub{fn: func() {
		current, present, witnessErr = sessions.CurrentSession("kimi", h.anc)
	}}
	if out := h.start(kimiCleared); out.Exit != 0 {
		t.Fatalf("failed SessionStart blocked Kimi: %+v", out)
	}
	if current != kimiCleared || !present || witnessErr != nil {
		t.Fatalf("registration still saw the old witness: current=%q present=%t err=%v", current, present, witnessErr)
	}
	if _, _, err := sessions.PrepareSuccession("kimi", kimiSession, 1, "ex_one", h.anc); !errors.Is(err, ErrSuccessionProof) {
		t.Fatalf("old session remained eligible during the outage: %v", err)
	}
	if got := h.mapped(); got != kimiSession {
		t.Fatalf("Kimi's wake map advanced without hub acceptance: %q", got)
	}
}

func TestCursorRepositoryFailurePublishesNewProcessWitness(t *testing.T) {
	m := newFileMemory(t)
	anc := proc.Ancestor{PID: 42, StartMS: 100, Name: "cursor-agent"}
	if err := m.rememberProcessSession("cursor", "S1", anc); err != nil {
		t.Fatal(err)
	}
	path := m.name("cursor", "S1")
	if err := m.write(path, "1"); err != nil {
		t.Fatal(err)
	}
	if err := m.write(path+".execution", "1 ex_one"); err != nil {
		t.Fatal(err)
	}
	out := (Runner{
		ClientName: "cursor", EventName: "sessionStart", Stdin: strings.NewReader(`{"session_id":"S2","cwd":"/repo"}`),
		Hub: scriptHub{}, Memory: m, Host: func() string { return "host" },
		Ancestor: func() (proc.Ancestor, error) { return anc, nil },
		Find:     func(string) (gitinfo.Repo, error) { return gitinfo.Repo{}, errors.New("unreadable git metadata") },
		after:    never, logAfter: never,
	}).Run()
	if out.Exit != 0 {
		t.Fatalf("repository failure blocked SessionStart: %+v", out)
	}
	sessions := &Sessions{memory: m}
	if current, ok, err := sessions.CurrentSession("cursor", anc); current != "S2" || !ok || err != nil {
		t.Fatalf("new witness was not published before repository discovery: current=%q present=%t err=%v", current, ok, err)
	}
	if _, _, err := sessions.PrepareSuccession("cursor", "S1", 1, "ex_one", anc); !errors.Is(err, ErrSuccessionProof) {
		t.Fatalf("old session remained eligible after repository discovery failed: %v", err)
	}
}

func TestFailedWitnessReplaceWithdrawsOldProofBeforeRegistration(t *testing.T) {
	h := newKimiHarness(t)
	h.hub = &successionHookHub{}
	h.start(kimiSession)
	sessions := NewSessions(h.env("IRIS_TOKEN_FILE"))
	memory := sessions.memory
	var failed bool
	protect := memory.restrict
	memory.restrict = func(path string) error {
		if strings.HasPrefix(filepath.Base(path), ".tmp-") && !failed {
			failed = true
			return errors.New("could not protect the temporary witness")
		}
		return protect(path)
	}
	var current string
	var witnessPresent bool
	var witnessErr error
	h.hub = countingHub{fn: func() {
		current, witnessPresent, witnessErr = sessions.CurrentSession("kimi", h.anc)
	}}
	r := h.runner("SessionStart", kimiCleared, "")
	r.Memory = memory
	if out := r.Run(); out.Exit != 0 || !failed {
		t.Fatalf("publication failure did not allow the hook: %+v", out)
	}
	if current != processWitnessWithdrawn || !witnessPresent || witnessErr != nil {
		t.Fatalf("registration did not see the withdrawal marker: current=%q present=%t err=%v", current, witnessPresent, witnessErr)
	}
	failures, err := ReadRecentFailures(h.env("IRIS_TOKEN_FILE"), 10)
	if err != nil || !slices.ContainsFunc(failures, func(f Failure) bool { return f.Stage == stageSessionFiles }) {
		t.Fatalf("witness failure was not recorded: %+v %v", failures, err)
	}
}

func TestHeldSessionsLockWithdrawsOldProcessWitness(t *testing.T) {
	sessions := successionSessionFiles(t, "cursor", "S1", 1, "ex_one")
	ancestor := proc.Ancestor{PID: 42, StartMS: 100, Name: "cursor-agent"}
	if err := sessions.memory.rememberProcessSession("cursor", "S1", ancestor); err != nil {
		t.Fatal(err)
	}
	unlock, err := sessions.Lock(hookLockBudget)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(unlock)
	if err := sessions.memory.rememberProcessSession("cursor", "S2", ancestor); !errors.Is(err, ErrSessionsLocked) {
		t.Fatalf("the publication did not observe the held lock: %v", err)
	}
	if current, ok, err := sessions.CurrentSession("cursor", ancestor); current != processWitnessWithdrawn || !ok || err != nil {
		t.Fatalf("the publication did not leave a withdrawal marker: current=%q present=%t err=%v", current, ok, err)
	}
}
