package wake

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/mateusz-klatt/iris/internal/hook"
	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/kimiwake"
)

// both is a snapshot with mail and freed paths pending, under one identity.
func both(stream string, cursor, high int64) attentionRead {
	return attentionRead{Stream: stream, HighWater: high, Attention: hubclient.Attention{
		Unread: 1, Available: 1, Identity: hubclient.Identity{Known: true, Pending: true, Stream: stream, Cursor: cursor},
	}}
}

// rerun starts a replacement watcher on the same record, with fresh output.
func (ot *onceTest) rerun(ctx context.Context) <-chan Outcome {
	ot.out = &onceOut{}
	ot.w.out = ot.out
	return ot.start(ctx)
}

// What a rollback took back is saved with the re-anchored checkpoint, so a replacement watcher does
// not suppress attention with an identity emitted before the restore.
func TestKimiOnceSavesARollbackForTheNextWatcher(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ot := newOnceTest(t)
		ot.baseline("s1", 100, func(r *kimiwake.Record) {
			r.Emitted = map[string]kimiwake.Identity{"claim_available": {Stream: "s1", Cursor: 100}}
		})
		ot.reads(mail("s1", 0, 0, 20))
		c := ot.stream()
		ctx, cancel := context.WithCancel(t.Context())
		done := ot.start(ctx)
		c.frames <- frame("s1", 0, "reset")
		synctest.Wait()
		cancel()
		<-done
		if r := ot.record(); r.Checkpoint.Cursor != 20 || len(r.Emitted) != 0 {
			t.Fatalf("the rollback was not saved: %+v", r)
		}
		ot.reads(claim("s1", 21, 21))
		c = ot.stream()
		done = ot.rerun(t.Context())
		c.frames <- frame("s1", 21, "claim_available")
		ot.announced(<-done, "s1", 21, map[string]int64{"claim_available": 21})
	})
}

// A reset whose read fails is read again after a call budget, with no further frame: attention
// pending after the reset is still announced on a quiet stream.
func TestKimiOnceReadsAFailedResetAgain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ot := newOnceTest(t)
		ot.baseline("s1", 10, nil)
		ot.hub.set(func(h *onceFake) {
			h.read = func(int64, string) (attentionRead, error) { return attentionRead{}, errors.New("down") }
		})
		c := ot.stream()
		done := ot.start(t.Context())
		c.frames <- frame("s1", 0, "reset")
		synctest.Wait()
		ot.reads(claim("s1", 15, 30))
		ot.announced(<-done, "s1", 30, map[string]int64{"claim_available": 15})
	})
}

// While a reset is unresolved, a later frame does not move the cursor past what the reset has yet
// to judge: the reset, read again, still finds attention newer than the old checkpoint.
func TestKimiOnceHoldsItsCursorWhileAResetIsUnresolved(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ot := newOnceTest(t)
		ot.baseline("s1", 10, nil)
		ot.hub.set(func(h *onceFake) {
			h.read = func(int64, string) (attentionRead, error) { return attentionRead{}, errors.New("down") }
		})
		c := ot.stream()
		done := ot.start(t.Context())
		c.frames <- frame("s1", 0, "reset")
		c.frames <- frame("s1", 31, "claim_released")
		synctest.Wait()
		if len(ot.hub.saved) != 0 || ot.record().Checkpoint.Cursor != 10 {
			t.Fatalf("moved past an unresolved reset: %v %+v", ot.hub.saved, ot.record())
		}
		ot.reads(claim("s1", 25, 31))
		ot.announced(<-done, "s1", 31, map[string]int64{"claim_available": 25})
	})
}

// Attention whose authoritative read fails is decided again after a call budget, without another
// frame.
func TestKimiOnceDecidesAgainAfterAFailedRead(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ot, c, done := following(t, t.Context())
		ot.hub.set(func(h *onceFake) {
			h.read = func(int64, string) (attentionRead, error) { return attentionRead{}, errors.New("down") }
		})
		c.frames <- frame("s1", 11, "inbox_changed")
		synctest.Wait()
		if ot.out.String() != "" {
			t.Fatalf("announced without a read: %q", ot.out)
		}
		ot.reads(mail("s1", 1, 11, 11))
		ot.announced(<-done, "s1", 11, map[string]int64{"inbox_changed": 11})
	})
}

// A stop, or the end of the lifetime, during the last map read before a line also prints nothing.
func TestKimiOnceEndedDuringTheLastMapReadPrintsNoLine(t *testing.T) {
	for name, want := range map[string]Outcome{"stopped": Disabled, "expired": Expired} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ot := newOnceTest(t)
				ot.baseline("s1", 10, nil)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				armed := false // set once the told state is read, which comes before the last map read
				ot.w.told = func(string) (hook.ToldSnapshot, error) {
					armed = true
					return hook.ToldSnapshot{}, nil
				}
				read := ot.w.mapped
				ot.w.mapped = func(pid int, start int64) (string, bool, error) {
					if armed && want == Disabled {
						cancel()
					} else if armed {
						time.Sleep(onceLifetime + time.Minute)
					}
					return read(pid, start)
				}
				ot.reads(mail("s1", 1, 11, 11))
				c := ot.stream()
				done := ot.start(ctx)
				c.frames <- frame("s1", 11, "inbox_changed")
				if got := <-done; got != want || ot.out.String() != outcomeLines[want]+"\n" || len(ot.hub.saved) != 0 {
					t.Fatalf("%v %q %v", got, ot.out, ot.hub.saved)
				}
				if r := ot.record(); r.Latch == nil || r.Latch.State != latchFor[want] {
					t.Fatalf("%+v", r)
				}
			})
		})
	}
}

// Attention held from one stream is dropped when the stream changes: it is never announced under
// the new stream, nor recorded there with the old stream's cursor.
func TestKimiOnceDropsHeldAttentionWhenTheStreamChanges(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ot := newOnceTest(t)
		due := time.Now().Add(20 * time.Second)
		ot.baseline("s1", 10, func(r *kimiwake.Record) { r.Coalesce = kimiwake.Delays{"claim_available": due.UnixMilli()} })
		ot.reads(claim("s1", 11, 11))
		c := ot.stream()
		done := ot.start(t.Context())
		c.frames <- frame("s1", 11, "claim_available")
		synctest.Wait()
		ot.reads(mail("s2", 0, 0, 3))
		c.frames <- frame("s1", 0, "reset")
		time.Sleep(30 * time.Second)
		synctest.Wait()
		if ot.out.String() != "" || ot.record().Checkpoint != (kimiwake.Checkpoint{Stream: "s2", Cursor: 3, Initialized: true}) {
			t.Fatalf("%q %+v", ot.out, ot.record())
		}
		ot.reads(claim("s2", 4, 4))
		c.frames <- frame("s2", 4, "claim_available")
		ot.announced(<-done, "s2", 4, map[string]int64{"claim_available": 4})
	})
}

// A reset snapshot with mail and freed paths pending holds both kinds, so a claim is announced even
// when the mail is fetched before the decision.
func TestKimiOnceKeepsAResetsClaimWhenItsMailIsFetched(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ot := newOnceTest(t)
		ot.baseline("s1", 10, nil)
		fetched := both("s1", 20, 20)
		fetched.Attention.Unread = 0
		ot.reads(both("s1", 20, 20), fetched)
		c := ot.stream()
		done := ot.start(t.Context())
		c.frames <- frame("s1", 0, "reset")
		ot.announced(<-done, "s1", 20, map[string]int64{"claim_available": 20})
	})
}

// A line needs the session map read positively. While it cannot be read, as while another process
// holds the state's lock, the line waits. Once read, it is printed for the session the process
// still runs, and not at all for one /clear replaced.
func TestKimiOnceNeedsTheSessionMapReadBeforeALine(t *testing.T) {
	for name, repoint := range map[string]bool{"still the session": false, "replaced by /clear": true} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ot, c, done := following(t, t.Context())
				ot.reads(mail("s1", 1, 11, 11))
				ot.mapFails(errors.New("locked"))
				c.frames <- frame("s1", 11, "inbox_changed")
				time.Sleep(time.Minute)
				synctest.Wait()
				if ot.out.String() != "" {
					t.Fatalf("a line without the map: %q", ot.out)
				}
				if repoint {
					if err := ot.store.MapSession(onceKimiPID, onceKimiStart, "session_other", time.Second); err != nil {
						t.Fatal(err)
					}
				}
				ot.mapFails(nil)
				got := <-done
				if repoint {
					if got != Disabled || ot.out.String() != outcomeLines[Disabled]+"\n" {
						t.Fatalf("%v %q", got, ot.out)
					}
					return
				}
				ot.announced(got, "s1", 11, map[string]int64{"inbox_changed": 11})
			})
		})
	}
}

// A stop, or the end of the lifetime, while the watcher decides prints no attention line. Each ends
// with its own outcome and latch, never as not completed.
func TestKimiOnceEndedWhileDecidingPrintsNoLine(t *testing.T) {
	for name, want := range map[string]Outcome{"stopped": Disabled, "expired": Expired} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ot := newOnceTest(t)
				ot.baseline("s1", 10, nil)
				ctx, cancel := context.WithCancel(t.Context())
				ot.w.told = func(string) (hook.ToldSnapshot, error) {
					if want == Disabled {
						cancel()
					} else {
						time.Sleep(onceLifetime + time.Minute)
					}
					return hook.ToldSnapshot{}, nil
				}
				ot.reads(mail("s1", 1, 11, 11))
				c := ot.stream()
				done := ot.start(ctx)
				c.frames <- frame("s1", 11, "inbox_changed")
				if got := <-done; got != want || ot.out.String() != outcomeLines[want]+"\n" || len(ot.hub.saved) != 0 {
					t.Fatalf("%v %q %v", got, ot.out, ot.hub.saved)
				}
				if r := ot.record(); r.Latch == nil || r.Latch.State != latchFor[want] {
					t.Fatalf("%+v", r)
				}
				cancel()
			})
		})
	}
}

// A kind outside its coalescing window is announced at once, even while another kind waits out its
// own window. The line covers both, and both windows start again.
func TestKimiOnceAnnouncesAKindOutsideItsWindowAtOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ot := newOnceTest(t)
		start := time.Now()
		ot.baseline("s1", 10, func(r *kimiwake.Record) {
			r.Coalesce = kimiwake.Delays{"inbox_changed": start.Add(25 * time.Second).UnixMilli()}
		})
		ot.reads(mail("s1", 1, 11, 11))
		c := ot.stream()
		done := ot.start(t.Context())
		c.frames <- frame("s1", 11, "inbox_changed")
		time.Sleep(5 * time.Second)
		synctest.Wait()
		if ot.out.String() != "" {
			t.Fatal("announced inside the mail's window")
		}
		ot.reads(both("s1", 12, 12))
		c.frames <- frame("s1", 12, "claim_available")
		got := <-done
		if time.Since(start) >= 25*time.Second {
			t.Fatalf("the claim waited for the mail's window: %v", time.Since(start))
		}
		ot.announced(got, "s1", 12, map[string]int64{"inbox_changed": 11, "claim_available": 12})
	})
}
