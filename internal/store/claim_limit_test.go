package store

import (
	"context"
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
