package binding

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/mateusz-klatt/iris/internal/gitinfo"
	"github.com/mateusz-klatt/iris/internal/hook"
	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/proc"
	"github.com/mateusz-klatt/iris/internal/store"
)

func TestFreshRung3VetoesEndedGenerationFromRestoredBackup(t *testing.T) {
	anc := proc.Ancestor{PID: 42, StartMS: 100, Name: "cursor-agent"}
	f := newSuccessionBindingFixture(t, "cursor", "sessionStart", "S", anc)
	original := executionRows(t, f.hub.file)[0]
	backup := f.hub.backup()
	out := (hook.Runner{
		ClientName: "cursor", EventName: "sessionEnd", Stdin: strings.NewReader(`{"session_id":"S","cwd":` + quoteJSON(f.cwd) + `}`),
		Hub: &hubclient.Client{Config: config{f.hub.srv.URL}}, Getenv: successionEnvironment("cursor", "", f.token), Host: func() string { return "floor-host" },
		Ancestor: func() (proc.Ancestor, error) { return anc, nil },
		Find:     func(string) (gitinfo.Repo, error) { return gitinfo.Repo{}, gitinfo.ErrNotRepo },
	}).Run()
	if failures, err := hook.ReadRecentFailures(f.token, 10); err != nil || len(failures) != 0 {
		t.Fatalf("SessionEnd failures: %v %v, stderr=%s", failures, err, out.Stderr)
	}
	if proof, marker := sessionProof(t, f.sessions.Dir()); proof != "" || marker != "1" {
		t.Fatalf("SessionEnd proof=%q marker=%q", proof, marker)
	}
	if session, ok, err := f.sessions.CurrentSession("cursor", anc); session != "S" || !ok || err != nil {
		t.Fatalf("session witness disappeared: %q %t %v", session, ok, err)
	}

	f.hub.restore(backup)
	assertRung3RestoreStillRecovering(t, f, original)
	r := f.resolver("cursor", "", anc)
	if b, err := r.Resolve(t.Context(), ""); b != (Bound{}) || !errors.Is(err, errGenerationFloor) {
		t.Fatalf("fresh Rung 3 bound the ended Execution: %+v %v", b, err)
	}
	if writes, succession := nonHookSessionRequests(f.hub.sessions()), f.hub.successions(); len(writes) != 0 || len(succession) != 0 {
		t.Fatalf("the end marker authorized requests: registrations=%+v succession=%+v", writes, succession)
	}
	assertRung3RestoreStillRecovering(t, f, original)
}

func TestFreshRung3CannotReattachDivergentRestoredExecution(t *testing.T) {
	anc := proc.Ancestor{PID: 42, StartMS: 100, Name: "cursor-agent"}
	f := newSuccessionBindingFixture(t, "cursor", "sessionStart", "S", anc)
	original := executionRows(t, f.hub.file)[0]
	other := newSuccessionBindingFixture(t, "cursor", "sessionStart", "S", anc)
	divergent := executionRows(t, other.hub.file)[0]
	if original.id == divergent.id || original.generation != divergent.generation {
		t.Fatalf("fixture histories did not diverge at the same generation: %+v and %+v", original, divergent)
	}
	f.hub.restore(other.hub.backup())
	assertRung3RestoreStillRecovering(t, f, divergent)
	r := f.resolver("cursor", "", anc)
	if b, err := r.Resolve(t.Context(), ""); b != (Bound{}) || !IsCode(err, "lost") {
		t.Fatalf("fresh Rung 3 reattached the divergent Execution: %+v %v", b, err)
	}
	if writes := nonHookSessionRequests(f.hub.sessions()); len(writes) != 0 {
		t.Fatalf("a process witness authorized ordinary registration: %+v", writes)
	}
	if requests := f.hub.successions(); len(requests) != 1 || requests[0]["prior_execution_id"] != original.id || requests[0]["prior_generation"] != float64(original.generation) {
		t.Fatalf("fresh Rung 3 omitted exact predecessor proof: %+v", requests)
	}
	if generation, execution, err := f.sessions.PrepareSuccession("cursor", "S", 1, original.id, anc); generation != 1 || execution != original.id || err != nil {
		t.Fatalf("restore changed local proof: %d %q %v", generation, execution, err)
	}
	assertRung3RestoreStillRecovering(t, f, divergent)
}

func TestFreshRung3WithWitnessAndNoLiveProofRefusesFallback(t *testing.T) {
	anc := proc.Ancestor{PID: 42, StartMS: 100, Name: "cursor-agent"}
	f := newSuccessionBindingFixture(t, "cursor", "sessionStart", "S", anc)
	if err := os.Remove(f.generationPath(t)); err != nil {
		t.Fatal(err)
	}
	f.hub.restart()
	r := f.resolver("cursor", "", anc)
	if b, err := r.Resolve(t.Context(), ""); b != (Bound{}) || !errors.Is(err, errGenerationFloor) {
		t.Fatalf("process witness without proof allowed a binding: %+v %v", b, err)
	}
	if writes, succession := nonHookSessionRequests(f.hub.sessions()), f.hub.successions(); len(writes) != 0 || len(succession) != 0 {
		t.Fatalf("process witness without proof authorized requests: registrations=%+v succession=%+v", writes, succession)
	}
}

func assertRung3RestoreStillRecovering(t *testing.T, f successionBindingFixture, row executionRow) {
	t.Helper()
	if rows := executionRows(t, f.hub.file); len(rows) != 1 || rows[0].id != row.id || rows[0].generation != row.generation || rows[0].state != "active" {
		t.Fatalf("restored history changed: %+v", rows)
	}
	_, err := f.hub.store.Call(t.Context(), "validate", store.Request{ExecutionID: row.id, Generation: row.generation})
	var lost *store.Error
	if !errors.As(err, &lost) || lost.Code != "lost" {
		t.Fatalf("restored Execution was live within recovery: %v", err)
	}
}
