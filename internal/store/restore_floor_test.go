package store

import (
	"errors"
	"sync"
	"testing"
	"time"
)

func assertRegistrationLost(t *testing.T, err error, highest int64) {
	t.Helper()
	var lost *Error
	if !errors.As(err, &lost) || lost.Code != "lost" {
		t.Fatalf("expected a generation refusal, got %v", err)
	}
	details, _ := lost.Details.(map[string]any)
	if details["highest_generation"] != highest {
		t.Fatalf("expected integer highest_generation %d, got %v", highest, lost.Details)
	}
}

func TestRetryCannotUseLowerRetainedHistoryAfterASweep(t *testing.T) {
	s, clock, _ := testStore(t)
	registration := Request{Client: "codex", Host: "host", SessionID: "same", Cwd: "/repo/w1", Worktree: "/repo/w1", Source: "hook"}
	first := call(t, s, "sessions", registration)
	firstProof := Request{ExecutionID: first["execution_id"].(string), Generation: 1}
	name := first["agent"].(map[string]any)["name"].(string)
	historySend(t, s, firstProof, name, "keep-first", "", "retention", "mail keeps the first Execution")
	call(t, s, "end", firstProof)
	second := call(t, s, "sessions", registration)
	call(t, s, "end", Request{ExecutionID: second["execution_id"].(string), Generation: 2})
	registration.Generation = 2
	_, err := s.Call(t.Context(), "sessions", registration)
	assertRegistrationLost(t, err, 2)

	clock.advance(finishedHorizon + time.Millisecond)
	removed, err := s.Sweep(t.Context())
	if err != nil || removed["executions"] != 1 {
		t.Fatalf("sweep did not retain just the mailed Execution: %v, %v", removed, err)
	}
	before := databaseState(t, s)
	registration.Generation, registration.RetryGeneration = 0, 2
	out, err := s.Call(t.Context(), "sessions", registration)
	assertRegistrationLost(t, err, 1)
	if out != nil || databaseState(t, s) != before {
		t.Fatalf("floor refusal changed the restored state or returned authority: %v", out)
	}
}

func TestRetryCannotBindBelowFloorWithSufficientTerminalHistory(t *testing.T) {
	s, _, _ := testStore(t)
	registration := Request{Client: "codex", Host: "host", SessionID: "same", Cwd: "/repo/w1", Worktree: "/repo/w1", Source: "hook"}
	first := call(t, s, "sessions", registration)
	earlier := first["execution_id"].(string)
	call(t, s, "end", Request{ExecutionID: earlier, Generation: 1})
	second := call(t, s, "sessions", registration)
	call(t, s, "end", Request{ExecutionID: second["execution_id"].(string), Generation: 2})
	// Construct a valid row layout with an active lower generation and higher terminal history.
	// The retry has to check the prospective active binding separately from the history it sees.
	if _, err := s.db.Exec("UPDATE Execution SET state='active' WHERE id=?", earlier); err != nil {
		t.Fatal(err)
	}
	before := databaseState(t, s)
	registration.RetryGeneration = 2
	out, err := s.Call(t.Context(), "sessions", registration)
	assertRegistrationLost(t, err, 2)
	if out != nil || databaseState(t, s) != before {
		t.Fatalf("floor refusal renewed the lower Execution or returned authority: %v", out)
	}
}

func TestConcurrentGenerationFloorRetriesShareOneSuccessor(t *testing.T) {
	s, _, _ := testStore(t)
	registration := Request{Client: "codex", Host: "host", SessionID: "same", Cwd: "/repo/w1", Worktree: "/repo/w1", Source: "hook"}
	first := call(t, s, "sessions", registration)
	call(t, s, "end", Request{ExecutionID: first["execution_id"].(string), Generation: 1})
	registration.Generation = 1
	_, err := s.Call(t.Context(), "sessions", registration)
	assertRegistrationLost(t, err, 1)
	registration.Generation, registration.RetryGeneration = 0, 1
	const attempts = 8
	type result struct {
		out map[string]any
		err error
	}
	var results [attempts]result
	changes := make(chan GenerationChange, attempts)
	ctx := WithGenerationObserver(t.Context(), func(change GenerationChange) { changes <- change })
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range results {
		wg.Go(func() {
			<-start
			out, err := s.Call(ctx, "sessions", registration)
			results[i] = result{out, err}
		})
	}
	close(start)
	wg.Wait()
	successor := results[0].out["execution_id"]
	for _, result := range results {
		if result.err != nil || successor == nil || successor == first["execution_id"] || result.out["execution_id"] != successor || result.out["generation"] != int64(2) {
			t.Fatalf("concurrent retries should converge on a single new G2: %v, %v", result.out, result.err)
		}
	}
	if len(changes) != 1 {
		t.Fatalf("retries made %d generation changes", len(changes))
	}
	if change := <-changes; change.ExecutionID != successor || change.RetryGeneration != 1 {
		t.Fatalf("unexpected generation change: %+v", change)
	}
}

func TestFailureReadingTheHighestGenerationIsNotARegistrationRefusal(t *testing.T) {
	s, _, file := testStore(t)
	registration := Request{Client: "codex", Host: "host", SessionID: "same", Cwd: "/repo/w1", Worktree: "/repo/w1", Source: "hook", WorktreeID: "device:inode"}
	call(t, s, "sessions", registration)
	fault := installFaultDriver(t, s, file)
	registration.Generation = 3
	// Discover where the real query and result stepping run in the transaction.
	_, err := s.Call(t.Context(), "sessions", registration)
	assertRegistrationLost(t, err, 1)
	var at int
	for i, query := range fault.trace {
		if query == highestGenerationSQL {
			at = i + 1
			break
		}
	}
	if at == 0 {
		t.Fatal("registration did not check retained history")
	}
	before := databaseState(t, s)
	for _, step := range []int{at, at + 1} {
		fault.count, fault.at = 0, step
		out, err := s.Call(t.Context(), "sessions", registration)
		fault.at = 0
		if !errors.Is(err, errStorageFault) || out != nil || databaseState(t, s) != before {
			t.Fatalf("failed history read at step %d exposed state or authority: %v, %v", step, out, err)
		}
	}
}

type leaseJumpClock struct {
	*fakeClock
	calls int
}

func (c *leaseJumpClock) Tick() int64 {
	c.calls++
	// Expiry and registration run before a pause. When reconciliation starts,
	// the new lease has just expired.
	if c.calls > 2 {
		return c.fakeClock.Tick() + (15 * time.Minute).Milliseconds()
	}
	return c.fakeClock.Tick()
}

func TestCombinedShellRefusalReportsOnlyCommittedHistory(t *testing.T) {
	s, clock, _ := testStore(t)
	registration := Request{Client: "codex", Host: "host", SessionID: "same", Cwd: "/repo/w1", Worktree: "/repo/w1", Source: "hook"}
	first := call(t, s, "sessions", registration)
	call(t, s, "end", Request{ExecutionID: first["execution_id"].(string), Generation: 1})
	before := databaseState(t, s)
	s.clock = &leaseJumpClock{fakeClock: clock}
	registration.RetryGeneration = 1
	registration.Kind = "before"
	out, err := s.Call(t.Context(), "reconcile", registration)
	assertRegistrationLost(t, err, 1)
	if out != nil || databaseState(t, s) != before {
		t.Fatalf("failed reconcile published its provisional replacement: %v", out)
	}
}
