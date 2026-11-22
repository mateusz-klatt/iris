// Package binding attributes long-lived clients using the positive D6 evidence chain.
package binding

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mateusz-klatt/iris/internal/gitinfo"
	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/proc"
)

type Bound struct {
	ExecutionID  string          `json:"execution_id"`
	Generation   int             `json:"generation"`
	RepositoryID string          `json:"repository_id"`
	Agent        hubclient.Agent `json:"agent"`
	SessionID    string          `json:"session_id,omitempty"`
	Rung         int             `json:"binding_rung"`
	// Worktree and Capabilities are the Execution's worktree and what its hub takes, from which an
	// explicit claim's path is placed in the checkout that holds it (ADR 033). An older hub names
	// neither.
	Worktree     string       `json:"worktree,omitempty"`
	Capabilities Capabilities `json:"capabilities,omitempty"`
	// Outside says the Execution belongs to its host's context of sessions outside git, which
	// takes no claim (ADR 034). An older hub never says so.
	Outside bool `json:"outside_git,omitempty"`
	// HookSeen says whether a hook has reached the hub for this Execution.
	HookSeen bool `json:"hook_seen,omitempty"`
}

// Capabilities is what a hub takes beyond version one, as its answers list it. It is kept as one
// value, in the hub's order, so a binding stays comparable (see StartIfPrimary).
type Capabilities string

// UnmarshalJSON reads the hub's list.
func (c *Capabilities) UnmarshalJSON(raw []byte) error {
	var list []string
	if err := json.Unmarshal(raw, &list); err != nil {
		return err
	}
	*c = Capabilities(strings.Join(list, "\n"))
	return nil
}

// MarshalJSON writes it back as the hub's list.
func (c Capabilities) MarshalJSON() ([]byte, error) { return json.Marshal(c.List()) }

// List is the hub's list.
func (c Capabilities) List() []string {
	if c == "" {
		return []string{}
	}
	return strings.Split(string(c), "\n")
}

type Resolver struct {
	Hub               *hubclient.Client
	Client, Host, CWD string
	Getenv            func(string) string
	Ancestor          func() (proc.Ancestor, error)
	// Recall reads the hook's local generation memory for client and session without creating
	// files. An unbound shim uses it only for a reattach-only registration with generation > 0.
	Recall func(client, session string) (generation int, present bool, err error)
	// HeartbeatSource names the heartbeat source sent during refresh reattachment (default "shim").
	HeartbeatSource string
	mu              sync.Mutex
	bound           Bound
	// last preserves the most recently bound Execution (including SessionID, Rung and HookSeen)
	// after a confirmed loss clears bound, so memory-based reattachment retains positive session
	// evidence even for clients without a session environment variable.
	last Bound
	// lost is set when a heartbeat found the bound generation gone, until a fresh bind
	// succeeds. gone holds every Execution found lost: none is ever bound again, so a
	// rebind never speaks for a stale identity.
	lost bool
	gone map[string]bool
	// Grace is how long the first bind keeps looking for the session's hook's Execution
	// before this process registers one of its own. The MCP often starts before SessionStart
	// has registered the session, so registering at once would split it. New sets 15 s.
	Grace time.Duration
	// since is when the first bind began; own says the binding is an Execution this process
	// registered, which it gives up for the hook's once one appears.
	since time.Time
	own   bool
	// pending keeps an unconfirmed handover's original source and destination.
	// Its /end may have committed even when the response never arrived.
	pending *handover

	// olderHub is set when this process had to register outside git on the legacy route, and
	// olderHubSaid once Observe has been told so (ADR 034).
	olderHub, olderHubSaid bool

	// Observe receives an observation's fixed text for doctor. cmd/iris wires it to the hook
	// package's bounded writer; nil drops it. It is called outside every lock.
	Observe func(string)
	// decided is what the first tool call that reached binding without a live override decided
	// (ADR 017), nil until then. It never changes once set, so a call reads it without mu.
	decided atomic.Pointer[callMode]
	// decideMu lets one undecided call at a time check the override and decide. A call that
	// found the process undecided reads decided again under it, before it takes mu, so it never
	// waits for mu if another call decided meanwhile and is now holding mu for its first bind.
	decideMu sync.Mutex
	// seenMu guards the first session a call named, apart from mu, which a first bind holds
	// while it waits: a call is noted before it waits.
	seenMu  sync.Mutex
	seen    string
	twoSeen bool
	// unusable is set once a call has been observed naming a session without a usable Execution.
	unusable    atomic.Bool
	wakeSession string
}

type handover struct {
	from Bound
	to   string
}

func New(h *hubclient.Client, client string) *Resolver {
	host, _ := os.Hostname()
	cwd, _ := os.Getwd()
	return &Resolver{Hub: h, Client: client, Host: host, CWD: cwd, Getenv: os.Getenv, Ancestor: proc.Nearest, Grace: 15 * time.Second}
}
func Key() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read always fills b or crashes the program.
	return hex.EncodeToString(b[:])
}

// Lost reports whether a heartbeat found this binding's generation gone at the hub and no
// fresh bind has replaced it yet. Only a heartbeat confirms a loss: a lost answer to any
// other call can be a session still recovering after a hub restart, which its next
// heartbeat reattaches.
func (r *Resolver) Lost() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lost
}

func IsCode(err error, code string) bool {
	var e *hubclient.Error
	return errors.As(err, &e) && e.Code == code
}

// errBinding answers a call that ended while the first bind was still waiting for the
// session's hook to register it.
var errBinding = &hubclient.Error{Code: "unbound", Message: "Iris is still binding this session; retry in a few seconds."}

// errLost answers a call while the binding is lost and no fresh bind has succeeded.
var errLost = &hubclient.Error{Code: "lost", Message: "This session's Execution is gone at the hub. Iris binds afresh once the session has reattached: send any prompt, then retry."}

// LostError keeps the failed retry available for diagnosis while calls still answer lost.
// Cause is evidence about the retry, never permission to register or reuse a lost Execution.
type LostError struct{ Cause error }

func (e *LostError) Error() string { return errLost.Error() }

func (e *LostError) Unwrap() []error { return []error{errLost, e.Cause} }

// Resolve refreshes an existing binding from its preserved Execution, making one when
// there is none. Repository merges can change its repository without changing the
// Execution, generation or mailbox. After a loss it binds afresh
// through the usual rungs, but never to an Execution found lost, however that comes back:
// IRIS_EXECUTION, a handle argument, or the hub's answer.
func (r *Resolver) Resolve(ctx context.Context, handle string) (Bound, error) {
	r.mu.Lock()
	b, err := r.resolveHeld(ctx, handle)
	say := r.olderHub && !r.olderHubSaid
	r.olderHubSaid = r.olderHubSaid || say
	r.mu.Unlock()
	if say {
		r.observe(ObservedOlderHub)
	}
	return b, err
}

// ObservedOlderHub is the observation, for doctor, of a session outside git that registered on
// the legacy route because its hub has no outside route: the hub makes a project of its folder
// until it is upgraded (ADR 034). It is fixed text and says nothing of which folder.
const ObservedOlderHub = "registered outside git on the legacy route: the hub is older than ADR 034"

// resolveHeld is Resolve with mu held.
func (r *Resolver) resolveHeld(ctx context.Context, handle string) (Bound, error) {
	if r.bound.ExecutionID != "" && (handle == "" || handle == r.bound.ExecutionID || r.gone[handle]) {
		b, err := r.refresh(ctx)
		if err == nil || r.bound.ExecutionID != "" {
			return b, err
		}
	}
	b, err := r.resolve(ctx, handle, "")
	if IsCode(err, "unbound") {
		if session, rung := r.recallSessionHeld(handle); session != "" {
			if rb, attempted, rerr := r.reattachByMemoryHeld(ctx, session, rung); attempted {
				b, err = rb, rerr
			}
		}
	}
	b, err = r.orRegister(ctx, handle, b, err)
	if err == nil && r.gone[b.ExecutionID] {
		r.bound, b, err = Bound{}, Bound{}, &hubclient.Error{Code: "lost", Message: "the binding retry returned an Execution already found lost"}
	}
	if err != nil && r.lost {
		return Bound{}, &LostError{Cause: err}
	}
	if err == nil {
		r.keepHeld(b)
	}
	return b, err
}

func (r *Resolver) keepHeld(b Bound) {
	r.bound = b
	r.last = b
	r.lost = false
}

func (r *Resolver) markLostHeld(b Bound) {
	if r.gone == nil {
		r.gone = map[string]bool{}
	}
	r.gone[b.ExecutionID] = true
	if r.bound.ExecutionID == b.ExecutionID {
		r.last = b
		r.bound, r.lost = Bound{}, true
	}
}

func (r *Resolver) heartbeatSource() string {
	if r.HeartbeatSource != "" {
		return r.HeartbeatSource
	}
	return "shim"
}

func (r *Resolver) bindExecutionHeld(ctx context.Context, orig Bound) (Bound, error) {
	b := orig
	// A bind answer names the worktree, capabilities, outside and hook_seen when its hub
	// has them. One rolled back since names none, and the binding must not keep stale flags.
	b.Worktree, b.Capabilities, b.Outside, b.HookSeen = "", "", false, false
	if err := r.Hub.Do(ctx, "POST", "/v1/bind", map[string]any{"execution": b.ExecutionID, "client": r.Client, "host": r.Host}, &b); err != nil {
		return Bound{}, err
	}
	return b, nil
}

// refresh checks the bound Execution at the hub, with mu held, and keeps what the hub says of it:
// a repository merge can change its repository without changing the Execution. When /v1/bind
// answers lost, refresh immediately heartbeats with the exact remembered generation so a
// recovering Execution reattaches on the tool call itself, then retries /v1/bind. A heartbeat
// answering lost marks the Execution lost while keeping its last identity; any other failure
// leaves the cached binding intact.
func (r *Resolver) refresh(ctx context.Context) (Bound, error) {
	b, err := r.bindExecutionHeld(ctx, r.bound)
	if err == nil {
		r.keepHeld(b)
		return b, nil
	}
	if !IsCode(err, "lost") {
		return Bound{}, err
	}
	fresh := r.bound
	fresh.Outside = false
	if hbErr := r.Hub.Do(ctx, "POST", "/v1/executions/"+r.bound.ExecutionID+"/heartbeat", map[string]any{"generation": r.bound.Generation, "source": r.heartbeatSource()}, &fresh); hbErr != nil {
		if IsCode(hbErr, "lost") {
			r.markLostHeld(r.bound)
		}
		return Bound{}, hbErr
	}
	r.bound = fresh
	r.last = fresh
	b, err = r.bindExecutionHeld(ctx, r.bound)
	if err != nil {
		return Bound{}, err
	}
	r.keepHeld(b)
	return b, nil
}

// recallSessionHeld returns the positively identified hook-registered session and binding rung
// eligible for generation-memory reattachment, with mu held. A live IRIS_EXECUTION override or
// an explicit non-gone handle takes precedence and disables memory reattachment.
func (r *Resolver) recallSessionHeld(handle string) (string, int) {
	if r.Recall == nil {
		return "", 0
	}
	if ex := r.Getenv("IRIS_EXECUTION"); ex != "" && !r.gone[ex] {
		return "", 0
	}
	if handle != "" && !r.gone[handle] {
		return "", 0
	}
	if r.last.HookSeen && r.last.SessionID != "" {
		return r.last.SessionID, r.last.Rung
	}
	if variable := sessionVariable[r.Client]; variable != "" {
		if session := r.Getenv(variable); session != "" {
			return session, 2
		}
	}
	return "", 0
}

// reattachByMemoryHeld attempts a reattach-only registration (generation > 0) from the hook's
// local generation memory for a positively identified session, with mu held. Missing memory
// returns attempted=false so nothing is sent. Malformed or mismatched memory returns a lost
// error with attempted=true and never falls back to a generation-0 registration.
func (r *Resolver) reattachByMemoryHeld(ctx context.Context, session string, rung int) (Bound, bool, error) {
	if r.Recall == nil {
		return Bound{}, false, nil
	}
	gen, present, err := r.Recall(r.Client, session)
	if !present {
		return Bound{}, false, nil
	}
	if err != nil || gen <= 0 {
		return Bound{}, true, &hubclient.Error{Code: "lost", Message: "remembered session generation is malformed"}
	}
	resp, err := r.Hub.RegisterSession(ctx, hubclient.SessionRequest{
		Client:     r.Client,
		SessionID:  session,
		Host:       r.Host,
		CWD:        r.CWD,
		Generation: gen,
		Source:     "shim",
	})
	if err != nil {
		return Bound{}, true, err
	}
	return Bound{
		ExecutionID:  resp.ExecutionID,
		Generation:   resp.Generation,
		RepositoryID: resp.RepositoryID,
		Agent:        resp.Agent,
		SessionID:    session,
		Rung:         rung,
		Worktree:     resp.Worktree,
		Capabilities: Capabilities(strings.Join(resp.Capabilities, "\n")),
		Outside:      resp.OutsideGit,
		HookSeen:     true,
	}, true, nil
}

// orRegister is the last resort when no rung finds the session: a session whose hooks never
// ran, as in an untrusted folder, still gets messaging. First it keeps looking, with backoff,
// for the grace period, because the hook usually registers within it; and never after a loss,
// when the hook is what reattaches. Only a process that could name its session registers: by
// its session id, or as a single-session client process (D6).
func (r *Resolver) orRegister(ctx context.Context, handle string, b Bound, err error) (Bound, error) {
	if !IsCode(err, "unbound") || len(r.gone) > 0 || r.last.HookSeen {
		return b, err
	}
	if r.since.IsZero() {
		r.since = time.Now()
	}
	for delay := 100 * time.Millisecond; time.Since(r.since) < r.Grace; delay = min(2*delay, 2*time.Second) {
		select {
		case <-ctx.Done():
			return b, errBinding
		case <-time.After(min(delay, r.Grace-time.Since(r.since))):
		}
		if b, err = r.resolve(ctx, handle, ""); !IsCode(err, "unbound") {
			return b, err
		}
	}
	session := ""
	if variable := sessionVariable[r.Client]; variable != "" {
		session = r.Getenv(variable)
	}
	ancestor := r.ancestor()
	switch {
	case session != "":
		return r.register(ctx, session, ancestor, false, 2)
	case ancestor.PID > 0 && ancestor.StartMS > 0 && proc.SingleSession(ancestor, r.Client):
		return r.register(ctx, "mcp-"+fmt.Sprint(ancestor.PID)+"-"+fmt.Sprint(ancestor.StartMS), ancestor, true, 3)
	}
	return b, err
}

// register makes an Execution of this process's own, which it gives up as soon as the hook's
// appears (see adopt).
func (r *Resolver) register(ctx context.Context, session string, a proc.Ancestor, single bool, rung int) (Bound, error) {
	req := hubclient.SessionRequest{Client: r.Client, SessionID: session, Host: r.Host, CWD: r.CWD, Source: "mcp", ClientPID: a.PID, ClientPIDStartMS: a.StartMS, SingleSession: single}
	repo, err := gitinfo.Find(r.CWD)
	var response hubclient.SessionResponse
	switch {
	case err == nil:
		req.CommonDir = repo.CommonDir
		req.Worktree = repo.Worktree
		req.WorktreeID = repo.WorktreeID
		req.RemoteURL = repo.RemoteURL
		req.RootCommit = repo.RootCommit
		req.Branch = repo.Branch
		response, err = r.Hub.RegisterSession(ctx, req)
	case errors.Is(err, gitinfo.ErrNotRepo):
		// Only a directory with no .git above it is outside git; a checkout Iris cannot read
		// fails the registration instead (ADR 034). A hub without the outside route takes the
		// same request on the session route, as before, and makes a project of the folder.
		response, err = r.Hub.RegisterOutsideSession(ctx, req)
		if hubclient.OlderHub(err) {
			response, err = r.Hub.RegisterSession(ctx, req)
			// Said only once the legacy registration it reports has happened.
			r.olderHub = r.olderHub || err == nil
		}
	}
	if err != nil {
		return Bound{}, err
	}
	b := Bound{ExecutionID: response.ExecutionID, Generation: response.Generation, RepositoryID: response.RepositoryID, Agent: response.Agent, SessionID: session, Rung: rung, Worktree: response.Worktree, Capabilities: Capabilities(strings.Join(response.Capabilities, "\n")), Outside: response.OutsideGit}
	r.bound, r.own = b, true
	return b, nil
}

// adopt gives up this process's own Execution for the session's hook's, once that exists:
// a hook slower than the grace period must not leave the session split either.
func (r *Resolver) adopt(ctx context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.own && r.pending == nil {
		return
	}
	// Adoption runs before the heartbeat and holds the binding lock. A stalled
	// lookup or handover must leave time to renew the session's 60-second lease.
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if r.pending == nil {
		mine := r.bound
		b, err := r.resolve(ctx, "", mine.ExecutionID)
		if err != nil {
			r.bound = mine
			return
		}
		r.keepHeld(b)
		r.own = false
		r.pending = &handover{from: mine, to: b.ExecutionID}
	}
	// Retry the same idempotent end until its response confirms that the mail moved.
	p := r.pending
	if err := r.Hub.HandOverExecution(ctx, p.from.ExecutionID, p.from.Generation, p.to); err == nil {
		r.pending = nil
	}
}

// ancestor is the nearest client process, or none when there is no way to look.
func (r *Resolver) ancestor() proc.Ancestor {
	if r.Ancestor == nil {
		return proc.Ancestor{}
	}
	a, _ := r.Ancestor()
	return a
}

var sessionVariable = map[string]string{"claude": "CLAUDE_CODE_SESSION_ID", "copilot": "COPILOT_AGENT_SESSION_ID", "grok": "GROK_SESSION_ID"}

// resolve tries the rungs in order and only binds. exclude names an Execution to skip.
func (r *Resolver) resolve(ctx context.Context, handle, exclude string) (Bound, error) {
	req := map[string]any{"client": r.Client, "host": r.Host, "cwd": r.CWD}
	if exclude != "" {
		req["exclude"] = exclude
	}
	bind := func(rung int) (Bound, error) {
		var b Bound
		err := r.Hub.Do(ctx, "POST", "/v1/bind", req, &b)
		if err == nil {
			b.Rung = rung
			r.bound = b
		}
		return b, err
	}
	ex := r.Getenv("IRIS_EXECUTION")
	var lastErr error = &hubclient.Error{Code: "unbound", Message: "Pass the execution handle from SessionStart in the execution argument, or restart with IRIS_EXECUTION set. Iris cannot identify this session safely."}
	if r.gone[handle] || r.gone[ex] {
		lastErr = &hubclient.Error{Code: "unbound", Message: "the execution handle names an Execution already found lost"}
	}
	if r.gone[handle] {
		handle = ""
	}
	if ex != "" && !r.gone[ex] {
		req["execution"] = ex
		return bind(1)
	}
	variable := sessionVariable[r.Client]
	session := ""
	if variable != "" {
		session = r.Getenv(variable)
	}
	// The session's hook is its registrar. On resume Claude Code gives an MCP server another
	// CLAUDE_CODE_SESSION_ID than its hooks, so a session id with nothing bound to it falls
	// through to the client process, which the hook's Execution records.
	if session != "" {
		req["session_id"] = session
		b, err := bind(2)
		if err == nil || !IsCode(err, "unbound") {
			return b, err
		}
		lastErr = err
		delete(req, "session_id")
	}
	ancestor := r.ancestor()
	if ancestor.PID > 0 && ancestor.StartMS > 0 {
		req["client_pid"] = ancestor.PID
		req["client_pid_start_ms"] = ancestor.StartMS
		b, err := bind(3)
		if err == nil {
			return b, nil
		}
		if !IsCode(err, "unbound") && !IsCode(err, "ambiguous") {
			return b, err
		}
		if handle != "" {
			req["execution"] = handle
			return bind(4)
		}
		return b, err
	}
	if handle != "" {
		req["execution"] = handle
		return bind(4)
	}
	return Bound{}, lastErr
}

// ObservedRebound is the observation, for doctor, of a shim whose tick bound its session's
// current Execution after a heartbeat had found the previous one lost (ADR 027). It is fixed
// text and says nothing of which session.
const ObservedRebound = "rebound on heartbeat after a loss"

// Heartbeat sends one heartbeat immediately for the currently bound Execution, if any.
// It updates the cached binding on success, marks it lost on a lost answer, returns nil when
// nothing is bound, and never revives a terminal Execution.
func (r *Resolver) Heartbeat(ctx context.Context, source string) error {
	r.mu.Lock()
	b := r.bound
	r.mu.Unlock()
	if b.ExecutionID == "" {
		return nil
	}
	return r.beatOne(ctx, b, source)
}

func (r *Resolver) beatOne(ctx context.Context, b Bound, source string) error {
	call, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	// Decode over the previous binding: older hubs return only lease_ms on an
	// ordinary heartbeat, and omit all binding fields, including its local rung.
	fresh := b
	// A heartbeat answer says outside git when its hub knows contexts (ADR 034).
	fresh.Outside = false
	err := r.Hub.Do(call, "POST", "/v1/executions/"+b.ExecutionID+"/heartbeat", map[string]any{"generation": b.Generation, "source": source}, &fresh)
	if err == nil {
		r.mu.Lock()
		// Resolve may have selected another explicit handle during this request.
		if r.bound.ExecutionID == b.ExecutionID && r.bound.Generation == b.Generation {
			r.bound = fresh
			r.last = fresh
		}
		r.mu.Unlock()
		return nil
	}
	if IsCode(err, "lost") {
		r.mu.Lock()
		r.markLostHeld(b)
		r.mu.Unlock()
	}
	return err
}

// Maintain stops before any renewal after parent loss. The process owner also cancels
// this context when the transport reaches EOF. A generation the heartbeat finds lost is
// dropped, never renewed, and the process keeps running. A shim's next tick looks for the
// session's current Execution, as a call would (ADR 027); the watch's own loop and a call's
// Resolve bind afresh too.
func (r *Resolver) Maintain(ctx context.Context, source string, interval time.Duration, parentAlive func() bool, onStop func()) {
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		if !parentAlive() {
			onStop()
			return
		}
		r.adopt(ctx)
		r.mu.Lock()
		rebound := source == "shim" && r.lookupHeld(ctx)
		renew := []Bound{r.bound}
		if r.pending != nil && !r.gone[r.pending.from.ExecutionID] {
			// Keep the fallback's mail reachable until handover is confirmed. An
			// already committed end answers lost; only the live destination binds.
			renew = append(renew, r.pending.from)
		}
		r.mu.Unlock()
		for _, b := range renew {
			if b.ExecutionID == "" {
				continue
			}
			if r.wakeSession != "" && !parentAlive() {
				onStop()
				return
			}
			if r.beatOne(ctx, b, source) == nil && rebound {
				// A rebind is only ever this tick's sole renewal: the lookup runs
				// without a pending handover.
				r.observe(ObservedRebound)
			}
		}
	}
}

// lookupHeld is a shim's tick looking for its session's current Execution while the process
// holds none, with mu held (ADR 027). A shim binds on a tool call, so without this one that
// had no call yet, or lost its generation and got no call after, renewed nothing while the
// session's hooks registered generation after generation on the hook-only lease.
//
// It binds what a call without a handle would, through rungs 1 to 3, and may reattach a
// recovering hook-registered session via local generation memory (generation > 0): it never
// registers a generation-0 Execution, never binds by a handle, never binds one found lost,
// and never decides how calls bind. Codex calls can name their session, so its first call
// decides the process's mode; Codex and a process in id mode are left to their calls. A
// failed lookup changes nothing, so a confirmed loss stays confirmed until a bind succeeds.
// It reports whether its bind replaced a generation a heartbeat had found lost.
func (r *Resolver) lookupHeld(ctx context.Context) bool {
	if m := r.decided.Load(); r.Client == "codex" || (m != nil && m.mode != legacy) {
		return false
	}
	if r.bound.ExecutionID != "" || r.own || r.pending != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var b Bound
	var err error
	if r.wakeSession != "" {
		b, err = r.lookupAntigravityWakeHeld(ctx)
	} else {
		b, err = r.resolve(ctx, "", "")
		if IsCode(err, "unbound") {
			if session, rung := r.recallSessionHeld(""); session != "" {
				if rb, attempted, rerr := r.reattachByMemoryHeld(ctx, session, rung); attempted {
					b, err = rb, rerr
				}
			}
		}
	}
	if err != nil {
		return false
	}
	if r.gone[b.ExecutionID] {
		r.bound = Bound{}
		return false
	}
	lost := r.lost
	r.keepHeld(b)
	return lost
}

// Rebound reports a positively known replacement for the stream's binding. An
// empty cache is not evidence of another Execution; the hub closes a lost stream.
func (r *Resolver) Rebound(b Bound) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.bound.ExecutionID != "" && (r.bound.ExecutionID != b.ExecutionID || r.bound.Generation != b.Generation)
}
