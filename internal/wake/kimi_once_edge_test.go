package wake

import (
	"context"
	"errors"
	"os"
	"testing"
	"testing/synctest"
	"time"

	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/kimiwake"
)

// break makes the watcher's record unreadable, as a disk fault or another program might.
func (ot *onceTest) breakRecord() {
	if err := os.WriteFile(ot.recordPath(), []byte("{"), 0o600); err != nil {
		ot.t.Error(err)
	}
}

// A record that becomes unreadable after the watcher owned it fails the watcher: it cannot take its
// baseline, or cannot save one. A record it cannot read is left as it is.
func TestKimiOnceFailsWhenItsRecordBreaks(t *testing.T) {
	ot := newOnceTest(t)
	ot.baseline("s1", 10, nil)
	ot.hub.onBind = ot.breakRecord
	if got := ot.w.run(t.Context()); got != Failed {
		t.Fatal(got)
	}
	ot = newOnceTest(t)
	ot.hub.set(func(h *onceFake) {
		h.read = func(int64, string) (attentionRead, error) {
			ot.breakRecord()
			return mail("s1", 0, 0, 10), nil
		}
	})
	if got := ot.w.run(t.Context()); got != Failed || ot.out.String() != outcomeLines[Failed]+"\n" {
		t.Fatalf("%v %q", got, ot.out)
	}
	if raw, _ := os.ReadFile(ot.recordPath()); string(raw) != "{" {
		t.Fatalf("an unreadable record was replaced: %q", raw)
	}
}

// A stop from outside ends the watcher while it backs off, in every phase: the bind, the baseline
// and the stream. The heartbeat and the local checks keep running while it waits, so a heartbeat
// answering lost ends it there too.
func TestKimiOnceStopsWhileItBacksOff(t *testing.T) {
	for name, setup := range map[string]func(ot *onceTest){
		"bind": func(ot *onceTest) {
			for range 40 {
				ot.hub.binds = append(ot.hub.binds, &hubclient.Error{Code: "unbound"})
			}
		},
		"baseline": func(ot *onceTest) {
			ot.hub.read = func(int64, string) (attentionRead, error) { return attentionRead{}, errors.New("down") }
		},
		"stream": func(ot *onceTest) {
			ot.baseline("s1", 10, nil)
			ot.hub.connErr = errors.New("down")
		},
	} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ot := newOnceTest(t)
				setup(ot)
				ctx, cancel := context.WithCancel(t.Context())
				done := ot.start(ctx)
				time.Sleep(1500 * time.Millisecond)
				cancel()
				if got := <-done; got != Disabled {
					t.Fatal(got)
				}
			})
		})
	}
	synctest.Test(t, func(t *testing.T) {
		ot := newOnceTest(t)
		ot.baseline("s1", 10, nil)
		ot.hub.connErr = errors.New("down")
		ot.hub.beat = func() error { return &hubclient.Error{Code: "lost"} }
		if got := <-ot.start(t.Context()); got != Lost {
			t.Fatal(got)
		}
	})
}

// Attention held for its window above the cursor a restore went back to is dropped with the events
// it came from. Attention that arrives after the re-anchoring is announced, once its window allows.
func TestKimiOnceDropsHeldAttentionAboveARollback(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ot := newOnceTest(t)
		due := time.Now().Add(20 * time.Second)
		ot.baseline("s1", 100, func(r *kimiwake.Record) { r.Coalesce = kimiwake.Delays{"claim_available": due.UnixMilli()} })
		ot.reads(claim("s1", 101, 101))
		c := ot.stream()
		done := ot.start(t.Context())
		c.frames <- frame("s1", 101, "claim_available")
		synctest.Wait()
		ot.reads(mail("s1", 0, 0, 20))
		c.frames <- frame("s1", 0, "reset")
		time.Sleep(30 * time.Second)
		synctest.Wait()
		if ot.out.String() != "" || ot.record().Checkpoint.Cursor != 20 {
			t.Fatalf("%q %+v", ot.out, ot.record())
		}
		ot.reads(claim("s1", 21, 21))
		c.frames <- frame("s1", 21, "claim_available")
		ot.announced(<-done, "s1", 21, map[string]int64{"claim_available": 21})
	})
}

// A frame that arrives while the watcher completes is neither announced nor checkpointed: it is left
// for the next watcher, and the stream's reader stops with the watcher.
func TestKimiOnceLeavesAFrameThatArrivesAsItCompletes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ot := newOnceTest(t)
		ot.baseline("s1", 10, nil)
		gate := make(chan struct{})
		ot.hub.set(func(h *onceFake) {
			h.read = func(int64, string) (attentionRead, error) {
				<-gate
				return mail("s1", 1, 11, 11), nil
			}
		})
		c := ot.stream()
		done := ot.start(t.Context())
		c.frames <- frame("s1", 11, "inbox_changed")
		c.frames <- frame("s1", 12, "inbox_changed")
		close(gate)
		ot.announced(<-done, "s1", 11, map[string]int64{"inbox_changed": 11})
	})
}

// The local checks run again right before the line: wake turned off while attention was read is
// not announced.
func TestKimiOnceChecksAgainRightBeforeTheLine(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ot, c, done := following(t, t.Context())
		ot.hub.set(func(h *onceFake) {
			h.read = func(int64, string) (attentionRead, error) {
				ot.wake("off")
				return mail("s1", 1, 11, 11), nil
			}
		})
		c.frames <- frame("s1", 11, "inbox_changed")
		if got := <-done; got != Disabled || ot.out.String() != outcomeLines[Disabled]+"\n" || len(ot.hub.saved) != 0 {
			t.Fatalf("%v %q %v", got, ot.out, ot.hub.saved)
		}
	})
}
