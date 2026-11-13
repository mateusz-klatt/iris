package wake

import (
	"context"
	"errors"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/mateusz-klatt/iris/internal/hook"
	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/kimiwake"
)

// following starts a watcher with a record, following one stream.
func following(t *testing.T, ctx context.Context) (*onceTest, *onceConn, <-chan Outcome) {
	ot := newOnceTest(t)
	ot.baseline("s1", 10, nil)
	ot.reads(mail("s1", 0, 0, 10))
	c := ot.stream()
	done := ot.start(ctx)
	synctest.Wait()
	return ot, c, done
}

func (ot *onceTest) count(call string) int {
	n := 0
	for _, c := range ot.hub.log() {
		if c == call {
			n++
		}
	}
	return n
}

// Every 20 seconds the watcher renews its pinned generation as a shim. A failed heartbeat changes
// nothing; only one answering lost ends the watcher, which gives up its record without a latch, so the
// next prompt can ask again once the session is back.
func TestKimiOnceEndsOnlyOnAHeartbeatAnsweringLost(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ot, _, done := following(t, t.Context())
		time.Sleep(onceBeat)
		synctest.Wait()
		if ot.count("heartbeat") != 1 {
			t.Fatalf("%v", ot.hub.log())
		}
		ot.hub.set(func(h *onceFake) { h.beat = func() error { return errors.New("down") } })
		time.Sleep(onceBeat)
		synctest.Wait()
		ot.hub.set(func(h *onceFake) { h.beat = func() error { return &hubclient.Error{Code: "lost"} } })
		if got := <-done; got != Lost || ot.out.String() != outcomeLines[Lost]+"\n" {
			t.Fatalf("%v %q", got, ot.out)
		}
		if r := ot.record(); r.Attempt != nil || r.Latch != nil {
			t.Fatalf("%+v", r)
		}
	})
}

// The local checks run on every tick: the Kimi process ending, wake turned off, and the process
// running another session each end the watcher, disabled, with a latch.
func TestKimiOnceEndsDisabledWhenItsProcessWakeOrSessionChanges(t *testing.T) {
	for name, change := range map[string]func(ot *onceTest){
		"process ended": func(ot *onceTest) { ot.setAlive(onceKimiPID, onceKimiStart, false) },
		"wake off":      func(ot *onceTest) { ot.wake("off") },
		"session changed": func(ot *onceTest) {
			if err := ot.store.MapSession(onceKimiPID, onceKimiStart, "session_other", time.Second); err != nil {
				ot.t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ot, _, done := following(t, t.Context())
				change(ot)
				if got := <-done; got != Disabled {
					t.Fatal(got)
				}
				if r := ot.record(); r.Attempt != nil || r.Latch == nil || r.Latch.State != kimiwake.LatchDisabled {
					t.Fatalf("%+v", r)
				}
			})
		})
	}
}

// The local checks also run right before a line: attention found after wake was turned off is not
// announced.
func TestKimiOnceChecksBeforeItAnnounces(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ot, c, done := following(t, t.Context())
		ot.wake("off")
		ot.reads(mail("s1", 1, 11, 11))
		c.frames <- frame("s1", 11, "inbox_changed")
		if got := <-done; got != Disabled || ot.out.String() != outcomeLines[Disabled]+"\n" {
			t.Fatalf("%v %q", got, ot.out)
		}
	})
}

// The watcher lives at most 24 hours, then ends expired and latches. A stop from outside, as removal
// or a turned-off wake sends, ends it disabled.
func TestKimiOnceExpiresAfterItsLifetimeAndStopsWhenStopped(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ot, _, done := following(t, t.Context())
		start := time.Now()
		if got := <-done; got != Expired || time.Since(start) < onceLifetime-time.Second {
			t.Fatalf("%v after %v", got, time.Since(start))
		}
		if r := ot.record(); r.Latch == nil || r.Latch.State != kimiwake.LatchExpired {
			t.Fatalf("%+v", r)
		}
	})
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		ot, _, done := following(t, ctx)
		cancel()
		if got := <-done; got != Disabled {
			t.Fatal(got)
		}
		if r := ot.record(); r.Latch == nil || r.Latch.State != kimiwake.LatchDisabled {
			t.Fatalf("%+v", r)
		}
	})
}

// Hub failures that a retry can fix never end the watcher. A baseline read that fails, a stream that
// cannot open (including the 409 a recovering session gets), a stream that drops, a reset whose
// read fails and an attention read that fails are all retried, while the heartbeat goes on.
func TestKimiOnceRetriesWhatTheHubFailsToAnswer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ot := newOnceTest(t)
		var fails int
		ot.hub.set(func(h *onceFake) {
			h.read = func(int64, string) (attentionRead, error) {
				if fails < 2 {
					fails++
					return attentionRead{}, errors.New("read failed")
				}
				return mail("s1", 0, 0, 10), nil
			}
			h.connErr = &hubclient.StatusError{Path: "/v1/watch", Code: 409, Status: "409 Conflict"}
		})
		done := ot.start(t.Context())
		time.Sleep(10 * time.Second)
		synctest.Wait()
		if ot.record().Checkpoint != (kimiwake.Checkpoint{Stream: "s1", Cursor: 10, Initialized: true}) || ot.count("connect") < 2 {
			t.Fatalf("%+v %v", ot.record(), ot.hub.log())
		}
		first := ot.stream()
		time.Sleep(time.Minute)
		synctest.Wait()
		second := ot.stream()
		close(first.frames)
		synctest.Wait()
		ot.hub.set(func(h *onceFake) {
			h.read = func(int64, string) (attentionRead, error) { return attentionRead{}, errors.New("read failed") }
		})
		second.frames <- frame("s1", 0, "reset")
		second.frames <- frame("s1", 11, "inbox_changed")
		time.Sleep(time.Minute)
		synctest.Wait()
		if ot.out.String() != "" {
			t.Fatalf("announced without a read: %q", ot.out)
		}
		ot.reads(mail("s1", 1, 11, 11))
		ot.announced(<-done, "s1", 11, map[string]int64{"inbox_changed": 11})
		if !slices.Contains(ot.hub.log(), "heartbeat") {
			t.Fatalf("no heartbeat while retrying: %v", ot.hub.log())
		}
	})
}

// A told state that cannot be read covers nothing, so the wake is not lost.
func TestKimiOnceAnnouncesWhenToldStateIsUnreadable(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ot := newOnceTest(t)
		ot.baseline("s1", 10, nil)
		ot.w.told = func(string) (hook.ToldSnapshot, error) { return hook.ToldSnapshot{}, errors.New("locked") }
		ot.reads(mail("s1", 1, 11, 11))
		c := ot.stream()
		done := ot.start(t.Context())
		c.frames <- frame("s1", 11, "inbox_changed")
		ot.announced(<-done, "s1", 11, map[string]int64{"inbox_changed": 11})
	})
}
