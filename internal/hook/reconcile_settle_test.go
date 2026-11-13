package hook

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/gitinfo"
)

// A shell reconcile returns only once its first outcome is recorded. Run reads the state as soon
// as reconcile returns, so a later outcome that returned at once, such as the budget's after the
// worker's, could leave it empty and lose doctor's over-budget entry. The seam holds the worker's
// outcome after it has taken the slot and before it is recorded, the window a preemption leaves
// open, while the budget runs out and the reconcile settles too.
func TestAReconcileReturnsOnlyOnceItsFirstOutcomeIsRecorded(t *testing.T) {
	hub := newOneCallHub(errors.New("connection refused"))
	r, ev := shellRunner(hub, "/repo")
	r.Find = func(string) (gitinfo.Repo, error) { return gitinfo.Repo{Worktree: ev.CWD}, nil }
	r.Memory = newFileMemory(t)
	var budget context.Context
	r.shellTimeout = func(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
		ctx, end := handTimeout(parent, d)
		budget = ctx
		return ctx, end
	}
	held, release, waiting := make(chan struct{}), make(chan struct{}), make(chan struct{})
	r.settling = func(first bool) {
		if first {
			close(held)
			<-release
			return
		}
		close(waiting)
	}
	state := &runState{}
	returned := make(chan Output, 1)
	go func() { returned <- r.reconcile(context.Background(), ev, state) }()
	<-held
	passDeadline(budget)
	select {
	case <-waiting:
		close(release)
	case out := <-returned:
		_, err := state.snapshot()
		close(release)
		t.Fatalf("reconcile returned before its first outcome was recorded, which read %v: %s", err, out.Stdout)
	}
	out := <-returned
	stage, err := state.snapshot()
	if !isAllow(out) || stage != "shell session" || err != hookFailure(shellOverBudget) {
		t.Fatalf("stage %q err %v: %s", stage, err, out.Stdout)
	}
}
