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
	"github.com/mateusz-klatt/iris/internal/sessionproof"
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
	HookSeen   bool                     `json:"hook_seen,omitempty"`
	Class      hubclient.Classification `json:"-"`
	Classified bool                     `json:"-"`
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
	// Recall reads the hook's live generation proof and end marker without creating files.
	// Both clients check them for binding and renewal. Only live proof can enable reattachment.
	Recall func(client, session string) (generation, endedThrough int, err error)
	// ObserveBindingRefusal records the local proof that refused binding or renewal. Like Recall,
	// it may run with the resolver lock held and must not call back into the resolver.
	ObserveBindingRefusal func(client, session string, ended bool)
	Evidence              EvidenceStore
	SuccessionEnabled     func(context.Context) bool
	ParentAlive           func() bool
	// HeartbeatSource names the heartbeat source sent during refresh reattachment (default "shim").
	HeartbeatSource string
	mu              sync.Mutex
	bound           Bound
	// last preserves the most recently bound process-owned Execution (rungs 2 and 3, including
	// SessionID, Rung and HookSeen) after a confirmed loss clears bound, so memory-based
	// reattachment retains positive session evidence even for clients without a session
	// environment variable, without ever leaking a foreign Rung-1 or Rung-4 handle.
	last Bound
	// lost records the dropped cache until a fresh bind succeeds. gone records loss
	// confirmed by an opted hub request. legacyGone retains the pre-opt-in behavior
	// for confirmed un-opted heartbeat losses.
	lost       bool
	gone       map[string]bool
	legacyGone map[string]bool
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
	// mismatch is the running client found at MCP startup when it is not the requested client.
	// It is set before the resolver's workers start and never changes afterwards.
	mismatch string
	// unidentified is an agent boundary that did not resolve to a client. It refuses every
	// rung without naming a client. Set once, with mismatch, before workers start.
	unidentified bool
}

type handover struct {
	from, to Bound
}

func New(h *hubclient.Client, client string) *Resolver {
	host, _ := os.Hostname()
	cwd, _ := os.Getwd()
	return &Resolver{Hub: h, Client: client, Host: host, CWD: cwd, Getenv: os.Getenv, Ancestor: AncestorFor(client), Grace: 15 * time.Second}
}

// AncestorFor is the shim's ancestor walk for client. Cursor cross-loads Claude plugins only
// (measured), so only a Claude shim reads Cursor's marker pair during the walk. Any other
// client walks without markers, resolving Cursor and Grok from executable evidence and
// stopping at an unidentified `agent` boundary.
func AncestorFor(client string) func() (proc.Ancestor, error) {
	if client == "claude" {
		return proc.Nearest
	}
	return proc.NearestWithoutMarkers
}

// ObservedClientMismatch groups observations of a shim another client cross-loaded.
const ObservedClientMismatch = "client mismatch"

// ClientMismatchObservation is the fixed, payload-free observation doctor groups and explains.
func ClientMismatchObservation(client, runner string) string {
	return ObservedClientMismatch + ": " + client + " MCP shim ran under " + runner
}

// UnidentifiedAgentObservation is the refusal when an agent process is not Cursor or Grok.
// It names no client: the shim must not invent one.
func UnidentifiedAgentObservation(client string) string {
	return ObservedClientMismatch + ": " + client + " MCP shim stopped at an unidentified agent"
}

// CheckClient refuses every binding route when another client cross-loaded this shim.
func (r *Resolver) CheckClient() bool {
	// Cursor cross-loads Claude plugins only (measured), and its MCP stdio environment carries no
	// markers, so for any other client a marker pair seen here was inherited, for example by Grok
	// started from a Cursor terminal, and is no evidence of Cursor.
	markers := r.Client == "claude" && proc.CursorEnvironment(r.Getenv)
	var markerEnv func(string) string
	if markers {
		markerEnv = r.Getenv
	}
	ancestor := r.ancestor()
	runner, known := proc.ClientNameOf(proc.Snapshot(ancestor), markerEnv)
	if known && runner == r.Client {
		return false
	}
	// ClientNameOf already accepts Cursor's marker pair on an unresolved agent when markers are
	// active, and never relabels an executable already identified as Grok or another client.
	// What remains is an agent with no executable evidence (and no honoured markers): a boundary,
	// not a client.
	if !known && proc.AgentAlias(ancestor.Name) {
		r.unidentified = true
		r.observe(UnidentifiedAgentObservation(r.Client))
		return true
	}
	// A matching nearest client keeps a native session nested in Cursor usable. Without one,
	// Cursor's marker pair identifies its cross-loaded hook shell despite an inherited Claude id.
	if !known && markers {
		runner, known = "cursor", true
	}
	if !known {
		return false
	}
	r.mismatch = runner
	r.observe(ClientMismatchObservation(r.Client, runner))
	return true
}

func (r *Resolver) clientMismatchError() error {
	if r.unidentified {
		return &hubclient.Error{Code: "client_mismatch", Message: "This Iris MCP server is for " + r.Client + ", but an unidentified agent process is running it. This server cannot bind, register, or heartbeat a session."}
	}
	if r.mismatch == "" {
		return nil
	}
	return &hubclient.Error{Code: "client_mismatch", Message: "This Iris MCP server is for " + r.Client + ", but " + r.mismatch + " is running it. Use the running client's native Iris MCP server (`iris mcp --client " + r.mismatch + "`) instead; this cross-loaded server cannot bind, register, or heartbeat a session."}
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

var errGenerationFloor = &hubclient.Error{Code: "lost", Message: "the hub has not bound the remembered session generation"}
var errMalformedGeneration = &hubclient.Error{Code: "lost", Message: "remembered session generation is malformed"}

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
	if err := r.clientMismatchError(); err != nil {
		return Bound{}, err
	}
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
	if r.bound.ExecutionID != "" && (handle == "" || handle == r.bound.ExecutionID || r.wasLostHeld(handle)) {
		original := r.bound
		b, err := r.refreshFor(ctx, handle == "")
		if err == nil || r.bound.ExecutionID != "" || original.Rung == 3 && r.gone[original.ExecutionID] {
			return b, err
		}
	}
	b, err := r.resolve(ctx, handle, "")
	if IsCode(err, "unbound") {
		if session, rung := r.recallSessionHeld(handle); session != "" {
			if rb, attempted, rerr := r.reattachByMemoryHeld(ctx, session, rung, sessionproof.Snapshot{}); attempted {
				b, err = rb, rerr
			}
		}
	}
	b, err = r.orRegister(ctx, handle, b, err)
	if err == nil && r.wasLostHeld(b.ExecutionID) {
		b, err = Bound{}, &hubclient.Error{Code: "lost", Message: "the binding retry returned an Execution already found lost"}
	}
	if err != nil && r.lost {
		return Bound{}, &LostError{Cause: err}
	}
	if err == nil {
		r.keepHeld(b)
	}
	return b, err
}

func (r *Resolver) rememberLastHeld(b Bound) {
	if rung := r.processRungHeld(b); rung > 0 && b.Generation >= r.processGenerationHeld(b.SessionID) {
		b.Rung = rung
		r.last = b
	}
}

// processRungHeld identifies the session as the process's own, even when an explicit handle
// selected it. Retain the positively known session/process rung when the handle is gone.
func (r *Resolver) processRungHeld(b Bound) int {
	if b.Rung == 2 || b.Rung == 3 {
		return b.Rung
	}
	if b.SessionID != "" {
		if b.SessionID == r.last.SessionID {
			return r.last.Rung
		}
		if b.SessionID == r.envSession() {
			return 2
		}
	}
	return 0
}

func (r *Resolver) processGenerationHeld(session string) int {
	if session != "" && r.last.SessionID == session {
		return r.last.Generation
	}
	return 0
}

func (r *Resolver) recallGeneration(session string) (generation, endedThrough int, err error) {
	if r.Recall == nil {
		return 0, 0, nil
	}
	gen, ended, err := r.Recall(r.Client, session)
	if err != nil || gen < 0 || ended < 0 {
		return 0, 0, errMalformedGeneration
	}
	return gen, ended, nil
}

func (r *Resolver) refuseBindingGeneration(session string, liveVeto, endedVeto bool) error {
	if (liveVeto || endedVeto) && r.ObserveBindingRefusal != nil {
		r.ObserveBindingRefusal(r.Client, session, endedVeto)
	}
	return errGenerationFloor
}

func (r *Resolver) keepHeld(b Bound) {
	r.bound = b
	r.rememberLastHeld(b)
	r.lost = false
}

func (r *Resolver) wasLostHeld(id string) bool {
	return r.gone[id] || r.legacyGone[id]
}

func (r *Resolver) markLostHeld(b Bound, opted bool) {
	gone := &r.legacyGone
	if opted {
		gone = &r.gone
	}
	if *gone == nil {
		*gone = make(map[string]bool)
	}
	(*gone)[b.ExecutionID] = true
	r.dropHeld(b)
}

func (r *Resolver) dropHeld(b Bound) {
	if r.bound.ExecutionID == b.ExecutionID && r.bound.Generation == b.Generation {
		r.rememberLastHeld(b)
		r.bound, r.lost, r.own = Bound{}, true, false
	}
}

// A floor mismatch proves this binding is stale. A local read error blocks use until the
// proof becomes readable, so preserve the exact generation for another verification.
func (r *Resolver) markGenerationLossHeld(b Bound, err error, opted bool) {
	if !opted && err == errGenerationFloor && !b.Class.IsHooklessSuccession() {
		r.dropHeld(b)
	}
}

func (r *Resolver) heartbeatSource() string {
	if r.HeartbeatSource != "" {
		return r.HeartbeatSource
	}
	return "shim"
}

func (r *Resolver) requestCandidate(ctx context.Context, req map[string]any, session string, rung int, opted bool) (Bound, error) {
	if opted {
		req["succession_proof"] = true
	}
	var candidate hubclient.Candidate
	if err := r.Hub.Do(ctx, "POST", "/v1/bind", req, &candidate); err != nil {
		return Bound{}, err
	}
	b, err := boundFromCandidate(candidate, session, rung)
	if ex, _ := req["execution"].(string); err == nil && ex != "" && (opted || b.Classified) && b.ExecutionID != ex {
		return Bound{}, errInvalidCandidate
	}
	return b, err
}

func (r *Resolver) bindExecutionHeld(ctx context.Context, orig Bound, opted bool) (Bound, error) {
	b, err := r.requestCandidate(ctx, map[string]any{"execution": orig.ExecutionID, "client": r.Client, "host": r.Host}, orig.SessionID, orig.Rung, opted)
	if err != nil {
		return Bound{}, err
	}
	if (opted || b.Classified || orig.Classified) && (b.ExecutionID != orig.ExecutionID || b.Generation != orig.Generation) {
		return Bound{}, errInvalidCandidate
	}
	if orig.Class.IsHooklessSuccession() && !b.Classified {
		b.Class, b.Classified, b.HookSeen = orig.Class, true, false
	}
	return b, nil
}

// shimSessionHeld identifies the only cached paths allowed to use a hookless succession child.
func (r *Resolver) shimSessionHeld(b Bound) bool {
	if b.Rung != 2 || r.HeartbeatSource == "watch" {
		return false
	}
	if m := r.decided.Load(); m != nil && m.mode != legacy {
		return m.mode == root && m.session == b.SessionID
	}
	return b.SessionID != "" && b.SessionID == r.envSession()
}

func (r *Resolver) admitHeld(ctx context.Context, b Bound, allowHookless, opted bool, prior sessionproof.Snapshot) error {
	return r.admitCandidate(ctx, b, r.processGenerationHeld(b.SessionID), r.processRungHeld(b) > 0, allowHookless, opted, prior)
}

// refresh checks for repository and session changes with mu held. An opted Rung-3
// binding recovers through registration. Other recovering bindings renew their
// remembered pair and then confirm it through bind.
func (r *Resolver) refresh(ctx context.Context) (Bound, error) {
	return r.refreshFor(ctx, true)
}

func (r *Resolver) refreshFor(ctx context.Context, native bool) (Bound, error) {
	original := r.bound
	opted := r.successionEnabled(ctx)
	allowHookless := native && r.shimSessionHeld(original)
	var before sessionproof.Snapshot
	if opted && original.SessionID != "" {
		var err error
		before, err = r.inspectEvidence(ctx, original.SessionID, opted)
		if err != nil {
			return Bound{}, err
		}
	}
	b, err := r.bindExecutionHeld(ctx, original, opted)
	if hubLost(err) {
		if !opted && original.Class.IsHooklessSuccession() {
			return Bound{}, err
		}
		if opted && original.Rung == 3 {
			b, err := r.reattachRung3Held(ctx, original, before)
			if err == nil {
				r.keepHeld(b)
			}
			return b, err
		}
		if err := r.admitHeld(ctx, original, allowHookless, opted, before); err != nil {
			r.markGenerationLossHeld(original, err, opted)
			return Bound{}, err
		}
		candidate, hbErr := r.sendHeartbeat(ctx, original, r.heartbeatSource(), opted)
		if hbErr != nil {
			if IsCode(hbErr, "lost") {
				r.markLostHeld(original, opted)
			}
			return Bound{}, hbErr
		}
		fresh, mergeErr := mergeCandidate(original, candidate)
		if mergeErr != nil {
			return Bound{}, mergeErr
		}
		if err := r.admitHeld(ctx, fresh, allowHookless, opted, before); err != nil {
			r.markGenerationLossHeld(original, err, opted)
			return Bound{}, err
		}
		if !opted {
			r.keepHeld(fresh)
		}
		b, err = r.bindExecutionHeld(ctx, fresh, opted)
		if err == nil && (b.ExecutionID != fresh.ExecutionID || b.Generation != fresh.Generation ||
			opted && !b.HookSeen && !b.Class.IsHooklessSuccession()) {
			return Bound{}, errInvalidCandidate
		}
	}
	if err != nil {
		return Bound{}, err
	}
	if err := r.admitHeld(ctx, b, allowHookless, opted, before); err != nil {
		r.markGenerationLossHeld(original, err, opted)
		return Bound{}, err
	}
	r.keepHeld(b)
	return b, nil
}

// recallSessionHeld returns the positively identified hook-registered session and binding rung
// eligible for generation-memory reattachment, with mu held. A live IRIS_EXECUTION override or
// an explicit non-gone handle takes precedence and disables memory reattachment.
func (r *Resolver) recallSessionHeld(handle string) (string, int) {
	if (r.Evidence == nil && r.Recall == nil) || r.HeartbeatSource == "watch" {
		return "", 0
	}
	if r.explicitBindingHeld(handle) {
		return "", 0
	}
	if r.last.HookSeen && r.last.SessionID != "" {
		return r.last.SessionID, r.last.Rung
	}
	if session := r.envSession(); session != "" {
		return session, 2
	}
	return "", 0
}

func (r *Resolver) explicitBindingHeld(handle string) bool {
	ex := r.Getenv("IRIS_EXECUTION")
	return (ex != "" && !r.wasLostHeld(ex)) || (handle != "" && !r.wasLostHeld(handle))
}

// reattachByMemoryHeld attempts a reattach-only registration (generation > 0) from the hook's
// local generation memory for a positively identified session, with mu held. Missing memory
// returns attempted=false so nothing is sent. Malformed or mismatched memory returns a lost
// error with attempted=true and never falls back to a generation-0 registration.
func (r *Resolver) reattachByMemoryHeld(ctx context.Context, session string, rung int, before sessionproof.Snapshot) (Bound, bool, error) {
	opted := r.successionEnabled(ctx)
	if opted && rung == 3 {
		b, err := r.reattachRung3Held(ctx, r.last, sessionproof.Snapshot{})
		return b, true, err
	}
	snap, err := r.inspectEvidence(ctx, session, opted)
	if err != nil {
		return Bound{}, true, err
	}
	gen, ended := snap.GenerationFloor(), max(snap.Ended, before.Ended)
	snap.Ended = ended
	if ended > 0 && gen <= ended {
		return Bound{}, true, r.refuseBindingGeneration(session, false, true)
	}
	if gen == 0 && before.GenerationFloor() == 0 {
		return Bound{}, false, nil
	}
	if gen < max(r.processGenerationHeld(session), before.GenerationFloor()) {
		return Bound{}, true, r.refuseBindingGeneration(session, true, false)
	}
	if pair, ok := before.ExactPair(); ok && pair.Generation == gen && snap.Record != before.Record {
		return Bound{}, true, errGenerationFloor
	}
	if opted && snap.RecordState == sessionproof.FileValid && snap.Record.Origin == sessionproof.OriginShim {
		b, err := r.reattachRecordedHeld(ctx, session, snap)
		return b, true, err
	}
	req := hubclient.SessionRequest{
		Client:     r.Client,
		SessionID:  session,
		Host:       r.Host,
		CWD:        r.CWD,
		Generation: gen,
		Source:     "shim",
	}
	if opted {
		req.EndedThrough, req.SuccessionProof = ended, true
	}
	if rung == 3 {
		anc := r.ancestor()
		if anc.PID <= 0 || anc.StartMS <= 0 {
			return Bound{}, false, nil
		}
		req.ClientPID = anc.PID
		req.ClientPIDStartMS = anc.StartMS
	}
	b, err := r.registerRecoveryHeld(ctx, req, rung, opted, snap)
	return b, true, err
}

func (r *Resolver) registerRecoveryHeld(ctx context.Context, req hubclient.SessionRequest, rung int, opted bool, before sessionproof.Snapshot) (Bound, error) {
	var resp hubclient.SessionResponse
	var err error
	if opted && rung == 3 {
		err = postProcessBound(ctx, r.Hub, "/v1/sessions", req, &resp)
	} else {
		resp, err = r.Hub.RegisterSession(ctx, req)
	}
	if err != nil {
		return Bound{}, err
	}
	b, err := shimRecoveryBinding(resp, req, rung)
	if err != nil {
		return Bound{}, err
	}
	if err := r.admitHeld(ctx, b, false, opted, before); err != nil {
		return Bound{}, err
	}
	return b, nil
}

func shimRecoveryBinding(resp hubclient.SessionResponse, req hubclient.SessionRequest, rung int) (Bound, error) {
	b, err := boundFromCandidate(resp.Candidate(), req.SessionID, rung)
	if err != nil {
		return Bound{}, err
	}
	// Older responses omit hook_seen. A successful positive-generation shim
	// registration has already passed the hub's hook-registration check.
	if !resp.HookSeen.Known {
		b.HookSeen = true
	}
	if !b.HookSeen || b.Generation != req.Generation {
		return Bound{}, errInvalidCandidate
	}
	return b, nil
}

func tryMemoryRecovery(err error, opted bool) bool {
	return IsCode(err, "unbound") || opted && hubLost(err)
}

func hubLost(err error) bool {
	return IsCode(err, "lost") && err != errGenerationFloor && err != errMalformedGeneration && err != errInvalidCandidate && err != errLost
}

// reattachRecordedHeld uses a published shim record to resume an exact Rung-2 pair.
// The heartbeat can resume the row, and the following bind establishes its current provenance.
func (r *Resolver) reattachRecordedHeld(ctx context.Context, session string, snap sessionproof.Snapshot) (Bound, error) {
	record := snap.Record
	b := Bound{ExecutionID: record.ExecutionID, Generation: record.Generation, SessionID: session, Rung: 2}
	if r.wasLostHeld(b.ExecutionID) || !r.shimSessionHeld(b) || r.ParentAlive == nil || !r.ParentAlive() ||
		snap.Ended >= record.PriorGeneration || b.Generation < snap.GenerationFloor() {
		return Bound{}, errGenerationFloor
	}
	candidate, err := r.sendHeartbeat(ctx, b, "shim", true)
	if err != nil {
		if IsCode(err, "lost") {
			r.markLostHeld(b, true)
		}
		return Bound{}, err
	}
	if _, err := mergeCandidate(b, candidate); err != nil {
		return Bound{}, err
	}
	fresh, err := r.bindExecutionHeld(ctx, b, true)
	if err != nil {
		return Bound{}, err
	}
	if !r.ParentAlive() || !fresh.HookSeen && !fresh.Class.IsHooklessSuccession() {
		return Bound{}, errInvalidCandidate
	}
	if err := r.admitHeld(ctx, fresh, true, true, snap); err != nil {
		return Bound{}, err
	}
	return fresh, nil
}

// reattachRung3Held never uses a heartbeat to resume a recovering process binding.
func (r *Resolver) reattachRung3Held(ctx context.Context, b Bound, before sessionproof.Snapshot) (Bound, error) {
	ancestor := r.ancestor()
	if r.Evidence == nil || !b.HookSeen || b.SessionID == "" || ancestor.PID <= 0 || ancestor.StartMS <= 0 {
		return Bound{}, errLost
	}
	snap, err := r.inspectEvidence(ctx, b.SessionID, true)
	if err != nil {
		return Bound{}, err
	}
	gen := max(snap.GenerationFloor(), before.GenerationFloor(), b.Generation)
	ended := max(snap.Ended, before.Ended)
	if gen <= ended {
		return Bound{}, r.refuseBindingGeneration(b.SessionID, false, true)
	}
	if gen == b.Generation && r.wasLostHeld(b.ExecutionID) {
		return Bound{}, errLost
	}
	proof := snap
	proof.Ended = ended
	for _, observed := range []sessionproof.Snapshot{before, snap} {
		if pair, ok := observed.ExactPair(); ok && pair.Generation == gen {
			if gen == b.Generation && pair.ExecutionID != b.ExecutionID ||
				proof.Record.Generation == gen && proof.Record.ExecutionID != pair.ExecutionID {
				return Bound{}, errGenerationFloor
			}
		}
	}
	// Retain a previously observed exact pair when a file disappeared in flight.
	if proof.Record.Generation < before.Record.Generation {
		proof.Record, proof.RecordState = before.Record, before.RecordState
	}
	req := hubclient.SessionRequest{
		Client: r.Client, SessionID: b.SessionID, Host: r.Host, CWD: r.CWD, Generation: gen,
		Source: "shim", EndedThrough: ended, ClientPID: ancestor.PID, ClientPIDStartMS: ancestor.StartMS,
	}
	if r.ParentAlive != nil && !r.ParentAlive() {
		return Bound{}, errLost
	}
	fresh, err := r.registerRecoveryHeld(ctx, req, 3, true, proof)
	if err != nil {
		if gen == b.Generation && hubLost(err) {
			r.markLostHeld(b, true)
		}
		return Bound{}, err
	}
	current := r.ancestor()
	if gen == b.Generation && fresh.ExecutionID != b.ExecutionID || current.PID != ancestor.PID || current.StartMS != ancestor.StartMS ||
		r.ParentAlive != nil && !r.ParentAlive() {
		return Bound{}, errGenerationFloor
	}
	return fresh, nil
}

// orRegister is the last resort when no rung finds the session: a session whose hooks never
// ran, as in an untrusted folder, still gets messaging. First it keeps looking, with backoff,
// for the grace period, because the hook usually registers within it; and never after a loss,
// when the hook is what reattaches. Only a process that could name its session registers: by
// its session id, or as a single-session client process (D6).
func (r *Resolver) orRegister(ctx context.Context, handle string, b Bound, err error) (Bound, error) {
	if !IsCode(err, "unbound") || r.lost || len(r.gone) > 0 || len(r.legacyGone) > 0 || r.last.HookSeen || r.explicitBindingHeld(handle) {
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
	session := r.envSession()
	ancestor := r.ancestor()
	opted := r.successionEnabled(ctx)
	if opted && r.Client == "claude" && session != "" && ancestor.PID > 0 && ancestor.StartMS > 0 {
		// A fresh Claude --continue shim can see a native-looking alias after the real
		// session ended. A process-scoped fallback lets Choice A check that history.
		if err := r.fallbackGenerationError(ctx, session, opted); err != nil {
			return Bound{}, err
		}
		return r.register(ctx, fmt.Sprintf("mcp-%d-%d", ancestor.PID, ancestor.StartMS), ancestor, true, 3, opted)
	}
	switch {
	case session != "":
		return r.register(ctx, session, ancestor, false, 2, opted)
	case ancestor.PID > 0 && ancestor.StartMS > 0 && proc.SingleSession(ancestor, r.Client):
		return r.register(ctx, fmt.Sprintf("mcp-%d-%d", ancestor.PID, ancestor.StartMS), ancestor, true, 3, opted)
	}
	return b, err
}

// register makes an Execution of this process's own, which it gives up as soon as the hook's
// appears (see adopt).
func (r *Resolver) register(ctx context.Context, session string, a proc.Ancestor, single bool, rung int, opted bool) (Bound, error) {
	if err := r.fallbackGenerationError(ctx, session, opted); err != nil {
		return Bound{}, err
	}
	req := hubclient.SessionRequest{Client: r.Client, SessionID: session, Host: r.Host, CWD: r.CWD, Source: "mcp", ClientPID: a.PID, ClientPIDStartMS: a.StartMS, SingleSession: single, SuccessionProof: opted}
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
			if err := r.fallbackGenerationError(ctx, session, opted); err != nil {
				return Bound{}, err
			}
			response, err = r.Hub.RegisterSession(ctx, req)
			// Said only once the legacy registration it reports has happened.
			r.olderHub = r.olderHub || err == nil
		}
	}
	if err != nil {
		return Bound{}, err
	}
	b, err := boundFromCandidate(response.Candidate(), session, rung)
	if err != nil {
		return Bound{}, err
	}
	if err := r.admitHeld(ctx, b, false, opted, sessionproof.Snapshot{}); err != nil {
		return Bound{}, err
	}
	r.bound, r.own = b, true
	return b, nil
}

func (r *Resolver) fallbackGenerationError(ctx context.Context, session string, opted bool) error {
	snap, err := r.inspectEvidence(ctx, session, opted)
	gen, ended := snap.GenerationFloor(), snap.Ended
	if max(gen, ended, r.processGenerationHeld(session)) > 0 {
		return r.refuseBindingGeneration(session, gen > 0, ended > 0)
	}
	return err
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
	opted := r.successionEnabled(ctx)
	p := r.pending
	if p == nil {
		mine := r.bound
		b, err := r.resolve(ctx, "", mine.ExecutionID)
		if err != nil || r.wasLostHeld(b.ExecutionID) || (opted || b.Classified) && !b.HookSeen {
			return
		}
		p = &handover{from: mine, to: b}
	}
	// Recheck before selecting a new destination or retrying an unconfirmed end.
	err := r.admitHeld(ctx, p.to, false, opted, sessionproof.Snapshot{})
	if opted && err != nil {
		return
	}
	if r.pending == nil {
		r.keepHeld(p.to)
		r.own = false
		r.pending = p
	}
	if err != nil {
		if err == errGenerationFloor {
			r.dropHeld(p.to)
			r.pending = nil
		}
		return
	}
	if err := r.Hub.HandOverExecution(ctx, p.from.ExecutionID, p.from.Generation, p.to.ExecutionID); err == nil {
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

func (r *Resolver) envSession() string {
	if variable := sessionVariable[r.Client]; variable != "" && r.Getenv != nil {
		return r.Getenv(variable)
	}
	return ""
}

// resolve tries the rungs in order. An opted native binding can recover from
// local memory before trying the process rung. exclude limits adoption to lookup.
func (r *Resolver) resolve(ctx context.Context, handle, exclude string) (Bound, error) {
	req := map[string]any{"client": r.Client, "host": r.Host, "cwd": r.CWD}
	opted := r.successionEnabled(ctx)
	if exclude != "" {
		req["exclude"] = exclude
	}
	session := ""
	var nativeProof sessionproof.Snapshot
	bind := func(rung int) (Bound, error) {
		var named string
		if rung == 2 {
			named = session
		}
		var before sessionproof.Snapshot
		if opted && named != "" {
			var err error
			before, err = r.inspectEvidence(ctx, named, true)
			if err != nil {
				return Bound{}, err
			}
			nativeProof = before
		}
		b, err := r.requestCandidate(ctx, req, named, rung, opted)
		if err != nil {
			return Bound{}, err
		}
		if err := r.admitHeld(ctx, b, handle == "" && exclude == "" && r.shimSessionHeld(b), opted, before); err != nil {
			return Bound{}, err
		}
		return b, nil
	}
	ex := r.Getenv("IRIS_EXECUTION")
	var lastErr error = &hubclient.Error{Code: "unbound", Message: "Pass the execution handle from SessionStart in the execution argument, or restart with IRIS_EXECUTION set. Iris cannot identify this session safely."}
	if r.wasLostHeld(handle) || r.wasLostHeld(ex) {
		lastErr = &hubclient.Error{Code: "unbound", Message: "the execution handle names an Execution already found lost"}
	}
	if r.wasLostHeld(handle) {
		handle = ""
	}
	if ex != "" && !r.wasLostHeld(ex) {
		req["execution"] = ex
		return bind(1)
	}
	// The session's hook is its registrar. On resume Claude Code gives an MCP server another
	// CLAUDE_CODE_SESSION_ID than its hooks, so a session id with nothing bound to it falls
	// through to the client process, which the hook's Execution records.
	if session = r.envSession(); session != "" {
		req["session_id"] = session
		b, err := bind(2)
		if opted && handle == "" && exclude == "" && r.HeartbeatSource != "watch" && tryMemoryRecovery(err, opted) {
			if recovered, attempted, recoveryErr := r.reattachByMemoryHeld(ctx, session, 2, nativeProof); attempted {
				if IsCode(recoveryErr, "unbound") {
					recoveryErr = &LostError{Cause: recoveryErr}
				}
				return recovered, recoveryErr
			}
		}
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
	if err := r.clientMismatchError(); err != nil {
		return err
	}
	r.mu.Lock()
	b := r.bound
	r.mu.Unlock()
	if b.ExecutionID == "" {
		return nil
	}
	return r.beatOne(ctx, b, source)
}

func (r *Resolver) sendHeartbeat(ctx context.Context, b Bound, source string, opted bool) (hubclient.Candidate, error) {
	req := hubclient.HeartbeatRequest{Generation: b.Generation, Source: source, SuccessionProof: opted}
	if opted && b.Rung == 3 {
		return SendProcessHeartbeat(ctx, r.Hub, b.ExecutionID, req)
	}
	return r.Hub.Heartbeat(ctx, b.ExecutionID, req)
}

// RenewPrimary renews the binding resolved for whoami. A concurrent selection
// of another binding invalidates the result even when the heartbeat committed.
func (r *Resolver) RenewPrimary(ctx context.Context, b Bound) (Bound, error) {
	return r.renew(ctx, b, "shim", true)
}

func (r *Resolver) beatOne(ctx context.Context, b Bound, source string) error {
	_, err := r.renew(ctx, b, source, false)
	return err
}

func (r *Resolver) renew(ctx context.Context, b Bound, source string, primary bool) (Bound, error) {
	call, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	opted := r.successionEnabled(call)
	r.mu.Lock()
	if primary && r.bound != b || r.wasLostHeld(b.ExecutionID) {
		r.mu.Unlock()
		return Bound{}, errLost
	}
	if r.bound.ExecutionID == b.ExecutionID && r.bound.Generation == b.Generation {
		b = r.bound
	}
	allowHookless := source == "shim" && r.shimSessionHeld(b)
	before, err := r.inspectEvidence(call, b.SessionID, opted)
	if err == nil {
		err = r.checkCandidate(b, r.processGenerationHeld(b.SessionID), r.processRungHeld(b) > 0, allowHookless, opted, before)
	}
	if err != nil {
		r.markGenerationLossHeld(b, err, opted)
		r.mu.Unlock()
		return Bound{}, err
	}
	r.mu.Unlock()

	candidate, err := r.sendHeartbeat(call, b, source, opted)
	r.mu.Lock()
	defer r.mu.Unlock()
	current := r.bound.ExecutionID == b.ExecutionID && r.bound.Generation == b.Generation
	if primary && r.bound != b || r.wasLostHeld(b.ExecutionID) {
		return Bound{}, errLost
	}
	if hubLost(err) {
		if opted && b.Rung == 3 {
			if current {
				fresh, recoveryErr := r.reattachRung3Held(call, b, before)
				if recoveryErr != nil {
					return Bound{}, recoveryErr
				}
				r.keepHeld(fresh)
				if primary && (fresh.ExecutionID != b.ExecutionID || fresh.Generation != b.Generation) {
					return Bound{}, errLost
				}
				return fresh, nil
			}
			return Bound{}, err
		}
		r.markLostHeld(b, opted)
	}
	if err != nil {
		return Bound{}, err
	}
	if current {
		b = r.bound
	}
	fresh, err := mergeCandidate(b, candidate)
	if err != nil {
		return Bound{}, err
	}
	if err := r.admitHeld(call, fresh, allowHookless, opted, before); err != nil {
		r.markGenerationLossHeld(b, err, opted)
		return Bound{}, err
	}
	if current {
		r.keepHeld(fresh)
	}
	return fresh, nil
}

// Maintain stops before any renewal after parent loss. The process owner also cancels
// this context when the transport reaches EOF. A generation the heartbeat finds lost is
// dropped, never renewed, and the process keeps running. A shim's next tick looks for the
// session's current Execution, as a call would (ADR 027); the watch's own loop and a call's
// Resolve bind afresh too.
func (r *Resolver) Maintain(ctx context.Context, source string, interval time.Duration, parentAlive func() bool, onStop func()) {
	if r.mismatch != "" || r.unidentified {
		return
	}
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
		if r.pending != nil && !r.wasLostHeld(r.pending.from.ExecutionID) {
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
				if rb, attempted, rerr := r.reattachByMemoryHeld(ctx, session, rung, sessionproof.Snapshot{}); attempted {
					b, err = rb, rerr
				}
			}
		}
	}
	if err != nil {
		return false
	}
	if r.wasLostHeld(b.ExecutionID) {
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
