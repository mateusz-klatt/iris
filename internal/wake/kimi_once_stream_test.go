package wake

import (
	"errors"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/mateusz-klatt/iris/internal/kimiwake"
)

func frame(stream string, cursor int64, kind string) eventFrame {
	return eventFrame{Stream: stream, Cursor: cursor, Kind: kind}
}

// announced asserts the attention exit: the fixed line, the hub and local checkpoints at cursor,
// and the record given up with what was emitted.
func (ot *onceTest) announced(got Outcome, stream string, cursor int64, emitted map[string]int64) {
	ot.t.Helper()
	if got != Attention || ot.out.String() != attentionLine+"\n" {
		ot.t.Fatalf("%v %q", got, ot.out)
	}
	if saved := ot.hub.saved; len(saved) == 0 || saved[len(saved)-1] != cursor {
		ot.t.Fatalf("hub checkpoints %v, want the last at %d", saved, cursor)
	}
	r := ot.record()
	if r.Attempt != nil || r.Checkpoint != (kimiwake.Checkpoint{Stream: stream, Cursor: cursor, Initialized: true}) {
		ot.t.Fatalf("record %+v", r)
	}
	for kind, c := range emitted {
		if r.Emitted[kind] != (kimiwake.Identity{Stream: stream, Cursor: c}) || r.Coalesce[kind] != time.Now().Add(onceWindow).UnixMilli() {
			ot.t.Fatalf("%s: emitted %+v, coalesce %v", kind, r.Emitted, r.Coalesce)
		}
	}
}

// A watcher with no prior record takes a baseline, and announces nothing for history even with mail
// pending, nor for frames the stream replays up to it. New attention after it is announced.
func TestKimiOnceAnnouncesNoHistory(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ot := newOnceTest(t)
		ot.reads(mail("s1", 3, 40, 40))
		c := ot.stream()
		done := ot.start(t.Context())
		c.frames <- frame("s1", 30, "inbox_changed")
		c.frames <- frame("s1", 40, "inbox_changed")
		synctest.Wait()
		if ot.out.String() != "" || len(ot.hub.saved) != 0 {
			t.Fatalf("history was announced: %q %v", ot.out, ot.hub.saved)
		}
		if r := ot.record(); r.Checkpoint != (kimiwake.Checkpoint{Stream: "s1", Cursor: 40, Initialized: true}) {
			t.Fatalf("baseline %+v", r)
		}
		ot.reads(mail("s1", 4, 41, 41))
		c.frames <- frame("s1", 41, "inbox_changed")
		ot.announced(<-done, "s1", 41, map[string]int64{"inbox_changed": 41})
	})
}

// A watcher with a record follows it from its checkpoint: what came after is announced, without a
// new baseline.
func TestKimiOnceReplaysAfterItsCheckpoint(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ot := newOnceTest(t)
		ot.baseline("s1", 10, nil)
		ot.reads(mail("s1", 1, 11, 11))
		c := ot.stream()
		done := ot.start(t.Context())
		c.frames <- frame("s1", 11, "inbox_changed")
		ot.announced(<-done, "s1", 11, map[string]int64{"inbox_changed": 11})
		if log := ot.hub.log(); !slices.Equal(log, []string{"bind", "connect", "attention", "checkpoint"}) {
			t.Fatalf("%v", log)
		}
	})
}

// Mail already fetched, or told by a hook, is checkpointed without a line. A told identity above the
// hub's high-water mark comes from before a restore and covers nothing. Events that need no
// attention are checkpointed too.
func TestKimiOnceCheckpointsFetchedAndToldMailSilently(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ot := newOnceTest(t)
		ot.baseline("s1", 10, nil)
		ot.reads(mail("s1", 0, 0, 11))
		c := ot.stream()
		done := ot.start(t.Context())
		c.frames <- frame("s1", 11, "inbox_changed")
		c.frames <- frame("s1", 12, "inbox_read")
		synctest.Wait()
		ot.told("s1", 13)
		ot.reads(mail("s1", 1, 13, 13))
		c.frames <- frame("s1", 13, "inbox_changed")
		synctest.Wait()
		if ot.out.String() != "" || !slices.Equal(ot.hub.saved, []int64{11, 12, 13}) || ot.record().Checkpoint.Cursor != 13 {
			t.Fatalf("%q %v %+v", ot.out, ot.hub.saved, ot.record())
		}
		ot.told("s1", 99)
		ot.reads(mail("s1", 1, 14, 14))
		c.frames <- frame("s1", 14, "inbox_changed")
		ot.announced(<-done, "s1", 14, map[string]int64{"inbox_changed": 14})
	})
}

// Freed paths stay historical: availability already emitted, or told, is not announced again, and a
// newer one is, once.
func TestKimiOnceAnnouncesAvailabilityOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ot := newOnceTest(t)
		ot.baseline("s1", 10, func(r *kimiwake.Record) {
			r.Emitted = map[string]kimiwake.Identity{"claim_available": {Stream: "s1", Cursor: 11}}
		})
		ot.reads(claim("s1", 11, 11))
		c := ot.stream()
		done := ot.start(t.Context())
		c.frames <- frame("s1", 11, "claim_available")
		synctest.Wait()
		ot.told("s1", 12)
		ot.reads(claim("s1", 12, 12))
		c.frames <- frame("s1", 12, "claim_available")
		synctest.Wait()
		if ot.out.String() != "" {
			t.Fatalf("availability announced again: %q", ot.out)
		}
		ot.reads(claim("s1", 13, 13))
		c.frames <- frame("s1", 13, "claim_available")
		ot.announced(<-done, "s1", 13, map[string]int64{"claim_available": 13})
	})
}

// The coalescing window outlives each watcher through the record: attention within the window of
// the previous line waits out only the rest of it.
func TestKimiOnceWaitsOutTheRestOfTheCoalescingWindow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ot := newOnceTest(t)
		due := time.Now().Add(20 * time.Second)
		ot.baseline("s1", 10, func(r *kimiwake.Record) { r.Coalesce = kimiwake.Delays{"inbox_changed": due.UnixMilli()} })
		ot.reads(mail("s1", 1, 11, 11))
		c := ot.stream()
		done := ot.start(t.Context())
		c.frames <- frame("s1", 11, "inbox_changed")
		time.Sleep(19 * time.Second)
		synctest.Wait()
		if ot.out.String() != "" {
			t.Fatal("announced inside the window")
		}
		got := <-done
		if time.Now().Before(due) {
			t.Fatalf("announced at %v, before %v", time.Now(), due)
		}
		ot.announced(got, "s1", 11, map[string]int64{"inbox_changed": 11})
	})
}

// While attention is held for its window, nothing is checkpointed past it, even an event that needs
// no wake: a watcher that dies holding it leaves it for the next one. The line, when due, covers both
// events.
func TestKimiOnceNeverCheckpointsPastHeldAttention(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ot := newOnceTest(t)
		due := time.Now().Add(20 * time.Second)
		ot.baseline("s1", 10, func(r *kimiwake.Record) { r.Coalesce = kimiwake.Delays{"claim_available": due.UnixMilli()} })
		ot.reads(claim("s1", 11, 11))
		c := ot.stream()
		done := ot.start(t.Context())
		c.frames <- frame("s1", 11, "claim_available")
		synctest.Wait()
		ot.reads(claim("s1", 11, 12))
		c.frames <- frame("s1", 12, "inbox_changed")
		c.frames <- frame("s1", 13, "claim_released")
		synctest.Wait()
		if len(ot.hub.saved) != 0 || ot.record().Checkpoint.Cursor != 10 {
			t.Fatalf("checkpointed past held attention: %v %+v", ot.hub.saved, ot.record())
		}
		ot.announced(<-done, "s1", 13, map[string]int64{"claim_available": 11})
	})
}

// A completion that fails after printing exits not completed, with the old checkpoint, so the next
// watcher replays: a repeated wake, never a lost one. Nothing latches.
func TestKimiOnceACompletionThatFailsLeavesTheCheckpoint(t *testing.T) {
	for name, fail := range map[string]func(ot *onceTest){
		"print":      func(ot *onceTest) { ot.w.out = failingWriter{} },
		"hub":        func(ot *onceTest) { ot.hub.saveErr = errors.New("down") },
		"own record": func(ot *onceTest) { ot.hub.onSave = func() { ot.takeOver() } },
	} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ot := newOnceTest(t)
				ot.baseline("s1", 10, nil)
				fail(ot)
				ot.reads(mail("s1", 1, 11, 11))
				c := ot.stream()
				done := ot.start(t.Context())
				c.frames <- frame("s1", 11, "inbox_changed")
				if got := <-done; got != NotCompleted {
					t.Fatal(got)
				}
				if r := ot.record(); r.Checkpoint.Cursor != 10 || r.Latch != nil || len(r.Emitted) != 0 {
					t.Fatalf("%+v", r)
				}
			})
		})
	}
}

// A reset to a new stream may announce what is pending there once, but not on a watcher's first
// baseline, which never announces history.
func TestKimiOnceAnnouncesANewStreamOnceButNotOnItsFirstBaseline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ot := newOnceTest(t)
		ot.baseline("s1", 10, nil)
		ot.reads(mail("s2", 2, 5, 5))
		c := ot.stream()
		done := ot.start(t.Context())
		c.frames <- frame("s1", 0, "reset")
		ot.announced(<-done, "s2", 5, map[string]int64{"inbox_changed": 5})
	})
	synctest.Test(t, func(t *testing.T) {
		ot := newOnceTest(t)
		ot.reads(mail("s1", 0, 0, 10), mail("s2", 2, 5, 5))
		c := ot.stream()
		done := ot.start(t.Context())
		c.frames <- frame("s2", 5, "inbox_changed")
		synctest.Wait()
		if ot.out.String() != "" || ot.record().Checkpoint != (kimiwake.Checkpoint{Stream: "s2", Cursor: 5, Initialized: true}) {
			t.Fatalf("%q %+v", ot.out, ot.record())
		}
		ot.reads(mail("s2", 3, 6, 6))
		c.frames <- frame("s2", 6, "inbox_changed")
		ot.announced(<-done, "s2", 6, map[string]int64{"inbox_changed": 6})
	})
}

// After a restore took the stream back, the watcher re-anchors at the high-water mark. What it
// emitted, and what hooks told, above that mark no longer suppress new attention.
func TestKimiOnceReanchorsAfterTheStreamWentBack(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ot := newOnceTest(t)
		ot.baseline("s1", 100, func(r *kimiwake.Record) {
			r.Emitted = map[string]kimiwake.Identity{"claim_available": {Stream: "s1", Cursor: 100}, "inbox_changed": {Stream: "s2", Cursor: 100}}
		})
		ot.told("s1", 100)
		ot.reads(mail("s1", 0, 0, 20))
		c := ot.stream()
		done := ot.start(t.Context())
		c.frames <- frame("s1", 0, "reset")
		synctest.Wait()
		if r := ot.record(); r.Checkpoint.Cursor != 20 || ot.out.String() != "" {
			t.Fatalf("%q %+v", ot.out, r)
		}
		ot.reads(claim("s1", 21, 21))
		c.frames <- frame("s1", 21, "claim_available")
		ot.announced(<-done, "s1", 21, map[string]int64{"claim_available": 21})
	})
}

// A reset in the same stream, after events were dropped, announces only an identity newer than the
// cursor; otherwise it re-anchors without a line. A frame from another stream is a reset too.
func TestKimiOnceResetInTheSameStreamAnnouncesOnlyNewerAttention(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ot := newOnceTest(t)
		ot.baseline("s1", 10, nil)
		ot.reads(mail("s1", 0, 0, 30))
		c := ot.stream()
		done := ot.start(t.Context())
		c.frames <- frame("s1", 0, "reset")
		synctest.Wait()
		if ot.out.String() != "" || ot.record().Checkpoint.Cursor != 30 {
			t.Fatalf("%q %+v", ot.out, ot.record())
		}
		ot.reads(claim("s1", 35, 40))
		c.frames <- frame("s9", 3, "inbox_changed")
		ot.announced(<-done, "s1", 40, map[string]int64{"claim_available": 35})
	})
}
