package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func expectTooLargeLimit(t *testing.T, s *Store, op string, r Request, want string) {
	t.Helper()
	_, err := s.Call(context.Background(), op, r)
	var e *Error
	if !errors.As(err, &e) || e.Code != "too_large" {
		t.Fatalf("%s: got %v, wanted too_large", op, err)
	}
	details, _ := e.Details.(map[string]any)
	if details["limit"] != want {
		t.Fatalf("%s: details = %#v, want limit %q", op, e.Details, want)
	}
}

func TestClaimLimit(t *testing.T) {
	s, _, _ := testStore(t)
	r, out := session(t, s, 1)
	paths := make([]string, 500)
	for i := range paths {
		paths[i] = fmt.Sprintf("file-%03d", i)
	}
	call(t, s, "touch", touchRequest(r, "fill", paths...))
	call(t, s, "touch", touchRequest(r, "reuse", paths...))
	expectTooLargeLimit(t, s, "claim", claimRequest(r, "extra", "extra", "exact"), "claim_limit")
	expectTooLargeLimit(t, s, "touch", touchRequest(r, "atomic", "extra-a", "extra-b"), "claim_limit")
	expectTooLargeLimit(t, s, "touch_files", filesTouch(r, "files-atomic", ClaimGroup{Worktree: "/repo/w1", RemoteURL: "https://example.com/team/project.git", RootCommit: "root", Paths: []string{"extra-c"}}), "claim_limit")
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
	expectTooLargeLimit(t, s, "touch", touchRequest(r, "new", paths[0], "other"), "claim_limit")
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
