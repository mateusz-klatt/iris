package wake

import (
	"context"
	"os"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/kimiwake"
	"github.com/mateusz-klatt/iris/internal/proc"
)

// The watcher needs its Kimi process and the session SessionStart published for it. Without
// either there is no record to hold, so it fails, touches no record and calls no hub.
func TestKimiOnceNeedsItsKimiProcessAndSession(t *testing.T) {
	for name, anc := range map[string]proc.Ancestor{
		"another process": {PID: onceKimiPID, StartMS: onceKimiStart, Name: "node"},
		"no start time":   {PID: onceKimiPID, Name: "kimi"},
		"no session":      {PID: 5000, StartMS: 5001, Name: "kimi"},
	} {
		t.Run(name, func(t *testing.T) {
			ot := newOnceTest(t)
			ot.w.nearest = func() (proc.Ancestor, error) { return anc, nil }
			if got := ot.w.run(t.Context()); got != Failed || ot.out.String() != outcomeLines[Failed]+"\n" || len(ot.hub.log()) != 0 {
				t.Fatalf("%v %q %v", got, ot.out, ot.hub.log())
			}
			if _, exists, _ := ot.store.Read(ot.key, time.Second); exists {
				t.Fatal("a record was made")
			}
		})
	}
	ot := newOnceTest(t)
	ot.w.nearest = func() (proc.Ancestor, error) { return proc.Ancestor{}, proc.ErrNotFound }
	if got := ot.w.run(t.Context()); got != Failed {
		t.Fatal(got)
	}
}

// With wake off the watcher ends disabled and latches, so the prompt hook stops asking, but a latch
// never replaces another live watcher.
func TestKimiOnceWithWakeOffLatchesUnlessAWatcherLives(t *testing.T) {
	ot := newOnceTest(t)
	ot.wake("off")
	if got := ot.w.run(t.Context()); got != Disabled || ot.out.String() != outcomeLines[Disabled]+"\n" || len(ot.hub.log()) != 0 {
		t.Fatalf("%v %q %v", got, ot.out, ot.hub.log())
	}
	if r := ot.record(); r.Latch == nil || r.Latch.State != kimiwake.LatchDisabled {
		t.Fatalf("%+v", r)
	}
	ot = newOnceTest(t)
	other := kimiwake.Attempt{PID: 8001, StartMS: 8002, ID: "other"}
	ot.setAlive(other.PID, other.StartMS, true)
	if owned, err := ot.store.Own(ot.key, other, time.Second); err != nil || !owned {
		t.Fatal(owned, err)
	}
	ot.wake("off")
	if got := ot.w.run(t.Context()); got != Disabled {
		t.Fatal(got)
	}
	if r := ot.record(); r.Latch != nil || r.Attempt == nil || r.Attempt.ID != "other" {
		t.Fatalf("%+v", r)
	}
}

// A second watcher for a key a live watcher holds exits as a duplicate. It calls no hub, opens no
// stream, moves no cursor and leaves the record as it was.
func TestKimiOnceDuplicateChangesNothing(t *testing.T) {
	ot := newOnceTest(t)
	other := kimiwake.Attempt{PID: 8001, StartMS: 8002, ID: "other"}
	ot.setAlive(other.PID, other.StartMS, true)
	if owned, err := ot.store.Own(ot.key, other, time.Second); err != nil || !owned {
		t.Fatal(owned, err)
	}
	before := ot.record()
	if got := ot.w.run(t.Context()); got != Duplicate || ot.out.String() != outcomeLines[Duplicate]+"\n" || len(ot.hub.log()) != 0 {
		t.Fatalf("%v %q %v", got, ot.out, ot.hub.log())
	}
	if after := ot.record(); after.Attempt == nil || *after.Attempt != *before.Attempt || after.Checkpoint != before.Checkpoint {
		t.Fatalf("%+v", after)
	}
}

// A record that cannot be read cannot be owned, and the watcher fails without a hub call.
func TestKimiOnceFailsOnARecordItCannotOwn(t *testing.T) {
	ot := newOnceTest(t)
	if err := os.WriteFile(ot.recordPath(), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := ot.w.run(t.Context()); got != Failed || len(ot.hub.log()) != 0 {
		t.Fatalf("%v %v", got, ot.hub.log())
	}
}

// The watcher owns its record before its first hub call, so the request it answers is cleared at
// once. A bind answer the watcher cannot use, and that no retry changes, fails it: the claim is
// given up and the record latches failed.
func TestKimiOnceOwnsBeforeTheHubAndFailsOnAPermanentRefusal(t *testing.T) {
	for name, refusal := range map[string]error{
		"no combined match": errUnmatched,
		"ambiguous":         &hubclient.Error{Code: "ambiguous"},
		"bad request":       &hubclient.Error{Code: "bad_request"},
		"unauthorized":      &hubclient.Error{Code: "unauthorized"},
	} {
		t.Run(name, func(t *testing.T) {
			ot := newOnceTest(t)
			ot.hub.binds = []error{refusal}
			ot.hub.onBind = func() {
				if r := ot.record(); r.Attempt == nil || r.Attempt.ID != "self" {
					t.Errorf("the hub was called before the record was owned: %+v", r)
				}
			}
			if got := ot.w.run(t.Context()); got != Failed || !slices.Equal(ot.hub.log(), []string{"bind"}) {
				t.Fatalf("%v %v", got, ot.hub.log())
			}
			if r := ot.record(); r.Attempt != nil || r.Latch == nil || r.Latch.State != kimiwake.LatchFailed {
				t.Fatalf("%+v", r)
			}
		})
	}
}

// An Execution that is not found may be recovering after a hub restart, so unbound is retried with
// backoff for the recovery window, then fails. A bind that stalls is cut off at its call budget,
// and the local checks go on between tries.
func TestKimiOnceRetriesUnboundForTheRecoveryWindow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ot := newOnceTest(t)
		unbound := &hubclient.Error{Code: "unbound"}
		ot.hub.binds = []error{unbound, errStall, unbound}
		ot.reads(mail("s1", 0, 0, 10))
		ot.stream()
		ctx, cancel := context.WithCancel(t.Context())
		done := ot.start(ctx)
		time.Sleep(time.Minute)
		synctest.Wait()
		if log := ot.hub.log(); !slices.Equal(log[:6], []string{"bind", "bind", "bind", "bind", "attention", "connect"}) {
			t.Fatalf("%v", log)
		}
		cancel()
		if got := <-done; got != Disabled {
			t.Fatal(got)
		}
	})
	synctest.Test(t, func(t *testing.T) {
		ot := newOnceTest(t)
		for range 40 {
			ot.hub.binds = append(ot.hub.binds, &hubclient.Error{Code: "unbound"})
		}
		start := time.Now()
		if got := <-ot.start(t.Context()); got != Failed || time.Since(start) < onceUnbound {
			t.Fatalf("%v after %v", got, time.Since(start))
		}
		if r := ot.record(); r.Latch == nil || r.Latch.State != kimiwake.LatchFailed {
			t.Fatalf("%+v", r)
		}
	})
}

// While the bind is refused, the watcher keeps its local checks: wake turned off ends it, disabled.
func TestKimiOnceKeepsItsChecksWhileTheBindIsRefused(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ot := newOnceTest(t)
		for range 40 {
			ot.hub.binds = append(ot.hub.binds, errStall)
		}
		done := ot.start(t.Context())
		time.Sleep(10 * time.Second)
		ot.wake("off")
		if got := <-done; got != Disabled {
			t.Fatal(got)
		}
		if r := ot.record(); r.Latch == nil || r.Latch.State != kimiwake.LatchDisabled || r.Attempt != nil {
			t.Fatalf("%+v", r)
		}
	})
}
