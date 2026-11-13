package binding

import (
	"context"
	"os"

	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/proc"
)

// WakeTarget is the positive session binding a native wake may follow. SessionID keeps
// the original identity named by the client; each native adapter validates its syntax.
// Client is empty for Codex to retain the original target snapshots.
type WakeTarget struct {
	SessionID string
	Bound     Bound
	Host      string
	Client    string
}

// NewAntigravityWake constructs a lookup-only Resolver for a detached Antigravity wake
// helper. It never reads IRIS_EXECUTION or registers a fallback Execution, and binds only
// to the hook-registered Execution of ancestor whose session_id equals session.
func NewAntigravityWake(h *hubclient.Client, session string, ancestor proc.Ancestor) *Resolver {
	host, _ := os.Hostname()
	cwd, _ := os.Getwd()
	r := &Resolver{
		Hub:         h,
		Client:      "antigravity",
		Host:        host,
		CWD:         cwd,
		Getenv:      func(string) string { return "" },
		Ancestor:    func() (proc.Ancestor, error) { return ancestor, nil },
		wakeSession: session,
	}
	r.decided.Store(decide("", ""))
	return r
}

// LookupWake performs one lookup-only bind when an Antigravity wake resolver is
// currently unbound, then returns Primary. It never registers a fallback Execution.
func (r *Resolver) LookupWake(ctx context.Context) (WakeTarget, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.wakeSession != "" {
		r.lookupHeld(ctx)
	}
	return r.primaryHeld()
}

func (r *Resolver) lookupAntigravityWakeHeld(ctx context.Context) (Bound, error) {
	a := r.ancestor()
	if a.PID <= 0 || a.StartMS <= 0 || !proc.SingleSession(a, "antigravity") {
		return Bound{}, errUnusable
	}
	var b Bound
	err := r.Hub.Do(ctx, "POST", "/v1/bind", map[string]any{
		"client":              "antigravity",
		"host":                r.Host,
		"cwd":                 r.CWD,
		"client_pid":          a.PID,
		"client_pid_start_ms": a.StartMS,
	}, &b)
	if IsCode(err, "ambiguous") {
		b, err = r.bySession(ctx, r.wakeSession)
	}
	if err != nil {
		return Bound{}, err
	}
	if b.SessionID != r.wakeSession {
		return Bound{}, errUnusable
	}
	b.Rung = 3
	r.bound = b
	return b, nil
}

// Primary snapshots this process's current native session binding. It does no lookup
// or registration: undecided, lookup-only and fallback registrations cannot arm a wake.
func (r *Resolver) Primary() (WakeTarget, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.primaryHeld()
}

// StartIfPrimary admits and starts a native operation in the same hold that checks
// its target, so binding replacement or loss cannot slip between those operations.
// start must only initiate the operation: the caller waits and handles its output
// after this method returns. Neither start nor its caller may hold a sessions
// file lock while entering this method.
func (r *Resolver) StartIfPrimary(expected WakeTarget, start func() error) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	current, ok := r.primaryHeld()
	if !ok || current != expected {
		return false, nil
	}
	if err := start(); err != nil {
		return false, err
	}
	return true, nil
}

// primaryHeld reads only positive cached evidence, with mu held. A binding decision
// alone is insufficient: the resolver must also retain the live hook-registered session.
func (r *Resolver) primaryHeld() (WakeTarget, bool) {
	m := r.decided.Load()
	if r.Host == "" || r.bound.ExecutionID == "" || r.bound.Generation <= 0 ||
		r.lost || r.gone[r.bound.ExecutionID] {
		return WakeTarget{}, false
	}
	if ex := r.Getenv("IRIS_EXECUTION"); ex != "" && !r.gone[ex] {
		return WakeTarget{}, false
	}
	switch r.Client {
	case "codex":
		if m == nil || m.mode != root || r.bound.Rung != 2 {
			return WakeTarget{}, false
		}
		return WakeTarget{SessionID: m.session, Bound: r.bound, Host: r.Host}, true
	case "kimi":
		if m == nil || m.mode != legacy || r.own || r.bound.Rung != 3 || r.bound.SessionID == "" {
			return WakeTarget{}, false
		}
		return WakeTarget{SessionID: r.bound.SessionID, Bound: r.bound, Host: r.Host, Client: "kimi"}, true
	case "antigravity":
		if m == nil || m.mode != legacy || r.wakeSession == "" || r.own || r.bound.Rung != 3 || r.bound.SessionID != r.wakeSession {
			return WakeTarget{}, false
		}
		return WakeTarget{SessionID: r.wakeSession, Bound: r.bound, Host: r.Host, Client: "antigravity"}, true
	default:
		return WakeTarget{}, false
	}
}
