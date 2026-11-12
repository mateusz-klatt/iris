package binding

import (
	"context"
	"errors"
	"testing"

	"github.com/mateusz-klatt/iris/internal/hubclient"
)

func wakeRoot(t *testing.T, env map[string]string) (*Resolver, *callHub, WakeTarget) {
	t.Helper()
	h := &callHub{sessions: map[string]string{"session": "ex_root"}, lost: map[string]bool{}}
	r, _ := callResolver(t, h, env)
	b, primary, err := r.ResolveCall(context.Background(), "", "session", "session")
	if err != nil || !primary {
		t.Fatalf("root binding: %+v, primary %v, %v", b, primary, err)
	}
	want := WakeTarget{SessionID: "session", Bound: b, Host: "host"}
	if got, ok := r.Primary(); !ok || got != want {
		t.Fatalf("root wake target: %+v, %v; want %+v", got, ok, want)
	}
	return r, h, want
}

func noWakeTarget(t *testing.T, r *Resolver) {
	t.Helper()
	if got, ok := r.Primary(); ok || got != (WakeTarget{}) {
		t.Fatalf("ineligible process has a wake target: %+v, %v", got, ok)
	}
	if started, err := r.StartIfPrimary(WakeTarget{}, func() error {
		t.Error("ineligible process started a command")
		return nil
	}); started || err != nil {
		t.Fatalf("ineligible admission: started %v, %v", started, err)
	}
}

func TestWakeRequiresAPositiveCodexRootBinding(t *testing.T) {
	for _, tc := range []struct {
		name, session, thread, handle, override string
		client                                  string
	}{
		{name: "undecided"},
		{name: "legacy", handle: "ex_handle"},
		{name: "subagent", session: "session", thread: "child"},
		{name: "missing thread", session: "session"},
		{name: "unbound root", session: "missing", thread: "missing"},
		{name: "live override", session: "session", thread: "session", override: "ex_override"},
		{name: "other client", session: "session", thread: "session", client: "claude"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &callHub{sessions: map[string]string{"session": "ex_root"}}
			r, _ := callResolver(t, h, map[string]string{"IRIS_EXECUTION": tc.override})
			if tc.client != "" {
				r.Client = tc.client
			}
			if tc.name != "undecided" {
				b, primary, err := r.ResolveCall(context.Background(), tc.handle, tc.session, tc.thread)
				if tc.name == "unbound root" {
					if !IsCode(err, "unbound") {
						t.Fatalf("unbound root: %v", err)
					}
				} else if err != nil || b.ExecutionID == "" {
					t.Fatalf("test did not establish its binding: %+v, %v", b, err)
				}
				if (tc.name == "subagent" || tc.name == "missing thread") && primary {
					t.Fatal("lookup-only call retained a primary binding")
				}
			}
			noWakeTarget(t, r)
		})
	}
}

func TestWakeSnapshotRetainsOriginalSessionAndCopiesBinding(t *testing.T) {
	r, h, want := wakeRoot(t, nil)
	h.with(func(h *callHub) { h.sessions["foreign"] = "ex_foreign" })
	b, primary, err := r.ResolveCall(context.Background(), "", "foreign", "foreign")
	if err != nil || primary || b.ExecutionID != "ex_foreign" {
		t.Fatalf("foreign lookup: %+v, primary %v, %v", b, primary, err)
	}
	got, ok := r.Primary()
	if !ok || got != want {
		t.Fatalf("lookup replaced root target: %+v, %v", got, ok)
	}
	got.Bound.ExecutionID, got.Bound.Generation, got.Bound.Agent.Name = "changed", 77, "changed"
	got.Host, got.SessionID = "elsewhere", "another session"
	if current, ok := r.Primary(); !ok || current != want {
		t.Fatalf("caller mutated resolver through snapshot: %+v, %v", current, ok)
	}
}

func TestWakeDoesNotFollowALostBinding(t *testing.T) {
	r, h, old := wakeRoot(t, nil)
	h.with(func(h *callHub) { h.lost[old.Bound.ExecutionID] = true })
	maintainUntil(t, r, lost(r))
	noWakeTarget(t, r)
	if started, err := r.StartIfPrimary(old, func() error {
		t.Error("lost target started a command")
		return nil
	}); started || err != nil {
		t.Fatalf("lost admission: started %v, %v", started, err)
	}
	h.with(func(h *callHub) { h.sessions[old.SessionID] = "ex_replacement" })
	b, primary, err := r.ResolveCall(context.Background(), "", old.SessionID, old.SessionID)
	if err != nil || !primary || b.ExecutionID != "ex_replacement" {
		t.Fatalf("replacement binding: %+v, primary %v, %v", b, primary, err)
	}
	if got, ok := r.Primary(); !ok || got.Bound != b || got.SessionID != old.SessionID {
		t.Fatalf("replacement target: %+v, %v", got, ok)
	}
	if started, err := r.StartIfPrimary(old, func() error {
		t.Error("replacement admitted the old target")
		return nil
	}); started || err != nil {
		t.Fatalf("stale admission: started %v, %v", started, err)
	}
}

func TestWakeRequiresCurrentPositiveBindingAndHost(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Resolver)
	}{
		{"missing host", func(r *Resolver) { r.Host = "" }},
		{"missing generation", func(r *Resolver) { r.bound.Generation = 0 }},
		{"negative generation", func(r *Resolver) { r.bound.Generation = -1 }},
		{"known gone execution", func(r *Resolver) { r.gone = map[string]bool{r.bound.ExecutionID: true} }},
		{"non-session binding", func(r *Resolver) { r.bound.Rung = 4 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, _, _ := wakeRoot(t, nil)
			r.mu.Lock()
			tc.change(r)
			r.mu.Unlock()
			noWakeTarget(t, r)
		})
	}
}

func TestWakeLiveOverrideDisarmsButKnownLostOverrideDoesNot(t *testing.T) {
	env := map[string]string{}
	r, _, want := wakeRoot(t, env)
	env["IRIS_EXECUTION"] = "ex_override"
	noWakeTarget(t, r)
	r.mu.Lock()
	r.gone = map[string]bool{"ex_override": true}
	r.mu.Unlock()
	if got, ok := r.Primary(); !ok || got != want {
		t.Fatalf("known lost override disarmed root: %+v, %v", got, ok)
	}
}

func TestWakeAdmissionComparesTheWholeTarget(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*WakeTarget)
	}{
		{"session", func(w *WakeTarget) { w.SessionID = "another session" }},
		{"host", func(w *WakeTarget) { w.Host = "another host" }},
		{"execution", func(w *WakeTarget) { w.Bound.ExecutionID = "another execution" }},
		{"generation", func(w *WakeTarget) { w.Bound.Generation++ }},
		{"repository", func(w *WakeTarget) { w.Bound.RepositoryID = "another repository" }},
		{"agent id", func(w *WakeTarget) { w.Bound.Agent.ID = "another agent" }},
		{"agent name", func(w *WakeTarget) { w.Bound.Agent.Name = "another name" }},
		{"rung", func(w *WakeTarget) { w.Bound.Rung = 3 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, _, expected := wakeRoot(t, nil)
			tc.change(&expected)
			if started, err := r.StartIfPrimary(expected, func() error {
				t.Error("mismatching target started a command")
				return nil
			}); started || err != nil {
				t.Fatalf("stale admission: started %v, %v", started, err)
			}
		})
	}
}

func TestWakeAdmissionReportsStartFailureAndUnlocks(t *testing.T) {
	r, _, expected := wakeRoot(t, nil)
	failure := errors.New("cannot start")
	started, err := r.StartIfPrimary(expected, func() error { return failure })
	if started || !errors.Is(err, failure) {
		t.Fatalf("failed start: started %v, %v", started, err)
	}
	if got, ok := r.Primary(); !ok || got != expected {
		t.Fatalf("failed start changed or locked binding: %+v, %v", got, ok)
	}
}

func TestWakeAdmissionSerializesStartWithBindingTransitions(t *testing.T) {
	for _, drop := range []bool{false, true} {
		t.Run(map[bool]string{false: "replacement", true: "loss"}[drop], func(t *testing.T) {
			r, _, expected := wakeRoot(t, nil)
			transitionReady := make(chan struct{})
			transitioned := make(chan struct{})
			started, err := r.StartIfPrimary(expected, func() error {
				// A failed TryLock deterministically proves that no binding transition
				// using mu can run between admission and the start callback.
				if r.mu.TryLock() {
					r.mu.Unlock()
					t.Error("start callback ran without the binding mutex")
				}
				go func() {
					close(transitionReady)
					r.mu.Lock()
					if drop {
						r.bound, r.lost = Bound{}, true
					} else {
						r.bound.Generation++
					}
					r.mu.Unlock()
					close(transitioned)
				}()
				<-transitionReady
				if r.bound != expected.Bound {
					t.Error("binding changed before start")
				}
				return nil
			})
			if !started || err != nil {
				t.Fatalf("positive admission: started %v, %v", started, err)
			}
			<-transitioned
			if started, err := r.StartIfPrimary(expected, func() error {
				t.Error("binding transition admitted the old target")
				return nil
			}); started || err != nil {
				t.Fatalf("overtaken admission: started %v, %v", started, err)
			}
		})
	}
}

func TestWakeCallerWaitDoesNotHoldTheBindingLock(t *testing.T) {
	r, _, expected := wakeRoot(t, nil)
	waiting, finishWait, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		started, err := r.StartIfPrimary(expected, func() error { return nil })
		if !started || err != nil {
			t.Errorf("positive admission: started %v, %v", started, err)
		}
		close(waiting)
		<-finishWait // A caller's command Wait occurs only after StartIfPrimary returns.
	}()
	<-waiting
	if !r.mu.TryLock() {
		t.Error("caller waiting for its command blocked the binding mutex")
	} else {
		r.bound.Agent = hubclient.Agent{ID: "updated", Name: "updated"}
		r.mu.Unlock()
	}
	close(finishWait)
	<-done
	if got, ok := r.Primary(); !ok || got.Bound.Agent.ID != "updated" {
		t.Fatalf("binding could not update during caller Wait: %+v, %v", got, ok)
	}
}
