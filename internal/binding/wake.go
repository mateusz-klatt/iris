package binding

// WakeTarget is the positive session binding a native wake may follow. SessionID keeps
// the original identity named by the client; each native adapter validates its syntax.
// Client is empty for Codex to retain the original target snapshots.
type WakeTarget struct {
	SessionID string
	Bound     Bound
	Host      string
	Client    string
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
	default:
		return WakeTarget{}, false
	}
}
