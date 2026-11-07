package hook

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/gitinfo"
	"github.com/mateusz-klatt/iris/internal/hubclient"
)

// recoveryShellHub is the hub of D9 for shell reconciliation: it refuses a recovering session
// that brings no generation, and counts the reconciliations it takes.
type recoveryShellHub struct {
	*recoveryHub
	reconciled *int
}

func (h recoveryShellHub) Reconcile(context.Context, hubclient.ReconcileRequest) (hubclient.CheckResponse, error) {
	*h.reconciled++
	return hubclient.CheckResponse{}, nil
}

// Shell reconciliation reattaches a recovering session with its remembered generation, as an
// edit does (#55). Without that proof it refuses the command beforehand and warns afterwards,
// rather than letting changes pass unchecked and unclaimed (gap review A17).
func TestShellReconciliationCarriesTheRecoveryProof(t *testing.T) {
	root := gitRepo(t)
	newHub := func() (recoveryShellHub, *int) {
		n := 0
		return recoveryShellHub{recoveryHub: &recoveryHub{generation: 3, recovering: true}, reconciled: &n}, &n
	}
	t.Run("with proof", func(t *testing.T) {
		hub, reconciled := newHub()
		r, ev := shellRunner(hub, root)
		m := fileMemory{dir: t.TempDir(), now: time.Now, restrict: func(string) error { return nil }}
		m.Remember(string(ev.Client), ev.SessionID, 3)
		r.Memory = m
		if out := r.reconcile(context.Background(), ev, &runState{}); !isAllow(out) || *reconciled != 1 {
			t.Fatalf("reconciled %d: %s", *reconciled, out.Stdout)
		}
	})
	t.Run("without proof, before", func(t *testing.T) {
		hub, reconciled := newHub()
		r, ev := shellRunner(hub, root)
		out := r.reconcile(context.Background(), ev, &runState{})
		if !strings.Contains(string(out.Stdout), "at most about two minutes") || !strings.Contains(string(out.Stdout), "deny") || *reconciled != 0 {
			t.Fatalf("reconciled %d: %s", *reconciled, out.Stdout)
		}
	})
	t.Run("without proof, after", func(t *testing.T) {
		hub, reconciled := newHub()
		r, ev := shellRunner(hub, root)
		ev.Kind = KindPostTool
		state := &runState{}
		out := r.reconcile(context.Background(), ev, state)
		stage, err := state.snapshot()
		if !strings.Contains(string(out.Stdout), "were not checked or claimed") || *reconciled != 0 || stage != "shell session" || !hubclient.IsCode(err, "lost") {
			t.Fatalf("stage %q err %v reconciled %d: %s", stage, err, *reconciled, out.Stdout)
		}
	})
}

// Every way reconciliation fails open is recorded, at its own stage, for doctor (D3, rule 6).
func TestShellReconciliationRecordsEveryFailOpen(t *testing.T) {
	root := gitRepo(t)
	cases := []struct {
		name, stage string
		setup       func(r *Runner)
		hub         Hub
	}{
		{"repository", "shell repository", func(r *Runner) {
			r.Find = func(string) (gitinfo.Repo, error) { return gitinfo.Repo{}, errors.New("unreadable") }
		}, scriptHub{}},
		{"panic", "shell repository", func(r *Runner) { r.Find = func(string) (gitinfo.Repo, error) { panic("dependency") } }, scriptHub{}},
		{"hub", "shell session", func(*Runner) {}, scriptHub{regErr: errors.New("down")}},
		{"git", "shell status", func(r *Runner) {
			r.Status = func(context.Context, string) (map[string]string, error) { return nil, errors.New("git") }
		}, scriptHub{}},
		{"reconcile", "shell reconcile", func(*Runner) {}, scriptHub{reconcileErr: errors.New("refused")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, ev := shellRunner(tc.hub, root)
			tc.setup(&r)
			state := &runState{}
			out := r.reconcile(context.Background(), ev, state)
			stage, err := state.snapshot()
			if !isAllow(out) || err == nil || stage != tc.stage {
				t.Fatalf("stage %q err %v: %s", stage, err, out.Stdout)
			}
		})
	}
	t.Run("deadline", func(t *testing.T) {
		r, ev := shellRunner(scriptHub{}, root)
		r.ShellDeadline = time.Millisecond
		r.Status = func(ctx context.Context, _ string) (map[string]string, error) {
			<-ctx.Done()
			time.Sleep(10 * time.Millisecond)
			return nil, ctx.Err()
		}
		state := &runState{}
		out := r.reconcile(context.Background(), ev, state)
		// Only running past the budget is degraded, which doctor reports without calling the
		// client silent; the same stage failing any other way is not.
		stage, err := state.snapshot()
		if !isAllow(out) || !(Failure{Stage: stage, Error: failureText(err)}).Degraded() {
			t.Fatalf("stage %q err %v: %s", stage, err, out.Stdout)
		}
		if (Failure{Stage: stage, Error: "dependency failure (*errors.errorString)"}).Degraded() {
			t.Fatal("a failed git status was graded as only slow")
		}
	})
	t.Run("rejected token", func(t *testing.T) {
		r, ev := shellRunner(scriptHub{regErr: &hubclient.Error{Code: "unauthorized", Message: "revoked"}}, root)
		state := &runState{}
		out := r.reconcile(context.Background(), ev, state)
		if _, err := state.snapshot(); err == nil || !strings.Contains(string(out.Stdout), "rejected this host's token") {
			t.Fatalf("err %v: %s", err, out.Stdout)
		}
	})
	t.Run("outside a worktree", func(t *testing.T) {
		r, ev := shellRunner(scriptHub{}, t.TempDir())
		state := &runState{}
		out := r.reconcile(context.Background(), ev, state)
		if _, err := state.snapshot(); !isAllow(out) || err != nil {
			t.Fatalf("outside a worktree is no failure: %v", err)
		}
	})
}
