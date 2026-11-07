package hook

import (
	"context"
	"errors"
	"time"

	"github.com/mateusz-klatt/iris/internal/gitinfo"
	"github.com/mateusz-klatt/iris/internal/hubclient"
)

const ReconcileDeadline = 300 * time.Millisecond

type shellHub interface {
	Reconcile(context.Context, hubclient.ReconcileRequest) (hubclient.CheckResponse, error)
}

func shellEvent(ev Event) bool {
	return ev.ToolClass == ClassShell && (ev.Kind == KindPreTool || ev.Kind == KindPostTool || ev.Kind == KindPostToolFailure)
}

// reconcile checks what a shell or Code Mode call changed. It registers as an edit does, with the
// session's remembered generation, so it reattaches a recovering session with proof (D9, #55),
// and every way it fails open is recorded for doctor (D3, rule 6). Gap review A17.
func (r Runner) reconcile(ctx context.Context, ev Event, state *runState) Output {
	allow := Encode(ev, Decision{Action: ActionAllow})
	hub, ok := r.Hub.(shellHub)
	if !ok {
		return allow
	}
	limit := r.ShellDeadline
	if limit <= 0 {
		limit = ReconcileDeadline
	}
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	done := make(chan Output, 1)
	go func() {
		// Keep the same fail-open guarantee as the main hook, including dependency panics.
		defer func() {
			if recover() != nil {
				done <- state.fail(hookFailure("panic during shell reconciliation"), allow)
			}
		}()
		state.at("shell repository")
		repo, err := r.repoFor(ev)
		if errors.Is(err, gitinfo.ErrNotRepo) {
			done <- allow // outside a worktree the hook does nothing (D3, rule 5)
			return
		}
		if err != nil {
			done <- state.fail(err, allow)
			return
		}
		state.at("shell session")
		sess, err := r.register(ctx, ev, repo)
		if hubclient.IsCode(err, "lost") {
			// A recovering session without its proof must not run unclaimed changes past it, as
			// an edit may not. After the call, only a warning is possible.
			if ev.Kind == KindPreTool {
				done <- Encode(ev, Decision{Action: ActionDeny, Reason: shellRefused})
				return
			}
			done <- state.fail(err, Encode(ev, Decision{Action: ActionWarn, Context: shellRecovering}))
			return
		}
		if err != nil {
			done <- state.fail(err, r.failOpen(ev, err, allow))
			return
		}
		state.at("shell status")
		status := r.Status
		if status == nil {
			status = gitinfo.Status
		}
		snapshot, err := status(ctx, repo.Worktree)
		if err != nil {
			done <- state.fail(err, allow)
			return
		}
		phase := "after"
		if ev.Kind == KindPreTool {
			phase = "before"
		}
		state.at("shell reconcile")
		out, err := hub.Reconcile(ctx, hubclient.ReconcileRequest{ExecutionID: sess.ExecutionID, Generation: sess.Generation, Kind: phase, Status: snapshot})
		if err != nil {
			done <- state.fail(err, r.failOpen(ev, err, allow))
			return
		}
		if out.Message == "" {
			done <- allow
			return
		}
		done <- Encode(ev, Decision{Action: ActionWarn, Context: out.Message})
	}()
	select {
	case out := <-done:
		return out
	case <-ctx.Done():
		return state.fail(hookFailure(shellOverBudget), allow)
	}
}

// shellRefused is the denial of a shell call during recovery. Every command is refused, read-only
// ones too, because the hook cannot tell beforehand which ones change files.
const shellRefused = "Iris: the hub restarted and this session cannot prove it is the same one yet, so the command is refused rather than run unchecked. This lasts at most about two minutes, until the session reattaches; retry shortly."

// shellRecovering is the warning after a shell call that could not be checked during recovery.
const shellRecovering = "Iris: the hub restarted and this session cannot prove it is the same one yet, so the changes this command made were not checked or claimed. Other agents cannot see them until the session reattaches."
