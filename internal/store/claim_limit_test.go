package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestClaimLimit(t *testing.T) {
	s, _, _ := testStore(t)
	r, out := session(t, s, 1)
	paths := make([]string, 500)
	for i := range paths {
		paths[i] = fmt.Sprintf("file-%03d", i)
	}
	call(t, s, "touch", touchRequest(r, "fill", paths...))
	call(t, s, "touch", touchRequest(r, "reuse", paths...))
	expectError(t, s, "claim", claimRequest(r, "extra", "extra", "exact"), "too_large")
	expectError(t, s, "touch", touchRequest(r, "atomic", "extra-a", "extra-b"), "too_large")
	rows := call(t, s, "claims", Request{RepositoryID: out["repository_id"].(string)})
	if len(rows["claims"].([]map[string]any)) != 500 {
		t.Fatal(rows)
	}
}

func TestCanceledTransactionsLeaveConnectionClean(t *testing.T) {
	s, _, _ := testStore(t)
	for i := 0; i < 200; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		timer := time.AfterFunc(time.Duration(i%10)*time.Microsecond, cancel)
		_ = s.transact(ctx, func(*transaction) error { return nil })
		timer.Stop()
		cancel()
		call(t, s, "status", Request{})
	}
}

// An upgrade can leave an Execution above the limit. It must keep working on what it holds.
func TestTouchAboveTheLimitKeepsPathsAlreadyHeld(t *testing.T) {
	s, _, _ := testStore(t)
	r, _ := session(t, s, 1)
	paths := make([]string, 500)
	for i := range paths {
		paths[i] = fmt.Sprintf("file-%03d", i)
	}
	call(t, s, "touch", touchRequest(r, "fill", paths...))
	err := s.transact(context.Background(), func(tx *transaction) error {
		_, err := tx.exec("INSERT INTO Claim(execution_id,repository_id,kind,path,origin,boot_epoch,expires_ms) " +
			"SELECT execution_id,repository_id,kind,'over-the-limit',origin,boot_epoch,expires_ms FROM Claim LIMIT 1")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	out := call(t, s, "touch", touchRequest(r, "again", paths[0]))
	if out["action"] != "allow" {
		t.Fatalf("%v", out)
	}
	expectError(t, s, "touch", touchRequest(r, "new", paths[0], "other"), "too_large")
}

func TestTransactionNeverStartsOnACancelledContext(t *testing.T) {
	s, _, _ := testStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ran := false
	err := s.transact(ctx, func(*transaction) error { ran = true; return nil })
	if !errors.Is(err, context.Canceled) || ran {
		t.Fatal(err, ran)
	}
}
