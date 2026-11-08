package hook

import (
	"context"
	"errors"
	"net"
	neturl "net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/mateusz-klatt/iris/internal/gitinfo"
	"github.com/mateusz-klatt/iris/internal/hubclient"
)

// ReconcileDeadline bounds a shell reconcile against a hub on this machine, and
// RemoteReconcileDeadline one against a hub elsewhere. A reconcile makes several hub calls, and
// each hook is a new process that opens its own connection: across a network 300 ms went past
// routinely, leaving those commands' changes unclaimed (D3).
const (
	ReconcileDeadline       = 300 * time.Millisecond
	RemoteReconcileDeadline = time.Second
)

// shellBudget is the reconcile budget for a hub at url. Only a loopback address or localhost
// counts as this machine; a name that resolves to one is not looked up, so it gets the larger
// budget, which costs only time.
func shellBudget(url string) time.Duration {
	u, err := neturl.Parse(url)
	if err != nil || u.Hostname() == "" {
		return ReconcileDeadline
	}
	host := u.Hostname()
	if strings.EqualFold(host, "localhost") {
		return ReconcileDeadline
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return ReconcileDeadline
	}
	return RemoteReconcileDeadline
}

// shellLimit is the Runner's reconcile budget: ShellDeadline when set, else the configured hub's.
func (r Runner) shellLimit() time.Duration {
	if r.ShellDeadline > 0 {
		return r.ShellDeadline
	}
	if r.Getenv == nil {
		return ReconcileDeadline
	}
	return shellBudget(hubclient.EnvConfig{Getenv: r.Getenv}.HubURL())
}

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
	ctx, cancel := context.WithTimeout(ctx, r.shellLimit())
	defer cancel()
	// settle records only the first outcome. When the budget runs out, the git status or hub
	// call it interrupts fails too, and that failure is the budget's, not a broken hook's:
	// otherwise it could land first or overwrite the budget's entry, and doctor would call a
	// client silent for one slow git status.
	var settled atomic.Bool
	settle := func(err error, out Output) Output {
		if !settled.CompareAndSwap(false, true) {
			return out
		}
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			err = hookFailure(shellOverBudget)
		}
		return state.fail(err, out)
	}
	done := make(chan Output, 1)
	go func() {
		// Keep the same fail-open guarantee as the main hook, including dependency panics.
		defer func() {
			if recover() != nil {
				done <- settle(hookFailure("panic during shell reconciliation"), allow)
			}
		}()
		state.at("shell repository")
		repo, err := r.repoFor(ev)
		if errors.Is(err, gitinfo.ErrNotRepo) {
			done <- allow // outside a worktree the hook does nothing (D3, rule 5)
			return
		}
		if err != nil {
			done <- settle(err, allow)
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
			done <- settle(err, Encode(ev, Decision{Action: ActionWarn, Context: shellRecovering}))
			return
		}
		if err != nil {
			done <- settle(err, r.failOpen(state, ev, err, allow))
			return
		}
		state.at("shell status")
		status := r.Status
		if status == nil {
			status = gitinfo.Status
		}
		snapshot, err := status(ctx, repo.Worktree)
		if err != nil {
			done <- settle(err, allow)
			return
		}
		phase := "after"
		if ev.Kind == KindPreTool {
			phase = "before"
		}
		state.at("shell reconcile")
		out, err := hub.Reconcile(ctx, hubclient.ReconcileRequest{ExecutionID: sess.ExecutionID, Generation: sess.Generation, Kind: phase, Status: snapshot})
		if err != nil {
			done <- settle(err, r.failOpen(state, ev, err, allow))
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
		return settle(hookFailure(shellOverBudget), allow)
	}
}

// shellRefused is the denial of a shell call during recovery. Every command is refused, read-only
// ones too, because the hook cannot tell beforehand which ones change files.
const shellRefused = "Iris: the hub restarted and this session cannot prove it is the same one yet, so the command is refused rather than run unchecked. This lasts at most about two minutes, until the session reattaches; retry shortly."

// shellRecovering is the warning after a shell call that could not be checked during recovery.
const shellRecovering = "Iris: the hub restarted and this session cannot prove it is the same one yet, so the changes this command made were not checked or claimed. Other agents cannot see them until the session reattaches."
