package binding

// WakeTarget is the positive root binding a Codex wake may follow. SessionID keeps
// the original identity named by the client; the queue runner validates its UUID
// syntax separately before constructing a command.
type WakeTarget struct {
	SessionID string
	Bound     Bound
	Host      string
}

// Primary snapshots this process's current Codex root binding. It does no lookup
// or registration: legacy, undecided and lookup-only processes cannot arm a wake.
func (r *Resolver) Primary() (WakeTarget, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.primaryHeld()
}

// StartIfPrimary admits and starts a command in the same hold that checks its
// target, so binding replacement or loss cannot slip between those operations.
// start must only start the command: the caller waits and handles its output
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

// primaryHeld reads only positive cached evidence, with mu held. A root decision
// alone is insufficient: its session lookup must have retained a live binding.
func (r *Resolver) primaryHeld() (WakeTarget, bool) {
	m := r.decided.Load()
	if r.Client != "codex" || m == nil || m.mode != root ||
		r.Host == "" || r.bound.ExecutionID == "" || r.bound.Generation <= 0 || r.bound.Rung != 2 ||
		r.lost || r.gone[r.bound.ExecutionID] {
		return WakeTarget{}, false
	}
	if ex := r.Getenv("IRIS_EXECUTION"); ex != "" && !r.gone[ex] {
		return WakeTarget{}, false
	}
	return WakeTarget{SessionID: m.session, Bound: r.bound, Host: r.Host}, true
}
