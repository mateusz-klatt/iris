package wake

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestWakeEngineActualStartFailuresRemainLimitedAcrossReconstructions(t *testing.T) {
	binary := commandBinary(t)
	base, resolver, _, now := newEngineTest(t)
	t.Setenv("HOME", filepath.Join(t.TempDir(), "home-sentinel"))
	t.Setenv("CODEX_HOME", filepath.Join(t.TempDir(), "codex-home-sentinel"))
	command, record, _ := commandSetup(t, binary, commandScenario{
		VersionOut: "codex-cli " + commandVersion + "\n", Out: commandAccept + "\n",
	})
	startCalls, startFailures := 0, 0
	var missingPath string
	resolver.admit = func(start func() error) (bool, error) {
		if !filepath.IsAbs(missingPath) {
			t.Errorf("Start failure path is not absolute: %q", missingPath)
		}
		if _, err := os.Stat(missingPath); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("Start failure path is not absent: %v", err)
		}
		startCalls++
		err := start()
		if err == nil || (!errors.Is(err, os.ErrNotExist) && !errors.Is(err, exec.ErrNotFound)) {
			t.Errorf("expected an actual missing-executable Start failure, got %v", err)
		}
		if err != nil {
			startFailures++
		}
		return err == nil, err
	}
	var first attempt
	for reconstruction := range 3 {
		// Reopen the durable ledger in a fresh engine with the same positive
		// root identity. Restarting grants neither another slot nor another try.
		e := *base
		e.store = newStateStore(base.store.sessions, base.target.SessionID)
		c := *command
		c.path, c.now = binary, base.now
		e.commands = engineIdle(&c)
		e.store.io.syncDir = func(string) error {
			// The real --version has succeeded. Make the queue's actual Start
			// fail after its reservation is published, without faking an error.
			missingPath = filepath.Join(t.TempDir(), "missing-codex")
			c.path = missingPath
			return nil
		}
		result := e.step(context.Background())
		want := stepResult{}
		if reconstruction == 0 {
			want = engineDue(*now, time.Minute)
		}
		if result != want {
			t.Fatalf("reconstruction %d: retry result %+v, want %+v", reconstruction, result, want)
		}
		d := mustSnapshot(t, e.store).Document
		if reconstruction == 0 {
			first = d.Attempts[0]
		}
		if d.Attempts[0] != first || first.Kind != "queue" || first.Answer != "failed" || first.AnsweredMS <= 0 {
			t.Fatalf("reconstruction changed the first persisted Start failure: %+v", d)
		}
		wantTries := min(reconstruction+1, 2)
		if len(d.Attempts) != wantTries || d.tries(0) != wantTries || d.outstanding() != nil {
			t.Fatalf("reconstruction %d bypassed known-failure limits: %+v", reconstruction, d)
		}
		if reconstruction > 0 && (d.Attempts[1].ID == first.ID || d.Attempts[1].Kind != "queue" || d.Attempts[1].Answer != "failed" || d.quota(0) != 2) {
			t.Fatalf("second Start failure did not exhaust the original slot: %+v", d)
		}
		*now = now.Add(time.Minute)
	}
	if startCalls != 2 || startFailures != 2 {
		t.Fatalf("three engine reconstructions made %d Starts and %d failures, want exactly two of each", startCalls, startFailures)
	}
	f, err := os.Open(record)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := f.Close(); err != nil {
			t.Errorf("close command recording: %v", err)
		}
	}()
	decoder := json.NewDecoder(f)
	for range 2 {
		var invocation commandRecord
		if err := decoder.Decode(&invocation); err != nil || !reflect.DeepEqual(invocation.Args, []string{"--version"}) {
			t.Fatalf("unexpected executable invocation or accepted queue process: %q, %v", invocation.Args, err)
		}
	}
	if err := decoder.Decode(new(commandRecord)); err != io.EOF {
		t.Fatalf("exhausted Start failures launched another version or queue process: %v", err)
	}
}
