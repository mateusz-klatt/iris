package hook

import (
	"context"
	"time"

	"github.com/mateusz-klatt/iris/internal/gitinfo"
	"github.com/mateusz-klatt/iris/internal/hubclient"
)

// Windows takes no reduced snapshot and keeps no breaker (ADR 018). When the deadline stops git
// there, what git started can outlive it (command_windows.go), and a rename, which the breaker
// file is replaced with, is not documented as atomic. Every shell reconcile there runs in full
// scope, as before, and nothing about a hub's capabilities is kept.
//
// fromGit says that err came from git status rather than the hub.
func (r Runner) shell(ctx context.Context, hub shellHub, ev Event, repo gitinfo.Repo, state *runState, run *shellRun) (out hubclient.CheckResponse, fromGit bool, err error) {
	return r.shellFull(ctx, hub, ev, repo, state, run, shellPhase(ev))
}

func (Runner) learnScope([]string) {}

func (Runner) fullSucceeded(*runState, gitinfo.Repo) {}

func (Runner) tripIfSlow(context.Context, context.Context, time.Duration, *runState, *shellRun) {}
