package gitinfo

import (
	"context"
	"errors"
	"os/exec"
	"sync"
)

// Stopper stops the gits started under the context it is in, from the caller's goroutine. A
// deadline alone leaves the kill to a goroutine of os/exec's, and a hook that returns at its
// deadline can exit before that goroutine runs: git then goes on after the hook is gone. Under
// load on WSL, the git stand-in was still sleeping three seconds later in 6 of 100 hook runs (ADR
// 018).
type Stopper struct {
	mu      sync.Mutex
	stopped bool
	running map[*exec.Cmd]bool
}

type stopperKey struct{}

// errStopped says that git was not started because its Stopper had already stopped.
var errStopped = errors.New("gitinfo: stopped before git started")

// WithStopper returns ctx with a Stopper for the gits started under it.
func WithStopper(ctx context.Context) (context.Context, *Stopper) {
	s := &Stopper{running: map[*exec.Cmd]bool{}}
	return context.WithValue(ctx, stopperKey{}, s), s
}

// Stop sends every git still running under its context the signal its deadline sends, before it
// returns, and no git starts under that context afterwards. A start already under way finishes
// first, so Stop waits for fork and exec, never for git to exit.
func (s *Stopper) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopped = true
	for cmd := range s.running {
		// os/exec may cancel it too; start made its Cancel signal only once.
		_ = cmd.Cancel()
	}
}

// start starts cmd under ctx's Stopper, if it has one. After Wait returns, finish removes cmd from
// the Stopper.
//
// Stop and os/exec may each cancel cmd, so cmd's Cancel is made to signal once, whichever calls
// first: a second signal, sent after os/exec's had killed git and Wait had reaped it, could reach
// another process group that took the same number.
func start(ctx context.Context, cmd *exec.Cmd) (finish func(), err error) {
	s, ok := ctx.Value(stopperKey{}).(*Stopper)
	if !ok {
		return func() {}, cmd.Start()
	}
	cancel, once, cancelErr := cmd.Cancel, sync.Once{}, error(nil)
	cmd.Cancel = func() error {
		once.Do(func() { cancelErr = cancel() })
		return cancelErr
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return nil, errStopped
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	s.running[cmd] = true
	return func() {
		s.mu.Lock()
		delete(s.running, cmd)
		s.mu.Unlock()
	}, nil
}

// run is cmd.Run under ctx's Stopper.
func run(ctx context.Context, cmd *exec.Cmd) error {
	finish, err := start(ctx, cmd)
	if err != nil {
		return err
	}
	defer finish()
	return cmd.Wait()
}
