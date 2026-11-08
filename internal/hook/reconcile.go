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

// sessionShellHub can register the session and reconcile in one call, one round trip instead
// of two. The hub client always can; a hub from before it answers with an error.
type sessionShellHub interface {
	ReconcileSession(context.Context, hubclient.ShellSessionRequest) (hubclient.ShellResponse, error)
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
	// The first stage is set before the goroutine starts. On a loaded machine the budget can run
	// out before the goroutine first runs, and a budget spent at no stage would read as a broken
	// hook instead of a slow one.
	state.at("shell repository")
	go func() {
		// Keep the same fail-open guarantee as the main hook, including dependency panics.
		defer func() {
			if recover() != nil {
				done <- settle(failPanicShell, allow)
			}
		}()
		repo, err := r.repoFor(ev)
		if errors.Is(err, gitinfo.ErrNotRepo) {
			done <- allow // outside a worktree the hook does nothing (D3, rule 5)
			return
		}
		if err != nil {
			done <- settle(err, allow)
			return
		}
		out, fromGit, err := r.shell(ctx, hub, ev, repo, state)
		if fromGit {
			// A failed git status fails open without the hub's context.
			done <- settle(err, allow)
			return
		}
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

// shell reconciles a shell call with the hub. One that can takes the snapshot and makes one
// call, which registers the session with its remembered generation and reconciles. Otherwise,
// and when that call is refused as an older hub would refuse it, the session registers as an
// edit's does, proof and its retry included, and reconciles in a second call.
// fromGit says that err came from git status rather than the hub.
func (r Runner) shell(ctx context.Context, hub shellHub, ev Event, repo gitinfo.Repo, state *runState) (out hubclient.CheckResponse, fromGit bool, err error) {
	phase := "after"
	if ev.Kind == KindPreTool {
		phase = "before"
	}
	status := r.Status
	if status == nil {
		status = gitinfo.Status
	}
	var taken map[string]string
	if combined, ok := hub.(sessionShellHub); ok {
		state.at("shell status")
		if taken, err = status(ctx, repo.Worktree); err != nil {
			return out, true, err
		}
		state.at("shell session")
		out, err = r.reconcileSession(ctx, combined, ev, repo, phase, taken)
		// A lost answer may also be an older hub's to a request with no Execution, so the
		// separate calls decide; they handle a real loss as an edit does.
		if err == nil || (!hubclient.IsCode(err, "lost") && !hubclient.IsCode(err, "bad_request") && !hubclient.IsCode(err, "not_found")) {
			return out, false, err
		}
	}
	state.at("shell session")
	sess, err := r.register(ctx, ev, repo)
	if err != nil {
		return out, false, err
	}
	if taken == nil {
		state.at("shell status")
		if taken, err = status(ctx, repo.Worktree); err != nil {
			return out, true, err
		}
	}
	state.at("shell reconcile")
	out, err = hub.Reconcile(ctx, hubclient.ReconcileRequest{ExecutionID: sess.ExecutionID, Generation: sess.Generation, Kind: phase, Status: taken})
	return out, false, err
}

// reconcileSession is the one-call form. The generation the hub answers with is remembered as
// the session's next proof, as a registration's is.
func (r Runner) reconcileSession(ctx context.Context, hub sessionShellHub, ev Event, repo gitinfo.Repo, phase string, snapshot map[string]string) (hubclient.CheckResponse, error) {
	req := hubclient.ShellSessionRequest{SessionRequest: r.sessionReq(ev, repo), Kind: phase, Status: snapshot}
	m := r.memory()
	if m != nil {
		req.Generation = m.Recall(string(ev.Client), ev.SessionID)
	}
	res, err := hub.ReconcileSession(ctx, req)
	if err == nil && m != nil && res.Session.Generation > 0 {
		m.Remember(string(ev.Client), ev.SessionID, res.Session.Generation)
	}
	return res.CheckResponse, err
}

// shellRefused is the denial of a shell call during recovery. Every command is refused, read-only
// ones too, because the hook cannot tell beforehand which ones change files.
const shellRefused = "Iris: the hub restarted and this session cannot prove it is the same one yet, so the command is refused rather than run unchecked. This lasts at most about two minutes, until the session reattaches; retry shortly."

// shellRecovering is the warning after a shell call that could not be checked during recovery.
const shellRecovering = "Iris: the hub restarted and this session cannot prove it is the same one yet, so the changes this command made were not checked or claimed. Other agents cannot see them until the session reattaches."
