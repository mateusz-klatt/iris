package binding

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/proc"
)

// callHub answers lookups by session from sessions, the Execution a hook registered for each
// session id; any other session is unbound, and one in ambiguous is ambiguous. A refresh or a
// heartbeat of an Execution in lost answers lost. One in refused answers a refresh with lost
// until a heartbeat reattaches it, as after a hub restart. Every lookup, refresh, heartbeat and
// registration is recorded.
type callHub struct {
	mu         sync.Mutex
	sessions   map[string]string
	ambiguous  map[string]bool
	lost       map[string]bool
	refused    map[string]bool
	lookups    []string
	refreshes  []string
	beats      []string
	registered int
}

// hubSeen is what a callHub recorded, copied under its lock.
type hubSeen struct {
	lookups, refreshes, beats []string
	registered                int
}

func (h *callHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req map[string]any
	_ = json.NewDecoder(r.Body).Decode(&req)
	ex, code := h.answer(r.URL.Path, req)
	if code != "" {
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": code, "message": "the hub says " + code}})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"execution_id": ex, "generation": 1})
}

func (h *callHub) answer(path string, req map[string]any) (string, string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	switch {
	case strings.HasSuffix(path, "/heartbeat"):
		ex := strings.TrimSuffix(strings.TrimPrefix(path, "/v1/executions/"), "/heartbeat")
		h.beats = append(h.beats, ex)
		delete(h.refused, ex)
		if h.lost[ex] {
			return "", "lost"
		}
		return ex, ""
	case path == "/v1/sessions":
		h.registered++
		return "ex_registered", ""
	}
	if ex, _ := req["execution"].(string); ex != "" {
		h.refreshes = append(h.refreshes, ex)
		if h.lost[ex] || h.refused[ex] {
			return "", "lost"
		}
		return ex, ""
	}
	session, _ := req["session_id"].(string)
	h.lookups = append(h.lookups, session)
	switch {
	case h.ambiguous[session]:
		return "", "ambiguous"
	case h.sessions[session] == "":
		return "", "unbound"
	}
	return h.sessions[session], ""
}

func (h *callHub) with(change func(h *callHub)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	change(h)
}

func (h *callHub) seen() hubSeen {
	h.mu.Lock()
	defer h.mu.Unlock()
	return hubSeen{append([]string(nil), h.lookups...), append([]string(nil), h.refreshes...), append([]string(nil), h.beats...), h.registered}
}

// observations collects what a resolver observed.
type observations struct {
	mu    sync.Mutex
	texts []string
}

func (o *observations) add(text string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.texts = append(o.texts, text)
}

func (o *observations) list() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.texts...)
}

// callResolver is a Codex shim's resolver against h, with env for its environment and no client
// process to find.
func callResolver(t *testing.T, h *callHub, env map[string]string) (*Resolver, *observations) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	seen := &observations{}
	return &Resolver{
		Hub:    &hubclient.Client{Config: config{srv.URL}},
		Client: "codex", Host: "host", CWD: t.TempDir(),
		Getenv:   func(k string) string { return env[k] },
		Ancestor: func() (proc.Ancestor, error) { return proc.Ancestor{}, nil },
		Observe:  seen.add,
	}, seen
}

// decidedMode names the mode r's calls decided, or "undecided".
func decidedMode(r *Resolver) string {
	m := r.decided.Load()
	if m == nil {
		return "undecided"
	}
	return [...]string{"legacy", "root", "subagent"}[m.mode]
}

// maintainUntil runs Maintain on short ticks until done says so. done is asked at the start of
// each tick, with the number of ticks so far, before that tick renews anything, so when it says
// so every earlier tick has finished. maintainUntil returns once Maintain has stopped.
func maintainUntil(t *testing.T, r *Resolver, done func(ticks int) bool) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reached := make(chan struct{})
	var once sync.Once
	stopped := make(chan struct{})
	ticks := 0
	go func() {
		defer close(stopped)
		r.Maintain(ctx, "shim", time.Millisecond, func() bool {
			ticks++
			if done(ticks) {
				once.Do(func() { close(reached) })
			}
			return true
		}, func() { t.Error("stopped as an orphan") })
	}()
	select {
	case <-reached:
	case <-time.After(10 * time.Second):
		t.Fatal("Maintain never got there")
	}
	cancel()
	<-stopped
}

func renewed(h *callHub) func(int) bool {
	return func(int) bool { return len(h.seen().beats) > 0 }
}

func lost(r *Resolver) func(int) bool {
	return func(int) bool { return r.Lost() }
}

// isText says err is an unbound answer with exactly want's text.
func isText(err error, want *hubclient.Error) bool {
	var got *hubclient.Error
	return errors.As(err, &got) && got.Code == "unbound" && got.Message == want.Message
}

// A live IRIS_EXECUTION binds every call, whatever session the call names: it is the person's
// explicit word. Once a heartbeat finds it lost, the next call decides the mode by its id.
func TestALiveOverrideBindsEveryCallWhateverItNames(t *testing.T) {
	h := &callHub{sessions: map[string]string{"other": "ex_other"}, lost: map[string]bool{}}
	r, seen := callResolver(t, h, map[string]string{"IRIS_EXECUTION": "ex_h"})
	ctx := context.Background()
	for _, session := range []string{"unregistered", "other"} {
		b, primary, err := r.ResolveCall(ctx, "", session, session)
		if err != nil || b.ExecutionID != "ex_h" || b.Rung != 1 || !primary {
			t.Fatalf("%s: %+v primary %v %v", session, b, primary, err)
		}
	}
	if got := h.seen(); len(got.lookups) != 0 || decidedMode(r) != "undecided" {
		t.Fatalf("an override's calls were looked up by session: %v, mode %s", got.lookups, decidedMode(r))
	}
	maintainUntil(t, r, renewed(h))
	if got := h.seen(); got.beats[0] != "ex_h" {
		t.Fatalf("renewed %v", got.beats)
	}
	h.with(func(h *callHub) { h.lost["ex_h"] = true })
	maintainUntil(t, r, lost(r))
	r.Grace = time.Hour // after a loss nothing waits, however long the grace period
	b, primary, err := r.ResolveCall(ctx, "", "other", "other")
	if err != nil || b.ExecutionID != "ex_other" || b.Rung != 2 || !primary || decidedMode(r) != "root" || r.Lost() {
		t.Fatalf("%+v primary %v %v mode %s lost %v", b, primary, err, decidedMode(r), r.Lost())
	}
	if len(seen.list()) != 0 {
		t.Fatalf("observed %v", seen.list())
	}
}

// Once the override is known lost, it never wins again, and a session no hook registered gets
// decision 7's text at once. Only a first bind's grace period, or a lookup-only call, is observed.
func TestAKnownGoneOverrideNeverWins(t *testing.T) {
	h := &callHub{}
	r, seen := callResolver(t, h, map[string]string{"IRIS_EXECUTION": "ex_h"})
	r.gone, r.lost, r.Grace = map[string]bool{"ex_h": true}, true, time.Hour
	_, primary, err := r.ResolveCall(context.Background(), "", "s", "s")
	if !isText(err, errUnusable) || !primary || decidedMode(r) != "root" {
		t.Fatalf("%v primary %v mode %s", err, primary, decidedMode(r))
	}
	if got := h.seen(); len(got.refreshes) != 0 || len(got.lookups) != 1 {
		t.Fatalf("asked about the lost override, or waited: %+v", got)
	}
	if len(seen.list()) != 0 {
		t.Fatalf("observed %v", seen.list())
	}
}

// A first call that names no session leaves the process to today's chain, which uses no call's
// id: a handle, and today's text, which advises one, are still what count.
func TestAFirstCallWithoutAnIdKeepsTodaysChain(t *testing.T) {
	h := &callHub{sessions: map[string]string{"s": "ex_s"}}
	r, _ := callResolver(t, h, nil)
	ctx := context.Background()
	_, primary, err := r.ResolveCall(ctx, "", "", "")
	if !IsCode(err, "unbound") || !strings.Contains(err.Error(), "Pass the execution handle from SessionStart") || !primary || decidedMode(r) != "legacy" {
		t.Fatalf("%v primary %v mode %s", err, primary, decidedMode(r))
	}
	r.Note("s")
	b, _, err := r.ResolveCall(ctx, "ex_handle", "s", "s")
	if err != nil || b.ExecutionID != "ex_handle" || b.Rung != 4 {
		t.Fatalf("%+v %v", b, err)
	}
	if got := h.seen(); len(got.lookups) != 0 || r.seen != "s" {
		t.Fatalf("legacy mode looked a call's id up: %v; noted %q", got.lookups, r.seen)
	}
}

// A root's first bind waits for the session's hook, which usually registers it within the grace
// period, and binds nothing else meanwhile: no other session, no client process, no registration.
func TestARootBindsItsSessionOnceTheHookRegistersIt(t *testing.T) {
	h := &callHub{sessions: map[string]string{"other": "ex_other"}}
	r, seen := callResolver(t, h, nil)
	r.Grace = time.Minute
	r.Ancestor = func() (proc.Ancestor, error) {
		t.Error("id mode consulted the client process")
		return proc.Ancestor{}, nil
	}
	go func() {
		for len(h.seen().lookups) < 2 {
			time.Sleep(time.Millisecond)
		}
		h.with(func(h *callHub) { h.sessions["s"] = "ex_s" })
	}()
	b, primary, err := r.ResolveCall(context.Background(), "", "s", "s")
	if err != nil || b.ExecutionID != "ex_s" || b.Rung != 2 || !primary || r.bound != b {
		t.Fatalf("%+v primary %v %v", b, primary, err)
	}
	got := h.seen()
	for _, session := range got.lookups {
		if session != "s" {
			t.Fatalf("looked up %q while binding s", session)
		}
	}
	if got.registered != 0 || len(seen.list()) != 0 {
		t.Fatalf("registered %d, observed %v", got.registered, seen.list())
	}
}

// When the grace period runs out, the call gets decision 7's text, which names no cause, whether
// no hook registered the session or the hub found two; the process is observed once. A hook that
// registers later is found by the next call.
func TestAGracePeriodThatRunsOutIsAnsweredWithoutACause(t *testing.T) {
	h := &callHub{ambiguous: map[string]bool{"twice": true}}
	ctx := context.Background()
	for _, session := range []string{"never", "twice"} {
		r, seen := callResolver(t, h, nil)
		for range 2 {
			if _, primary, err := r.ResolveCall(ctx, "", session, session); !isText(err, errUnusable) || !primary {
				t.Fatalf("%s: %v primary %v", session, err, primary)
			}
		}
		if got := seen.list(); len(got) != 1 || got[0] != ObservedUnusable {
			t.Fatalf("%s: observed %v", session, got)
		}
	}
	r, _ := callResolver(t, h, nil)
	_, _, _ = r.ResolveCall(ctx, "", "late", "late")
	h.with(func(h *callHub) { h.sessions = map[string]string{"late": "ex_late"} })
	if b, _, err := r.ResolveCall(ctx, "", "late", "late"); err != nil || b.ExecutionID != "ex_late" {
		t.Fatalf("%+v %v", b, err)
	}
	if h.seen().registered != 0 {
		t.Fatal("id mode registered an Execution")
	}
}

// A call that found the process undecided, while another call decided and then took the lock it
// holds through its first bind, does not wait for that lock: it reads the decision again first,
// and a call that names another session is then looked up at once.
func TestACallThatFoundTheModeUndecidedDoesNotWaitBehindTheFirstBind(t *testing.T) {
	h := &callHub{sessions: map[string]string{"other": "ex_other"}}
	r, _ := callResolver(t, h, nil)
	r.decided.Store(decide("s", "s")) // another call decided for a root's process
	r.mu.Lock()                       // and holds mu through its first bind's grace period
	defer r.mu.Unlock()
	rechecked := make(chan error, 1)
	go func() {
		// This call read the decision before it was made, so it takes the undecided step.
		_, overridden, err := r.overrideOrDecide(context.Background(), "", "other", "other")
		if overridden {
			err = errors.New("an absent override bound the call")
		}
		rechecked <- err
	}()
	select {
	case err := <-rechecked:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the undecided step waited for the first bind's lock")
	}
	if b, primary, err := r.ResolveCall(context.Background(), "", "other", "other"); err != nil || b.ExecutionID != "ex_other" || primary {
		t.Fatalf("%+v primary %v %v", b, primary, err)
	}
}

// A call that ends while a lookup of its first bind is still in flight says Iris is still
// binding, as one that ends between two lookups does, and is not observed.
func TestACallThatEndsDuringALookupOfItsFirstBindIsStillBinding(t *testing.T) {
	var lookups atomic.Int32
	inFlight := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The server notices a client that hung up only once it has read the request.
		_, _ = io.Copy(io.Discard, r.Body)
		if lookups.Add(1) > 1 {
			once.Do(func() { close(inFlight) })
			select {
			case <-r.Context().Done():
			case <-release:
			}
			return
		}
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":{"code":"unbound","message":"no positive binding"}}`))
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) }) // runs first: a stalled handler never holds up Close
	r, seen := callResolver(t, &callHub{}, nil)
	r.Hub = &hubclient.Client{Config: config{srv.URL}}
	r.Grace = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, _, err := r.ResolveCall(ctx, "", "s", "s")
		done <- err
	}()
	<-inFlight
	cancel()
	if err := <-done; err != errBinding {
		t.Fatal(err)
	}
	if len(seen.list()) != 0 {
		t.Fatalf("observed %v", seen.list())
	}
}

// A call that ends while the first bind still waits says so, as today, and is not observed: only
// the binding step's own end without an Execution is.
func TestACallThatEndsDuringTheGracePeriodIsStillBinding(t *testing.T) {
	r, seen := callResolver(t, &callHub{}, nil)
	r.Grace = time.Hour
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, _, err := r.ResolveCall(ctx, "", "s", "s"); err != errBinding {
		t.Fatal(err)
	}
	if len(seen.list()) != 0 {
		t.Fatalf("observed %v", seen.list())
	}
}

// Across a hub restart a bound root keeps its Execution and generation: the refusals pass
// through, nothing is looked up by id, and the heartbeat is what reattaches the session.
func TestARootKeepsItsBindingUntilTheHeartbeatReattachesIt(t *testing.T) {
	h := &callHub{sessions: map[string]string{"s": "ex_s"}}
	r, _ := callResolver(t, h, nil)
	ctx := context.Background()
	if _, _, err := r.ResolveCall(ctx, "", "s", "s"); err != nil {
		t.Fatal(err)
	}
	h.with(func(h *callHub) { h.refused = map[string]bool{"ex_s": true} })
	if _, _, err := r.ResolveCall(ctx, "", "s", "s"); !IsCode(err, "lost") {
		t.Fatal(err)
	}
	if got := h.seen(); r.bound.ExecutionID != "ex_s" || r.bound.Generation != 1 || len(got.lookups) != 1 {
		t.Fatalf("binding %+v, lookups %v", r.bound, got.lookups)
	}
	maintainUntil(t, r, renewed(h))
	if b, _, err := r.ResolveCall(ctx, "", "s", "s"); err != nil || b.ExecutionID != "ex_s" || len(h.seen().lookups) != 1 {
		t.Fatalf("%+v %v lookups %v", b, err, h.seen().lookups)
	}
}

// After a heartbeat confirms the loss, the next call looks the session up again at once and binds
// its new Execution. One found lost is never bound again.
func TestAfterALossARootLooksItsSessionUpAgain(t *testing.T) {
	h := &callHub{sessions: map[string]string{"s": "ex_s"}, lost: map[string]bool{}}
	r, seen := callResolver(t, h, nil)
	ctx := context.Background()
	if _, _, err := r.ResolveCall(ctx, "", "s", "s"); err != nil {
		t.Fatal(err)
	}
	h.with(func(h *callHub) { h.lost["ex_s"] = true })
	maintainUntil(t, r, lost(r))
	r.Grace = time.Hour
	// The hub still names the lost Execution: it is not bound again.
	if _, _, err := r.ResolveCall(ctx, "", "s", "s"); !isText(err, errUnusable) || !r.Lost() {
		t.Fatalf("%v lost %v", err, r.Lost())
	}
	h.with(func(h *callHub) { h.sessions["s"] = "ex_s2" })
	b, _, err := r.ResolveCall(ctx, "", "s", "s")
	if err != nil || b.ExecutionID != "ex_s2" || r.Lost() || len(h.seen().lookups) != 3 {
		t.Fatalf("%+v %v lost %v lookups %v", b, err, r.Lost(), h.seen().lookups)
	}
	if len(seen.list()) != 0 {
		t.Fatalf("observed %v", seen.list())
	}
}

// A call that names another session is attributed by one lookup outside the lock a first bind
// holds while it waits: it returns first, renews nothing and changes nothing of the root's. A
// call is noted, and a second session observed, before the call is decoded or bound.
func TestALookupOnlyCallNeverWaitsBehindTheRootsGracePeriod(t *testing.T) {
	h := &callHub{sessions: map[string]string{"other": "ex_other"}}
	r, seen := callResolver(t, h, nil)
	r.Grace = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	waiting := make(chan error, 1)
	r.Note("s")
	go func() {
		_, _, err := r.ResolveCall(ctx, "", "s", "s")
		waiting <- err
	}()
	for len(h.seen().lookups) == 0 {
		time.Sleep(time.Millisecond)
	}
	r.Note("other")
	b, primary, err := r.ResolveCall(context.Background(), "", "other", "other")
	if err != nil || b.ExecutionID != "ex_other" || b.Rung != 2 || primary {
		t.Fatalf("%+v primary %v %v", b, primary, err)
	}
	if got := seen.list(); len(got) != 1 || got[0] != ObservedTwoSessions {
		t.Fatalf("observed %v", got)
	}
	select {
	case err := <-waiting:
		t.Fatalf("the root returned first: %v", err)
	default:
	}
	cancel()
	if err := <-waiting; err != errBinding {
		t.Fatal(err)
	}
	if r.bound.ExecutionID != "" || r.lost || len(r.gone) != 0 || r.since.IsZero() || len(h.seen().beats) != 0 {
		t.Fatalf("the lookup changed the root's state: %+v lost %v gone %v since %v", r.bound, r.lost, r.gone, r.since)
	}
	// A third session is not a new observation.
	r.Note("third")
	if len(seen.list()) != 1 {
		t.Fatalf("observed %v", seen.list())
	}
}

// A sub-agent's calls carry the root's session and their own thread. Its process looks every
// call up and renews nothing, so the root's lease stays its own process's and hooks' to keep.
func TestASubagentsProcessLooksEveryCallUpAndRenewsNothing(t *testing.T) {
	h := &callHub{sessions: map[string]string{"root": "ex_root"}}
	r, _ := callResolver(t, h, nil)
	ctx := context.Background()
	b, primary, err := r.ResolveCall(ctx, "", "root", "child")
	if err != nil || b.ExecutionID != "ex_root" || b.Rung != 2 || primary || decidedMode(r) != "subagent" || r.bound.ExecutionID != "" {
		t.Fatalf("%+v primary %v %v mode %s bound %+v", b, primary, err, decidedMode(r), r.bound)
	}
	// Five whole ticks find nothing to renew; the root re-registers and the next call binds the
	// new one.
	maintainUntil(t, r, func(ticks int) bool { return ticks > 5 })
	h.with(func(h *callHub) { h.sessions["root"] = "ex_root2" })
	if b, _, err = r.ResolveCall(ctx, "", "root", "child"); err != nil || b.ExecutionID != "ex_root2" {
		t.Fatalf("%+v %v", b, err)
	}
	if _, primary, err = r.ResolveCall(ctx, "ex_root2", "", ""); err != errNoSession || primary {
		t.Fatalf("%v primary %v", err, primary)
	}
	if got := h.seen(); len(got.beats) != 0 || len(got.refreshes) != 0 || got.registered != 0 {
		t.Fatalf("%+v", got)
	}
}

// In id mode the execution argument changes nothing: a call that names its session binds by it;
// one that names none uses a root's binding, and in a sub-agent's process is unbound.
func TestHandlesNeverChangeAttributionInIdMode(t *testing.T) {
	h := &callHub{sessions: map[string]string{"s": "ex_s"}}
	ctx := context.Background()
	rootR, _ := callResolver(t, h, nil)
	if b, _, err := rootR.ResolveCall(ctx, "ex_other", "s", "s"); err != nil || b.ExecutionID != "ex_s" {
		t.Fatalf("%+v %v", b, err)
	}
	if b, primary, err := rootR.ResolveCall(ctx, "ex_other", "", ""); err != nil || b.ExecutionID != "ex_s" || !primary {
		t.Fatalf("%+v primary %v %v", b, primary, err)
	}
	subR, _ := callResolver(t, h, nil)
	if b, _, err := subR.ResolveCall(ctx, "ex_other", "s", "t"); err != nil || b.ExecutionID != "ex_s" {
		t.Fatalf("%+v %v", b, err)
	}
	if _, _, err := subR.ResolveCall(ctx, "ex_other", "", ""); err != errNoSession {
		t.Fatal(err)
	}
	for _, ex := range h.seen().refreshes {
		if ex == "ex_other" {
			t.Fatal("a handle was asked about")
		}
	}
}

// A root's process with no binding yet binds its own session for a call that names none.
func TestACallWithoutAnIdBindsTheRootsSession(t *testing.T) {
	r, _ := callResolver(t, &callHub{sessions: map[string]string{"s": "ex_s"}}, nil)
	r.decided.Store(decide("s", "s"))
	if b, primary, err := r.ResolveCall(context.Background(), "", "", ""); err != nil || b.ExecutionID != "ex_s" || !primary {
		t.Fatalf("%+v primary %v %v", b, primary, err)
	}
}

// A lookup-only call that finds no single usable Execution gets decision 7's text and is observed
// once; any other failure, such as a hub that cannot be reached, passes through.
func TestALookupOnlyFailureIsAnsweredWithoutACause(t *testing.T) {
	h := &callHub{sessions: map[string]string{"s": "ex_s"}, ambiguous: map[string]bool{"twice": true}}
	r, seen := callResolver(t, h, nil)
	ctx := context.Background()
	if _, _, err := r.ResolveCall(ctx, "", "s", "s"); err != nil {
		t.Fatal(err)
	}
	for _, session := range []string{"never", "twice"} {
		if _, primary, err := r.ResolveCall(ctx, "", session, session); !isText(err, errUnusable) || primary {
			t.Fatalf("%s: %v primary %v", session, err, primary)
		}
	}
	if got := seen.list(); len(got) != 1 || got[0] != ObservedUnusable {
		t.Fatalf("observed %v", got)
	}
	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close()
	r.Hub = &hubclient.Client{Config: config{gone.URL}}
	if _, _, err := r.ResolveCall(ctx, "", "other", "other"); err == nil || IsCode(err, "unbound") {
		t.Fatalf("an unreachable hub gave %v", err)
	}
	// So does a root's own lookup.
	fresh, seenFresh := callResolver(t, h, nil)
	fresh.Hub = r.Hub
	if _, primary, err := fresh.ResolveCall(ctx, "", "s", "s"); err == nil || IsCode(err, "unbound") || !primary || len(seenFresh.list()) != 0 {
		t.Fatalf("an unreachable hub gave %v primary %v observed %v", err, primary, seenFresh.list())
	}
}

// A session id is the client's opaque word, matched exactly; a thread id that is not the
// session's, a missing one among them, makes a sub-agent's process.
func TestSessionIdsAreOpaqueAndMatchedExactly(t *testing.T) {
	h := &callHub{sessions: map[string]string{"session-1": "ex_1"}}
	ctx := context.Background()
	r, _ := callResolver(t, h, nil)
	if b, _, err := r.ResolveCall(ctx, "", "session-1", "session-1"); err != nil || b.ExecutionID != "ex_1" {
		t.Fatalf("%+v %v", b, err)
	}
	other, _ := callResolver(t, h, nil)
	if _, _, err := other.ResolveCall(ctx, "", "Session-1", "Session-1"); !isText(err, errUnusable) {
		t.Fatal(err)
	}
	missing, _ := callResolver(t, h, nil)
	if _, primary, err := missing.ResolveCall(ctx, "", "session-1", ""); err != nil || primary || decidedMode(missing) != "subagent" {
		t.Fatalf("%v primary %v mode %s", err, primary, decidedMode(missing))
	}
}

// Note keeps the first session and notices the first other one, once; a call without a session
// is not noted, and without a recorder an observation is dropped.
func TestNoteObservesASecondSessionOnce(t *testing.T) {
	r, seen := callResolver(t, &callHub{}, nil)
	r.Note("")
	r.Note("a")
	r.Note("a")
	if r.seen != "a" || len(seen.list()) != 0 {
		t.Fatalf("seen %q observed %v", r.seen, seen.list())
	}
	r.Note("b")
	r.Note("c")
	if r.seen != "a" || !r.twoSeen || len(seen.list()) != 1 {
		t.Fatalf("seen %q two %v observed %v", r.seen, r.twoSeen, seen.list())
	}
	quiet := &Resolver{}
	quiet.Note("a")
	quiet.Note("b")
	quiet.noteUnusable()
	if !quiet.twoSeen || !quiet.unusable.Load() {
		t.Fatal("an observation without a recorder was not taken as made")
	}
}

// An observation is written outside every lock: while one blocks, other calls are noted and
// bound.
func TestObservationsHoldNoLock(t *testing.T) {
	h := &callHub{sessions: map[string]string{"a": "ex_a"}}
	r, _ := callResolver(t, h, nil)
	entered := make(chan string, 2)
	release := make(chan struct{})
	defer close(release)
	r.Observe = func(text string) {
		entered <- text
		<-release
	}
	ctx := context.Background()
	r.Note("a")
	go r.Note("b")
	if got := <-entered; got != ObservedTwoSessions {
		t.Fatal(got)
	}
	r.Note("c")
	if _, _, err := r.ResolveCall(ctx, "", "a", "a"); err != nil {
		t.Fatal(err)
	}
	go func() { _, _, _ = r.ResolveCall(ctx, "", "unknown", "unknown") }()
	if got := <-entered; got != ObservedUnusable {
		t.Fatal(got)
	}
	if b, _, err := r.ResolveCall(ctx, "", "a", "a"); err != nil || b.ExecutionID != "ex_a" {
		t.Fatalf("%+v %v", b, err)
	}
}
