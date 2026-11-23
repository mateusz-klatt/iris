package binding

import (
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/proc"
)

func TestCodexRootHandleDisablesSuccessionButDoesNotSelectTheBinding(t *testing.T) {
	f := newSuccessionBindingFixture(t, "codex", "SessionStart", "S", proc.Ancestor{})
	r := f.resolver("codex", "", proc.Ancestor{})
	live, primary, err := r.ResolveCall(t.Context(), "ex_other_session", "S", "S")
	if err != nil || !primary || live.SessionID != "S" || live.Rung != 2 {
		t.Fatalf("explicit handle displaced the metadata binding: %+v %t %v", live, primary, err)
	}
	f.hub.clock.Advance(16 * time.Minute)
	if b, primary, err := r.ResolveCall(t.Context(), "ex_other_session", "S", "S"); b.ExecutionID != "" || !primary || !IsCode(err, "lost") {
		t.Fatalf("explicit handle allowed a successor: %+v %t %v", b, primary, err)
	}
	if got := f.hub.successions(); len(got) != 0 {
		t.Fatalf("explicit handle reached succession: %+v", got)
	}
	if b, primary, err := r.ResolveCall(t.Context(), "", "S", "S"); b.Generation != 2 || !primary || err != nil {
		t.Fatalf("metadata root without a handle did not succeed: %+v %t %v", b, primary, err)
	}
}
