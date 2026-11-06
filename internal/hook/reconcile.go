package hook

import (
	"context"
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

func (r Runner) reconcile(ctx context.Context, ev Event) Output {
	allow := Encode(ev, Decision{Action: ActionAllow})
	hub, ok := r.Hub.(shellHub)
	if !ok {
		return allow
	}
	ctx, cancel := context.WithTimeout(ctx, ReconcileDeadline)
	defer cancel()
	done := make(chan Output, 1)
	go func() {
		// Keep the same fail-open guarantee as the main hook, including dependency panics.
		defer func() {
			if recover() != nil {
				done <- allow
			}
		}()
		repo, err := r.repoFor(ev)
		if err != nil {
			done <- allow
			return
		}
		sess, err := r.Hub.RegisterSession(ctx, r.sessionReq(ev, repo))
		if err != nil {
			done <- allow
			return
		}
		status := r.Status
		if status == nil {
			status = gitinfo.Status
		}
		snapshot, err := status(ctx, repo.Worktree)
		if err != nil {
			done <- allow
			return
		}
		phase := "after"
		if ev.Kind == KindPreTool {
			phase = "before"
		}
		out, err := hub.Reconcile(ctx, hubclient.ReconcileRequest{ExecutionID: sess.ExecutionID, Generation: sess.Generation, Kind: phase, Status: snapshot})
		if err != nil || out.Message == "" {
			done <- allow
			return
		}
		done <- Encode(ev, Decision{Action: ActionWarn, Context: out.Message})
	}()
	select {
	case out := <-done:
		return out
	case <-ctx.Done():
		return allow
	}
}
