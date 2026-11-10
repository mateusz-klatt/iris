package binding

import (
	"context"
	"time"

	"github.com/mateusz-klatt/iris/internal/hubclient"
)

// In the runs measured on Codex 0.167.0, each tool call named its session in _meta.sessionId and
// its thread in _meta.threadId (ADR 017). Codex puts no session id in its MCP child's environment,
// and on the app-server daemon every thread's MCP child has the daemon for a parent, so for a
// session the daemon hosts this is the only positive evidence the shim has of its session.

// The observations a call's ids can lead to, in the log doctor reads. Each is fixed text, with no
// id, handle or metadata in it, and doctor groups observations by their text.
const (
	ObservedTwoSessions = "calls of two sessions"
	ObservedUnusable    = "calls of a session without a usable Execution"
)

// errUnusable answers an id-mode call whose session has no single usable Execution. The lookup
// answers alike for a session never registered and for one recovering, expired or ended, so the
// text names no cause; and it advises no handle, since in id mode none is used.
var errUnusable = &hubclient.Error{Code: "unbound", Message: "No single usable Iris Execution matches the session Codex names on this call, on this host. Its hooks may not have run, or the hub may have restarted and the session not reattached yet. Run `iris doctor`, which says whether the hooks need review in /hooks when the hub can tell, then send any prompt and retry. A call that names its session is never bound by an execution handle."}

// errNoSession answers a call that names no session in a sub-agent's process, which keeps no
// binding of its own to fall back on.
var errNoSession = &hubclient.Error{Code: "unbound", Message: "This call names no session. This Iris MCP server serves a Codex sub-agent, or a thread Iris cannot tell from one, and binds each call only by the session the call names."}

// mode is how a process binds its tool calls.
type mode int

const (
	// legacy: the first call named no session. Today's chain binds every call and uses no
	// call's id.
	legacy mode = iota
	// root: the first call came from the session's root thread, whose thread id is the session
	// id. The process keeps and renews its session's binding.
	root
	// subagent: the first call came from a sub-agent's thread, or from one Iris cannot tell from
	// it. Each call is looked up and nothing is renewed: a shim's heartbeat would put the root's
	// Execution on a 60-second lease, which a sub-agent's MCP child, whose lifetime was not
	// measured, could then leave to lapse while the root is idle.
	subagent
)

// callMode is what the first call to reach binding without a live override decided, and the
// session and thread it named. The wake (ADR 017) needs the thread: it may arm only in a root's
// process.
type callMode struct {
	mode            mode
	session, thread string
}

// decide is the mode a first call's ids give. Only a thread whose id is its session's is a
// root's; a different or missing thread id counts as a sub-agent's.
func decide(session, thread string) *callMode {
	switch {
	case session == "":
		return &callMode{mode: legacy}
	case thread == session:
		return &callMode{mode: root, session: session, thread: thread}
	}
	return &callMode{mode: subagent, session: session, thread: thread}
}

// Note records the session a tool call names, before anything else is done with the call, so a
// call that fails to decode, waits or is cancelled still counts. It holds a lock of its own,
// never mu, which a first bind holds while it waits. The first call that names a second session
// is observed, once.
func (r *Resolver) Note(session string) {
	if session == "" {
		return
	}
	r.seenMu.Lock()
	second := r.seen != "" && session != r.seen && !r.twoSeen
	if r.seen == "" {
		r.seen = session
	}
	r.twoSeen = r.twoSeen || second
	r.seenMu.Unlock()
	if second {
		r.observe(ObservedTwoSessions)
	}
}

// ResolveCall binds one tool call. session and thread are what the call's metadata names, or ""
// for none; only Codex calls name them. primary says the binding is this process's own, which
// whoami renews; a binding by lookup alone is never renewed.
//
// A live IRIS_EXECUTION binds every call, whatever the call names. Otherwise the first call to
// get here decides the mode for the process's life. If it names no session, today's chain binds
// every call and uses no call's id. If it names one, the process is in id mode: it binds only by
// the id a call names, never through a handle or the client process, and it registers, adopts and
// hands over nothing.
func (r *Resolver) ResolveCall(ctx context.Context, handle, session, thread string) (Bound, bool, error) {
	m := r.decided.Load()
	if m == nil {
		if b, overridden, err := r.overrideOrDecide(ctx, handle, session, thread); overridden {
			return b, true, err
		}
		m = r.decided.Load()
	}
	switch {
	case m.mode == legacy:
		b, err := r.Resolve(ctx, handle)
		return b, true, err
	case m.mode == root && (session == "" || session == m.session):
		b, unusable, err := r.sessionBinding(ctx, m.session)
		if unusable {
			r.noteUnusable()
		}
		return b, true, err
	case session == "":
		return Bound{}, false, errNoSession
	}
	b, err := r.lookup(ctx, session)
	return b, false, err
}

// overrideOrDecide is the step of a call that found the process undecided. Under decideMu it reads
// the decision again, so a call that another one decided for meanwhile goes on without touching
// mu. If the process is still undecided, the call takes mu: a live override binds the call as
// today, in the same hold that checks it, so a loss the heartbeat confirms meanwhile cannot slip
// between; otherwise the call decides the mode.
func (r *Resolver) overrideOrDecide(ctx context.Context, handle, session, thread string) (Bound, bool, error) {
	r.decideMu.Lock()
	defer r.decideMu.Unlock()
	if r.decided.Load() == nil {
		r.mu.Lock()
		defer r.mu.Unlock()
		if ex := r.Getenv("IRIS_EXECUTION"); ex != "" && !r.gone[ex] {
			b, err := r.resolveHeld(ctx, handle)
			return b, true, err
		}
		r.decided.Store(decide(session, thread))
	}
	return Bound{}, false, nil
}

// sessionBinding is a root's process's binding of its session. Once bound, it is refreshed by its
// Execution, as today, so a refusal leaves it cached and the heartbeat reattaches it after a hub
// restart. While the process has no binding, it looks the Execution up by the session's id. On
// the first bind it repeats the lookup for the grace period, while the session's hook may not have
// registered the session yet; after a loss it looks once, since the hook is what reattaches a
// session. An Execution found lost is never bound again. unusable says the first bind's grace
// period ran out without a usable Execution; a call that ends before then is still binding.
func (r *Resolver) sessionBinding(ctx context.Context, session string) (b Bound, unusable bool, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.bound.ExecutionID != "" {
		b, err = r.refresh(ctx)
		return b, false, err
	}
	first := len(r.gone) == 0
	if first && r.since.IsZero() {
		r.since = time.Now()
	}
	b, err = r.bySession(ctx, session)
	for delay := 100 * time.Millisecond; first && unusableAnswer(err) && time.Since(r.since) < r.Grace; delay = min(2*delay, 2*time.Second) {
		select {
		case <-ctx.Done():
			return Bound{}, false, errBinding
		case <-time.After(min(delay, r.Grace-time.Since(r.since))):
		}
		b, err = r.bySession(ctx, session)
	}
	switch {
	case first && err != nil && ctx.Err() != nil:
		// The call ended while its first bind was still under way, in a lookup or between two.
		return Bound{}, false, errBinding
	case unusableAnswer(err):
		return Bound{}, first, errUnusable
	case err != nil:
		return Bound{}, false, err
	case r.gone[b.ExecutionID]:
		return Bound{}, false, errUnusable
	}
	r.bound, r.lost = b, false
	return b, false, nil
}

// lookup attributes a call to the session it names by one lookup, without mu, so it never waits
// behind a first bind's grace period. It keeps nothing and renews nothing, so the hub's answer
// stands as given: the Executions this process found lost matter only to a binding it keeps.
func (r *Resolver) lookup(ctx context.Context, session string) (Bound, error) {
	b, err := r.bySession(ctx, session)
	if unusableAnswer(err) {
		r.noteUnusable()
		err = errUnusable
	}
	if err != nil {
		return Bound{}, err
	}
	return b, nil
}

// bySession asks the hub for the one usable Execution of session on this host: active, of the
// hub's current boot, not recovering and within its lease. It only looks and changes nothing at
// the hub. A session's id is the session-id rung, 2, which other clients reach through a variable.
func (r *Resolver) bySession(ctx context.Context, session string) (Bound, error) {
	var b Bound
	err := r.Hub.Do(ctx, "POST", "/v1/bind", map[string]any{"client": r.Client, "host": r.Host, "cwd": r.CWD, "session_id": session}, &b)
	b.Rung = 2
	return b, err
}

// unusableAnswer is a lookup's answer that no single usable Execution has the session.
func unusableAnswer(err error) bool {
	return IsCode(err, "unbound") || IsCode(err, "ambiguous")
}

// noteUnusable observes, once per process, a call that named a session without a usable
// Execution. The binding step calls it where it ends without one; nothing infers the observation
// from an error's code later, so a call cancelled while it waits is not counted.
func (r *Resolver) noteUnusable() {
	if r.unusable.CompareAndSwap(false, true) {
		r.observe(ObservedUnusable)
	}
}

func (r *Resolver) observe(text string) {
	if r.Observe != nil {
		r.Observe(text)
	}
}
