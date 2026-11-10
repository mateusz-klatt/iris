//go:build !windows

package hook

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/mateusz-klatt/iris/internal/gitinfo"
	"github.com/mateusz-klatt/iris/internal/hubclient"
)

// A worktree's breaker (ADR 018 decision 4) is a hint for which status a shell reconcile runs:
// full, or reduced, which leaves tracked edits inside submodules out. It trips when a full
// status that started with at least half the budget is still running at the reconcile's own
// deadline. The worktree then runs reduced for a cooldown, doubled after each failed full
// attempt up to breakerCap, and a hook after the cooldown tries full again.
//
// It is a scheduling hint, not a ledger. Hooks that run at once read and replace it in no
// particular order, so two failures can count as one doubling and a late writer can replace a
// newer result. Neither changes what a snapshot says, because each states the scope it ran with.
type breaker struct {
	V            int    `json:"v"`
	Worktree     string `json:"worktree"`
	Scope        string `json:"scope"`
	Reason       string `json:"reason"`
	TrippedMS    int64  `json:"tripped_ms"`
	NextFullMS   int64  `json:"next_full_ms"`
	CooldownMS   int64  `json:"cooldown_ms"`
	LastFullOKMS int64  `json:"last_full_ok_ms"`
}

const (
	scopeFull    = "full"
	scopeReduced = "reduced"
	breakerFirst = 60 * time.Second
	breakerCap   = 10 * time.Minute
)

// scopeMemory keeps what a reduced reconcile needs between hook processes: whether a hub takes
// reduced snapshots, and each worktree's breaker. fileMemory has it. Without it a hook runs
// full scope, as before.
type scopeMemory interface {
	Capable(hub string) bool
	SetCapable(hub string, capable bool)
	Breaker(worktree string) (breaker, bool)
	SetBreaker(worktree string, b breaker)
}

// capability and breakerFile are the files beside the session entries, named by a digest of
// the hub URL or the worktree so that neither appears in a file name.
func (m fileMemory) capability(hub string) string {
	return filepath.Join(m.dir, "capability-"+digest(hub))
}
func (m fileMemory) breakerFile(worktree string) string {
	return filepath.Join(m.dir, "breaker-"+digest(worktree))
}

func digest(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:16])
}

func (m fileMemory) Capable(hub string) bool { return m.read(m.capability(hub)) == "shell_scope" }

// SetCapable is best effort: a lost write costs one registration before the next reduced
// snapshot, or one refused request after a rollback.
func (m fileMemory) SetCapable(hub string, capable bool) {
	if !capable {
		_ = os.Remove(m.capability(hub))
		return
	}
	m.write(m.capability(hub), "shell_scope")
}

// Breaker reads a worktree's breaker. A file that is missing, unreadable or not this version's
// is no breaker: the worktree runs full, as before.
func (m fileMemory) Breaker(worktree string) (breaker, bool) {
	return readBreaker(m.breakerFile(worktree))
}

func readBreaker(path string) (breaker, bool) {
	raw, err := os.ReadFile(path)
	var b breaker
	if err != nil || json.Unmarshal(raw, &b) != nil || b.V != 1 || (b.Scope != scopeFull && b.Scope != scopeReduced) {
		return breaker{}, false
	}
	return b, true
}

// reducedAt is whether a hook starting at now runs reduced by this breaker: it says reduced and
// its cooldown has not passed. A next attempt more than breakerCap away can only come from a
// clock that moved, and is not trusted.
func (b breaker) reducedAt(now time.Time) bool {
	ms := now.UnixMilli()
	return b.Scope == scopeReduced && ms < b.NextFullMS && b.NextFullMS <= ms+breakerCap.Milliseconds()
}

// ScopeHint is one worktree's breaker, as doctor reports it (ADR 018 decision 6).
type ScopeHint struct {
	Worktree string
	// Reduced says the breaker tripped and no full status has finished since. While Due, it
	// suggests reduced scope; once the cooldown is over, the next hook tries full.
	Reduced, Due bool
	Reason       string
	// NextFull is when a reduced worktree's next hook tries full again; zero when it is full.
	NextFull time.Time
	// LastFullOK is when a full status that ended a trip finished; zero when none has. It is
	// written only then, so later full statuses do not move it.
	LastFullOK time.Time
}

// ScopeHints reads every worktree's breaker in the session memory beside the token file at
// tokenPath, sorted by worktree. A file a hook would not read is left out. A hook takes the
// suggestion only where its hub takes reduced snapshots, nothing in its environment points git
// elsewhere and the index exists, which only that hook can see.
func ScopeHints(tokenPath string, now time.Time) []ScopeHint {
	dir := filepath.Join(filepath.Dir(tokenPath), "sessions")
	entries, _ := os.ReadDir(dir) // a missing directory has no breakers
	var hints []ScopeHint
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "breaker-") {
			continue
		}
		b, ok := readBreaker(filepath.Join(dir, e.Name()))
		if !ok {
			continue
		}
		hint := ScopeHint{Worktree: b.Worktree, Reduced: b.Scope == scopeReduced, Due: b.reducedAt(now), Reason: b.Reason}
		if hint.Reduced {
			hint.NextFull = time.UnixMilli(b.NextFullMS).UTC()
		}
		if b.LastFullOKMS > 0 {
			hint.LastFullOK = time.UnixMilli(b.LastFullOKMS).UTC()
		}
		hints = append(hints, hint)
	}
	sort.Slice(hints, func(i, j int) bool { return hints[i].Worktree < hints[j].Worktree })
	return hints
}

// SetBreaker replaces the breaker whole, through a temporary file and a rename, never in place.
func (m fileMemory) SetBreaker(worktree string, b breaker) {
	raw, _ := json.Marshal(b) // strings and integers always encode
	m.write(m.breakerFile(worktree), string(raw))
}

func (s *shellRun) read() (gitinfo.Repo, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.repo, s.full
}

// worktreeKey names a worktree for its breaker, by device and inode where they could be read.
func worktreeKey(repo gitinfo.Repo) string {
	if repo.WorktreeID != "" {
		return repo.WorktreeID
	}
	return repo.Worktree
}

func (r Runner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r Runner) getenv(key string) string {
	if r.Getenv == nil {
		return ""
	}
	return r.Getenv(key)
}

func (r Runner) hubURL() string {
	return hubclient.EnvConfig{Getenv: r.getenv}.HubURL()
}

func (r Runner) scopeMemory() (scopeMemory, bool) {
	m, ok := r.memory().(scopeMemory)
	return m, ok
}

// learnScope keeps what a registration answer says about reduced snapshots, for this hub.
func (r Runner) learnScope(capabilities []string) {
	if m, ok := r.scopeMemory(); ok {
		m.SetCapable(r.hubURL(), hubclient.ShellScopeCapable(capabilities))
	}
}

// gitOverride is whether the hook's environment points git at another repository or index than
// the one gitinfo found. Then the reads of a reduced snapshot would not be of that index, so the
// reconcile stays full.
func (r Runner) gitOverride() bool {
	for _, key := range []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_COMMON_DIR", "GIT_INDEX_FILE"} {
		if r.getenv(key) != "" {
			return true
		}
	}
	return false
}

// reducedDue is whether this reconcile runs reduced: nothing overrides git's repository, the
// index exists, and the worktree's breaker says reduced at this moment.
func (r Runner) reducedDue(repo gitinfo.Repo) bool {
	m, ok := r.scopeMemory()
	if !ok || r.gitOverride() || repo.GitDir == "" {
		return false
	}
	// Without an index file there is no generation to read; git would take it as empty, and
	// the reduced snapshot does not rest on that.
	if _, err := os.Stat(filepath.Join(repo.GitDir, "index")); err != nil {
		return false
	}
	b, ok := m.Breaker(worktreeKey(repo))
	return ok && b.reducedAt(r.now())
}

// fullSucceeded returns a reduced worktree to full scope once a full status has finished. It
// leaves the status stage first: git is done, so a deadline that passes during this write is
// not git's and must not trip the breaker again.
func (r Runner) fullSucceeded(state *runState, repo gitinfo.Repo) {
	state.at("shell scope")
	m, ok := r.scopeMemory()
	if !ok {
		return
	}
	if b, found := m.Breaker(worktreeKey(repo)); found && b.Scope == scopeReduced {
		m.SetBreaker(worktreeKey(repo), breaker{V: 1, Worktree: repo.Worktree, Scope: scopeFull, LastFullOKMS: r.now().UnixMilli()})
	}
}

// tripIfSlow records a trip when the reconcile's own deadline passed, its parent not cancelled,
// while its full status was running, uncapped, with at least half the budget its own. Less than
// half means earlier stages took the budget, and git is then not the evidence. It runs before
// the hook returns, because the process exits right after. A write that stalls past the hook's
// own deadline is lost, and the output stays valid.
func (r Runner) tripIfSlow(parent, ctx context.Context, budget time.Duration, state *runState, run *shellRun) {
	stage, _ := state.snapshot()
	repo, full := run.read()
	a := &run.attempt
	m, ok := r.scopeMemory()
	if !ok || !errors.Is(ctx.Err(), context.DeadlineExceeded) || parent.Err() != nil ||
		stage != "shell status" || !full || !a.Started() || a.Capped() || a.Allowance() < budget/2 {
		return
	}
	key := worktreeKey(repo)
	previous, _ := m.Breaker(key)
	cooldown := breakerFirst
	if previous.Scope == scopeReduced && previous.CooldownMS > 0 {
		cooldown = min(2*time.Duration(previous.CooldownMS)*time.Millisecond, breakerCap)
	}
	now := r.now()
	m.SetBreaker(key, breaker{
		V: 1, Worktree: repo.Worktree, Scope: scopeReduced,
		Reason:    "a full git status passed its " + budget.String() + " budget",
		TrippedMS: now.UnixMilli(), NextFullMS: now.Add(cooldown).UnixMilli(), CooldownMS: cooldown.Milliseconds(),
		LastFullOKMS: previous.LastFullOKMS,
	})
}

// isOlderHub is whether a scoped request was refused as a hub without the route refuses it:
// not_found from Iris, a bare 404 from a proxy in front of it, or bad_request.
func isOlderHub(err error) bool {
	var status *hubclient.StatusError
	return hubclient.IsCode(err, "not_found") || hubclient.IsCode(err, "bad_request") || (errors.As(err, &status) && status.Code == 404)
}

// scopedShellHub sends reduced snapshots on the scoped route (ADR 018). The hub client can; a
// hub from before the route answers not_found.
type scopedShellHub interface {
	ReconcileScoped(context.Context, hubclient.ScopedRequest) (hubclient.CheckResponse, error)
	ReconcileScopedSession(context.Context, hubclient.ScopedSessionRequest) (hubclient.ShellResponse, error)
}

// shell reconciles a shell call with the hub: in reduced scope when the worktree's breaker says
// so and the hub can be sent a reduced snapshot, otherwise in full scope.
// fromGit says that err came from git rather than the hub.
func (r Runner) shell(ctx context.Context, hub shellHub, ev Event, repo gitinfo.Repo, state *runState, run *shellRun) (out hubclient.CheckResponse, fromGit bool, err error) {
	if scoped, ok := hub.(scopedShellHub); ok && r.reducedDue(repo) {
		return r.shellReduced(ctx, scoped, hub, ev, repo, state, run, shellPhase(ev))
	}
	return r.shellFull(ctx, hub, ev, repo, state, run, shellPhase(ev))
}

// shellReduced reconciles in reduced scope (ADR 018). It sends nothing it cannot vouch for: the
// gitlink set and the status must have read one generation of the index, and a failed scoped
// call is never sent again on the plain route, which would read it as full.
func (r Runner) shellReduced(ctx context.Context, hub scopedShellHub, plain shellHub, ev Event, repo gitinfo.Repo, state *runState, run *shellRun, phase string) (out hubclient.CheckResponse, fromGit bool, err error) {
	m, _ := r.scopeMemory() // reducedDue found it
	var sess *hubclient.SessionResponse
	if !m.Capable(r.hubURL()) {
		// Whether this hub takes a reduced snapshot is not known yet. One registration says,
		// before anything is read; a worktree whose full status always times out learns it so.
		state.at("shell session")
		s, err := r.register(ctx, ev, repo)
		if err != nil {
			return out, false, err
		}
		if !hubclient.ShellScopeCapable(s.Capabilities) {
			run.setFull()
			state.at("shell status")
			taken, err := r.status(ctx, repo.Worktree, gitinfo.FullScope, &run.attempt)
			if err != nil {
				return out, true, err
			}
			r.fullSucceeded(state, repo)
			state.at("shell reconcile")
			out, err = plain.Reconcile(ctx, hubclient.ReconcileRequest{ExecutionID: s.ExecutionID, Generation: s.Generation, Kind: phase, Status: taken})
			return out, false, err
		}
		sess = &s
	}
	state.at("shell scope")
	index := filepath.Join(repo.GitDir, "index")
	before, err := r.indexID(index)
	if err != nil {
		return out, true, hookFailure(shellScopeUnknown)
	}
	set, err := r.gitlinks(ctx, repo.Worktree)
	if err != nil {
		return out, true, hookFailure(shellScopeUnknown)
	}
	state.at("shell status")
	taken, err := r.status(ctx, repo.Worktree, gitinfo.ReducedScope, &run.attempt)
	if err != nil {
		return out, true, err
	}
	state.at("shell scope")
	if after, err := r.indexID(index); err != nil || after != before {
		return out, true, hookFailure(shellScopeUnknown)
	}
	// Up to 500 submodule paths and the status can still pass the hub's body limit. The hub then
	// answers too_large, the snapshot is lost, and the reconcile fails open like any hub error.
	scope := hubclient.Scope{Mode: "dirty", Submodules: set}
	if sess != nil {
		state.at("shell reconcile")
		out, err = hub.ReconcileScoped(ctx, hubclient.ScopedRequest{ReconcileRequest: hubclient.ReconcileRequest{ExecutionID: sess.ExecutionID, Generation: sess.Generation, Kind: phase, Status: taken}, Scope: scope})
	} else {
		state.at("shell session")
		out, err = r.reconcileScopedSession(ctx, hub, ev, repo, phase, taken, scope)
	}
	switch {
	case err == nil:
		return out, false, nil
	case isOlderHub(err):
		// The hub was rolled back or never had the route: forget the capability, send nothing.
		m.SetCapable(r.hubURL(), false)
		return out, false, err
	case hubclient.IsCode(err, "lost"):
		// The session's proof is repaired as a registration repairs it, and this snapshot is
		// dropped; the next one compares with whatever baseline the Execution has.
		state.at("shell session")
		if _, err = r.register(ctx, ev, repo); err != nil {
			return out, false, err
		}
		return hubclient.CheckResponse{}, false, nil
	}
	return out, false, err
}

// reconcileScopedSession is the one-call form of a reduced snapshot.
func (r Runner) reconcileScopedSession(ctx context.Context, hub scopedShellHub, ev Event, repo gitinfo.Repo, phase string, snapshot map[string]string, scope hubclient.Scope) (hubclient.CheckResponse, error) {
	req := hubclient.ScopedSessionRequest{ShellSessionRequest: hubclient.ShellSessionRequest{SessionRequest: r.sessionReq(ev, repo), Kind: phase, Status: snapshot}, Scope: scope}
	m := r.memory()
	if m != nil {
		req.Generation = m.Recall(string(ev.Client), ev.SessionID)
	}
	res, err := hub.ReconcileScopedSession(ctx, req)
	r.answered(m, ev, res, err)
	return res.CheckResponse, err
}

// indexID and gitlinks read the index for a reduced snapshot, or the Runner's stand-ins.
func (r Runner) indexID(path string) (gitinfo.IndexID, error) {
	if r.IndexID != nil {
		return r.IndexID(path)
	}
	return gitinfo.ReadIndexID(path)
}

func (r Runner) gitlinks(ctx context.Context, root string) ([]string, error) {
	if r.Gitlinks != nil {
		return r.Gitlinks(ctx, root)
	}
	return gitinfo.Gitlinks(ctx, root)
}
