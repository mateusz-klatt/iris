package binding

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/mateusz-klatt/iris/internal/proc"
)

func TestLegacyHookWithoutWitnessBindsAndRenews(t *testing.T) {
	anc := proc.Ancestor{PID: 42, StartMS: 100, Name: "cursor-agent"}
	f := newSuccessionBindingFixture(t, "cursor", "sessionStart", "S", anc)
	witness := filepath.Join(f.sessions.Dir(), fmt.Sprintf("proc-%d-%d", anc.PID, anc.StartMS))
	if err := os.Remove(witness); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(f.generationPath(t) + ".execution"); err != nil {
		t.Fatal(err)
	}
	r := f.resolver("cursor", "", anc)
	first := executionRows(t, f.hub.file)[0].id
	b, err := r.Resolve(t.Context(), "")
	if err != nil || b.ExecutionID != first || b.Rung != 3 {
		t.Fatalf("0.1.14 session did not bind without a witness: bound=%+v err=%v", b, err)
	}
	if err := r.Heartbeat(t.Context(), "shim"); err != nil {
		t.Fatalf("0.1.14 session was not renewed: %v", err)
	}
	again, err := r.Resolve(t.Context(), "")
	if err != nil || again.ExecutionID != first {
		t.Fatalf("0.1.14 session did not refresh: bound=%+v err=%v", again, err)
	}
	if requests := f.hub.successions(); len(requests) != 0 {
		t.Fatalf("healthy legacy binding attempted succession: %+v", requests)
	}
}

func TestDamagedWitnessDoesNotBreakExistingReattachment(t *testing.T) {
	anc := proc.Ancestor{PID: 42, StartMS: 100, Name: "cursor-agent"}
	f := newSuccessionBindingFixture(t, "cursor", "sessionStart", "S", anc)
	r := f.resolver("cursor", "", anc)
	first, err := r.Resolve(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	f.hub.restart()
	if err := os.WriteFile(filepath.Join(f.sessions.Dir(), fmt.Sprintf("proc-%d-%d", anc.PID, anc.StartMS)), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	r.bound = Bound{}
	got, err := r.Resolve(t.Context(), "")
	if err != nil || got.ExecutionID != first.ExecutionID || got.Generation != 1 {
		t.Fatalf("damaged legacy evidence blocked existing reattachment: bound=%+v err=%v", got, err)
	}
	if err := r.Heartbeat(t.Context(), "shim"); err != nil {
		t.Fatalf("damaged legacy evidence blocked renewal: %v", err)
	}
}
