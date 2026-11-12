package wake

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/hook"
)

type stateProcessMessage struct {
	Kind        string
	PID         int
	Snapshot    stateSnapshot
	Publication publication
	Error       string
}

// Each opt-in child uses the production state store and OS file lock. No test
// process runs a client command, reaches the network, or opens the token file.
func init() {
	if mode := os.Getenv("IRIS_WAKE_STATE_HELPER"); mode != "" {
		os.Exit(runStateProcessHelper(mode))
	}
}

func stateProcessAttempt(id string) attempt {
	return attempt{ID: id, Kind: "queue", Stream: "stream", Cursor: 7, Slot: 1,
		StartedMS: 1800000000001, Token: "process-token-" + id, Message: "fixed process admission prompt " + id}
}

func runStateProcessHelper(mode string) int {
	store := newStateStore(hook.NewSessions(os.Getenv("IRIS_WAKE_STATE_TOKEN")), commandSession)
	before, err := store.snapshot()
	if err != nil {
		return 81
	}
	output := json.NewEncoder(os.Stdout)
	message := stateProcessMessage{Kind: "snapshot", PID: os.Getpid(), Snapshot: before}
	if err := output.Encode(message); err != nil {
		return 82
	}
	if mode == "reopen" {
		return 0
	}
	var barrier struct{ Publish bool }
	if err := json.NewDecoder(os.Stdin).Decode(&barrier); err != nil || !barrier.Publish {
		return 83
	}
	// Keep the complete pre-barrier snapshot: refreshing it here would allow
	// the second child to append after the first instead of refusing its race.
	next := before.Document.appendAttempt(stateProcessAttempt(os.Getenv("IRIS_WAKE_STATE_ID")))
	published, err := store.publish(before, next)
	message = stateProcessMessage{Kind: "result", PID: os.Getpid(), Publication: published}
	if errors.Is(err, errStateChanged) {
		message.Error = "changed"
	} else if err != nil {
		message.Error = "unexpected"
	}
	if err := output.Encode(message); err != nil {
		return 84
	}
	return 0
}

type stateProcessChild struct {
	cmd    *exec.Cmd
	input  io.WriteCloser
	output *json.Decoder
	errors bytes.Buffer
	waited bool
}

func startStateProcess(t *testing.T, executable, token, id, mode string) *stateProcessChild {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	child := &stateProcessChild{cmd: exec.CommandContext(ctx, executable)}
	child.cmd.WaitDelay = time.Second
	// Private coverage output keeps runtime diagnostics off the barrier protocol
	// and avoids mixing concurrently written child data with the parent profile.
	child.cmd.Env = append(os.Environ(),
		"IRIS_WAKE_COMMAND_HELPER=", "IRIS_WAKE_STATE_HELPER="+mode,
		"IRIS_WAKE_STATE_TOKEN="+token, "IRIS_WAKE_STATE_ID="+id,
		"GOCOVERDIR="+t.TempDir())
	var err error
	child.input, err = child.cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := child.cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	child.output = json.NewDecoder(output)
	child.cmd.Stderr = &child.errors
	if err := child.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = child.input.Close()
		if !child.waited {
			_ = child.cmd.Process.Kill()
			_ = child.cmd.Wait()
		}
	})
	return child
}

func (c *stateProcessChild) receive(t *testing.T, kind string) stateProcessMessage {
	t.Helper()
	var message stateProcessMessage
	if err := c.output.Decode(&message); err != nil {
		t.Fatalf("state child %s response: %v", kind, err)
	}
	if message.Kind != kind || message.PID != c.cmd.Process.Pid || message.PID == os.Getpid() {
		t.Fatalf("state child did not execute independently: kind %q, PID %d", message.Kind, message.PID)
	}
	return message
}

func (c *stateProcessChild) wait(t *testing.T) {
	t.Helper()
	if err := c.output.Decode(new(stateProcessMessage)); err != io.EOF {
		t.Fatalf("state child produced extra protocol output: %v", err)
	}
	err := c.cmd.Wait()
	c.waited = true
	if err != nil || c.errors.Len() != 0 {
		t.Fatalf("state child did not exit cleanly: %v; stderr %q", err, c.errors.String())
	}
}

func TestStateTwoProcessesAdmitExactlyOneReservationFromTheSameSnapshot(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	token := filepath.Join(t.TempDir(), "token")
	store := newStateStore(hook.NewSessions(token), commandSession)
	// The race must preserve a prior spent slot as well as the whole told file,
	// including bytes beyond the line its parser understands.
	engineTold(t, store, "stream", 7, time.Unix(1799999000, 0), false)
	told, err := os.OpenFile(toldPath(store), os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := told.WriteString("retained full snapshot bytes\n")
	closeErr := told.Close()
	if writeErr != nil || closeErr != nil {
		t.Fatalf("prepare complete told snapshot: %v, %v", writeErr, closeErr)
	}
	spent := testAttempt(1, "queue", "submitted")
	spent.ConsumedMS = 1800000000000
	writeState(t, store, testDocument(spent))
	before := mustSnapshot(t, store)
	ids := []string{fmt.Sprintf("%032x", 2), fmt.Sprintf("%032x", 3)}
	children := []*stateProcessChild{
		startStateProcess(t, executable, token, ids[0], "writer"),
		startStateProcess(t, executable, token, ids[1], "writer"),
	}
	for _, child := range children {
		ready := child.receive(t, "snapshot")
		if !sameState(before, ready.Snapshot) || !reflect.DeepEqual(before, ready.Snapshot) {
			t.Fatal("independent children did not reach the barrier with the same complete snapshot")
		}
	}
	if children[0].cmd.Process.Pid == children[1].cmd.Process.Pid {
		t.Fatal("admission contenders were the same operating-system process")
	}
	// Both children are already blocked after their identical snapshots. Release
	// them only now, so neither writer can acquire a snapshot after its competitor.
	for _, child := range children {
		if err := json.NewEncoder(child.input).Encode(struct{ Publish bool }{true}); err != nil {
			t.Fatal(err)
		}
	}
	winner, loser := "", ""
	for i, child := range children {
		result := child.receive(t, "result")
		if result.Publication.ID != ids[i] {
			t.Fatal("publication result addressed another child's reservation")
		}
		if result.Publication.Published && result.Error == "" {
			if winner != "" {
				t.Fatal("both independent processes published their stale snapshots")
			}
			winner = ids[i]
		} else if !result.Publication.Published && result.Error == "changed" {
			if loser != "" {
				t.Fatal("neither independent process published")
			}
			loser = ids[i]
		} else {
			t.Fatalf("unexpected independent-process admission result: %+v", result.Publication)
		}
		child.wait(t)
	}
	if winner == "" || loser == "" {
		t.Fatalf("expected one publication and one changed-snapshot refusal: winner %q, loser %q", winner, loser)
	}
	after := mustSnapshot(t, store)
	if len(after.Document.Attempts) != 2 || after.Document.Attempts[0] != spent || after.Document.Attempts[1] != stateProcessAttempt(winner) || after.Document.quota(0) != 2 || after.Document.quota(1) != 2 || after.Told != before.Told {
		t.Fatalf("winner discarded prior quota, installed the loser, or changed told state: %+v", after.Document)
	}
	entries, err := os.ReadDir(filepath.Dir(store.path()))
	if err != nil || len(entries) != 1 || entries[0].Name() != filepath.Base(store.path()) {
		t.Fatalf("independent publication left temporary or competing files: %v, %v", entries, err)
	}
	// A third process opens the surviving guard after both publishers exit. It
	// must see the winner and both spent slots, without a new allowance.
	reopened := startStateProcess(t, executable, token, "", "reopen")
	restored := reopened.receive(t, "snapshot").Snapshot
	reopened.wait(t)
	if !sameState(after, restored) || restored.Document.outstanding() == nil || restored.Document.outstanding().ID != winner || restored.Document.quota(0) != 2 || restored.Document.quota(1) != 2 {
		t.Fatal("a separate process restart lost the outstanding guard or spent quota")
	}
	if _, err := os.Stat(token); !os.IsNotExist(err) {
		t.Fatalf("state admission read or created a credential file: %v", err)
	}
}
