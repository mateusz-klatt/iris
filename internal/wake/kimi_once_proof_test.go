package wake

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/mateusz-klatt/iris/internal/hook"
)

// A wake file has the same hashed basename as its generation file.
func wakeGenerationPath(token, client, session string) string {
	sessions := hook.NewSessions(token)
	return filepath.Join(sessions.Dir(), filepath.Base(sessions.WakePath(client, session)))
}

func writeWakeProof(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Error(err)
		return
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Error(err)
	}
}

func requireWakeObservation(t *testing.T, token, client, path string) {
	t.Helper()
	entries, err := hook.ReadRecentObservations(token, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Client == client && entry.Observed() && strings.HasSuffix(entry.Error, " at "+path) {
			return
		}
	}
	t.Fatalf("doctor has no observation for %s: %+v", path, entries)
}

// A backup can still offer generation 3 after SessionEnd. The watcher must refuse a bind for
// that generation, skip its renewal, or leave as soon as it finds the end racing a heartbeat.
func TestKimiOnceVetoesEndedGeneration(t *testing.T) {
	for _, phase := range []string{"during bind", "before heartbeat", "during heartbeat"} {
		t.Run(phase, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ot := newOnceTest(t)
				token := filepath.Join(filepath.Dir(ot.store.Root), "hub.token")
				proof := wakeGenerationPath(token, "kimi", onceSession)
				marker := proof + ".ended"
				writeWakeProof(t, proof, "3")
				writeWakeProof(t, marker, "2")
				end := func() {
					writeWakeProof(t, marker, "3")
					if err := os.Remove(proof); err != nil && !os.IsNotExist(err) {
						t.Error(err)
					}
				}
				wantBeats := 0
				switch phase {
				case "during bind":
					ot.hub.onBind = end
				case "during heartbeat":
					wantBeats = 1
					ot.hub.beat = func() error { end(); return nil }
				}
				ot.stream()
				ctx, cancel := context.WithTimeout(t.Context(), 2*onceBeat+time.Second)
				defer cancel()
				done := ot.start(ctx)
				synctest.Wait()
				if phase == "before heartbeat" {
					end()
				}
				if got := <-done; got != Lost || ot.count("heartbeat") != wantBeats {
					t.Fatalf("watcher ended with %v after %v", got, ot.hub.log())
				}
				if phase == "during bind" && ot.count("connect") != 0 {
					t.Fatal("stale bind opened a stream")
				}
				requireWakeObservation(t, token, "kimi", marker)
			})
		})
	}
}

func TestKimiOnceRefusesABindBelowLiveProof(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ot := newOnceTest(t)
		token := filepath.Join(filepath.Dir(ot.store.Root), "hub.token")
		proof := wakeGenerationPath(token, "kimi", onceSession)
		writeWakeProof(t, proof, "4")
		ot.stream()
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		if got := ot.w.run(ctx); got != Lost || !slices.Equal(ot.hub.log(), []string{"bind"}) {
			t.Fatalf("watcher ended with %v after %v", got, ot.hub.log())
		}
		requireWakeObservation(t, token, "kimi", proof)
	})
}

func TestKimiOnceRetriesBindAndSkipsHeartbeatWhileProofIsUnreadable(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ot := newOnceTest(t)
		token := filepath.Join(filepath.Dir(ot.store.Root), "hub.token")
		proof := wakeGenerationPath(token, "kimi", onceSession)
		marker := proof + ".ended"
		writeWakeProof(t, marker, "damaged")
		ot.stream()
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := ot.start(ctx)
		synctest.Wait()
		if !slices.Equal(ot.hub.log(), []string{"bind"}) {
			t.Fatalf("accepted bind with an unreadable end marker: %v", ot.hub.log())
		}
		requireWakeObservation(t, token, "kimi", marker)
		writeWakeProof(t, marker, "2")
		time.Sleep(time.Second)
		synctest.Wait()
		if ot.count("connect") != 1 {
			t.Fatalf("could not bind after proof became readable: %v", ot.hub.log())
		}
		writeWakeProof(t, proof, "damaged")
		time.Sleep(onceBeat)
		synctest.Wait()
		if ot.count("heartbeat") != 0 {
			t.Fatalf("renewed with unreadable proof: %v", ot.hub.log())
		}
		requireWakeObservation(t, token, "kimi", proof)
		writeWakeProof(t, proof, "3")
		time.Sleep(onceBeat)
		synctest.Wait()
		if ot.count("heartbeat") != 1 {
			t.Fatalf("did not renew after proof became readable: %v", ot.hub.log())
		}
		cancel()
		if got := <-done; got != Disabled {
			t.Fatalf("watcher ended with %v", got)
		}
	})
}
