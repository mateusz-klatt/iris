package hook

import (
	"sync"

	"github.com/mateusz-klatt/iris/internal/gitinfo"
)

// shellRun is what the reconcile's worker shares with the hook while it runs: the worktree,
// whether the status is a full one, and that status's Attempt. The hook reads them when the
// deadline passes, as the worker may still be waiting for git. Only a system that keeps a
// breaker reads them (ADR 018).
type shellRun struct {
	mu      sync.Mutex
	repo    gitinfo.Repo
	full    bool
	attempt gitinfo.Attempt
}

func (s *shellRun) setRepo(repo gitinfo.Repo) { s.mu.Lock(); s.repo = repo; s.mu.Unlock() }
func (s *shellRun) setFull()                  { s.mu.Lock(); s.full = true; s.mu.Unlock() }

// shellScopeUnknown is what a reduced reconcile records when it sends nothing because the index
// changed between its reads, or it or its gitlinks could not be read whole. Like a status past
// its budget, it only skips that one reconciliation; once the budget has passed, settle records
// shellOverBudget instead. The text names only the index and stays as written, because saved
// failures in hook-failures.log are graded by this exact text.
const shellScopeUnknown = "reduced snapshot not taken: the index could not be read as one generation"
