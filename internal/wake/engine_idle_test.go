package wake

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/hubclient"
)

func TestWakeEngineActiveBurstChecksBinaryThenDefersWithoutQuota(t *testing.T) {
	e, r, source, now := newEngineTest(t)
	c, record, _ := commandSetup(t, commandBinary(t), commandScenario{VersionOut: "codex-cli " + commandVersion + "\n", Out: commandAccept + "\n"})
	c.now, e.commands = e.now, c
	e.target.SessionID = strings.ToUpper(commandSession)
	r.target = e.target
	idleReads := 0
	c.idleRead = func(ctx context.Context, session string) (bool, string) {
		idleReads++
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 5*time.Second || time.Until(deadline) <= 0 || session != commandSession {
			t.Fatalf("idle read has incorrect target or bound: %q %v", session, deadline)
		}
		body, err := os.ReadFile(record)
		rows := strings.Split(strings.TrimSpace(string(body)), "\n")
		var latest commandRecord
		if err != nil || len(rows) != idleReads || json.Unmarshal([]byte(rows[len(rows)-1]), &latest) != nil || !reflect.DeepEqual(latest.Args, []string{"--version"}) {
			t.Fatalf("idle queried before validating the binary: %q, %v", body, err)
		}
		unlock, err := e.store.sessions.Lock(0)
		if err != nil {
			t.Fatal("idle read held the session lock")
		}
		unlock()
		return false, "Codex thread is active"
	}
	for range 25 {
		if got := e.step(context.Background()); got != engineDue(*now, 15*time.Second) {
			t.Fatalf("active burst did not defer: %+v", got)
		}
	}
	if snapshot := mustSnapshot(t, e.store); snapshot.Present || r.starts != 0 || source.reads != 25 || idleReads != 25 {
		t.Fatalf("active burst reserved or queued a wake: %+v, starts%d reads%d idle%d", snapshot.Document, r.starts, source.reads, idleReads)
	}
	// An ordinary active turn fetches the mail. The next authoritative read
	// stops the retry before another status query or a queued prompt.
	source.read.Attention.Identity = hubclient.Identity{Known: true}
	*now = now.Add(15 * time.Second)
	if got := e.step(context.Background()); got != (stepResult{}) || idleReads != 25 || r.starts != 0 || mustSnapshot(t, e.store).Present {
		t.Fatalf("mail fetched by the active turn caused a wake: %+v", got)
	}
}

func TestWakeEngineUnavailablePreflightIsQuotaFreeAndRecovers(t *testing.T) {
	binary := commandBinary(t)
	for _, reason := range []string{
		"Codex status proxy could not start",
		"Codex status proxy did not establish its connection",
		"Codex thread status could not be verified",
		"Codex status proxy exceeded its bound or failed",
	} {
		t.Run(reason, func(t *testing.T) {
			e, r, source, now := newEngineTest(t)
			c, _, _ := commandSetup(t, binary, commandScenario{VersionOut: "codex-cli " + commandVersion + "\n", Out: commandAccept + "\n"})
			c.now, e.commands = e.now, c
			c.idleRead = func(context.Context, string) (bool, string) { return false, reason }
			var observed []string
			e.observe = func(text string) { observed = append(observed, text) }
			for range 2 {
				if got := e.step(context.Background()); got != engineDue(*now, time.Minute) {
					t.Fatalf("unverified status did not back off: %+v", got)
				}
				d := mustSnapshot(t, e.store).Document
				if len(d.Attempts) != 0 || nativeQuota(d) != 0 || d.LastFailure == nil || d.LastFailure.Detail != reason || d.Binary.Version != commandVersion || r.starts != 0 {
					t.Fatalf("status failure spent quota or lost fixed diagnostics: %+v", d)
				}
				*now = now.Add(time.Minute)
			}
			if !reflect.DeepEqual(observed, []string{failedIdleCheck, failedIdleCheck}) {
				t.Fatalf("unverified status was not observed as an idle check failure: %q", observed)
			}
			// Interrupted/unavailable checks do not poison the later idle result.
			c.idleRead = func(context.Context, string) (bool, string) { return true, "" }
			if got := e.step(context.Background()); !got.FileWait || r.starts != 1 || source.reads != 4 {
				t.Fatalf("verified idle did not recover from operational failures: %+v", got)
			}
			d := mustSnapshot(t, e.store).Document
			if len(d.Attempts) != 1 || d.Attempts[0].Kind != "queue" || d.Attempts[0].Slot != 0 || d.LastFailure != nil {
				t.Fatalf("recovery retained failure or created more than one wake: %+v", d)
			}
		})
	}
}

func TestWakeEngineDiagnosticFailureRetainsPriorIdentityMark(t *testing.T) {
	for _, attention := range []string{"new cursor", "other stream", "restore"} {
		t.Run(attention, func(t *testing.T) {
			e, _, source, now := newEngineTest(t)
			old := testAttempt(1, "queue", "submitted")
			old.ConsumedMS = now.Add(-time.Hour).UnixMilli()
			writeState(t, e.store, testDocument(old))
			switch attention {
			case "new cursor":
				source.read.Attention.Identity.Cursor++
			case "other stream":
				source.read.Stream, source.read.Attention.Identity.Stream = "new-stream", "new-stream"
			case "restore":
				source.read.HighWater, source.read.Attention.Identity.Cursor = 3, 2
			}
			if got := e.step(context.Background()); got != engineDue(*now, time.Minute) {
				t.Fatalf("binary failure was not operational: %+v", got)
			}
			d := mustSnapshot(t, e.store).Document
			if !reflect.DeepEqual(d.Attempts, []attempt{old}) || d.mark().Cursor != old.Cursor || nativeQuota(d) != 2 || d.LastFailure == nil {
				t.Fatalf("diagnostic publication retired a prior identity: %+v", d)
			}
			// The authoritative stream can subsequently return to the old
			// identity; retaining its mark must still suppress a second wake.
			source.read.Stream, source.read.HighWater = "stream", 20
			source.read.Attention.Identity = hubclient.Identity{Known: true, Pending: true, Stream: "stream", Cursor: old.Cursor}
			if got := e.step(context.Background()); got != (stepResult{}) {
				t.Fatalf("diagnostic-only identity change reopened spent quota: %+v", got)
			}
		})
	}
}

func TestWakeEngineVerifiedIdleRequiresFreshAttentionAndGuard(t *testing.T) {
	binary := commandBinary(t)
	for _, change := range []string{"mail fetched", "hook told", "new unknown guard", "file read error", "fresh hub error", "fresh invalid identity", "file race during fresh read"} {
		t.Run(change, func(t *testing.T) {
			e, r, source, now := newEngineTest(t)
			c, record, _ := commandSetup(t, binary, commandScenario{VersionOut: "codex-cli " + commandVersion + "\n", Out: commandAccept + "\n"})
			e.commands = c
			want := stepResult{}
			c.idleRead = func(context.Context, string) (bool, string) {
				switch change {
				case "mail fetched":
					source.read.Attention.Identity = hubclient.Identity{Known: true}
				case "hook told":
					engineTold(t, e.store, "stream", 7, *now, false)
				case "new unknown guard":
					writeState(t, e.store, testDocument(testAttempt(1, "queue", "unknown")))
					want = stepResult{Due: now.Add(15 * time.Second), FileWait: true}
				case "file read error":
					e.store.io.readFile = func(string) ([]byte, error) { return nil, errors.New(commandSecret) }
					want = engineDue(*now, time.Minute)
				case "fresh hub error":
					source.readErr = errors.New(commandSecret)
					want = engineDue(*now, 30*time.Second)
				case "fresh invalid identity":
					source.read.Attention.Identity.Known = false
					want = engineDue(*now, 30*time.Second)
				case "file race during fresh read":
					source.duringRead = func() { engineTold(t, e.store, "stream", 7, *now, false) }
					want = engineDue(*now, 10*time.Second)
				}
				return true, ""
			}
			if got := e.step(context.Background()); got != want || r.starts != 0 {
				t.Fatalf("idle alone authorized stale work: %+v, want %+v, starts%d", got, want, r.starts)
			}
			body, err := os.ReadFile(record)
			if err != nil || strings.Contains(string(body), `"queue"`) {
				t.Fatalf("fresh evidence refusal queued a prompt: %q, %v", body, err)
			}
		})
	}
}

func TestWakeEngineUnreadAfterActiveTurnQueuesOnceAcrossRestarts(t *testing.T) {
	e, r, source, now := newEngineTest(t)
	c, _, _ := commandSetup(t, commandBinary(t), commandScenario{VersionOut: "codex-cli " + commandVersion + "\n", Out: commandAccept + "\n"})
	c.now, e.commands = e.now, c
	idle := false
	c.idleRead = func(context.Context, string) (bool, string) {
		if !idle {
			return false, "Codex thread is active"
		}
		return true, ""
	}
	if got := e.step(context.Background()); got != engineDue(*now, 15*time.Second) || r.starts != 0 {
		t.Fatalf("busy turn queued a prompt: %+v", got)
	}
	// New activity while handling the pending wake stays deferred until idle.
	source.read.Attention.Identity.Cursor = 8
	*now = now.Add(15 * time.Second)
	if got := e.step(context.Background()); got != engineDue(*now, 15*time.Second) || mustSnapshot(t, e.store).Present {
		t.Fatalf("activity during wake handling escaped the idle gate: %+v", got)
	}
	idle = true
	*now = now.Add(15 * time.Second)
	if got := e.step(context.Background()); !got.FileWait || r.starts != 1 || source.reads != 4 {
		t.Fatalf("unread idle attention was not queued exactly once: %+v", got)
	}
	d := mustSnapshot(t, e.store).Document
	if len(d.Attempts) != 1 || d.Attempts[0].Cursor != 8 || d.Attempts[0].Slot != 0 {
		t.Fatalf("wake did not use the fresh identity and sole slot: %+v", d)
	}
	d.Attempts[0].ConsumedMS = now.UnixMilli()
	writeState(t, e.store, d)
	engineTold(t, e.store, "stream", 8, *now, false)
	if err := os.Remove(toldPath(e.store)); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		*now = now.Add(time.Hour)
		restarted := *e
		restarted.hookSeen = false
		restarted.store = newStateStore(e.store.sessions, e.target.SessionID)
		e = &restarted
		if got := e.step(context.Background()); got != (stepResult{}) || r.starts != 1 || len(mustSnapshot(t, e.store).Document.Attempts) != 1 {
			t.Fatalf("consumed wake repeated after ten minutes or a lost told file: %+v", got)
		}
	}
}
