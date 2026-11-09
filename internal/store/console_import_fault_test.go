package store

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestConsoleImportsRollBackAtEveryStorageBoundaryAndRetry(t *testing.T) {
	for _, operation := range []string{"accounts", "accounts-dry", "accounts-skip", "accounts-dry-skip", "assignments", "assignments-dry", "assignments-skip", "assignments-dry-skip"} {
		t.Run(operation, func(t *testing.T) {
			s, _, file := testStore(t)
			_, registered := session(t, s, 1)
			batch := importBatch()
			transfer := importTransfer(registered["repository_id"].(string))
			assignment := strings.HasPrefix(operation, "assignments")
			dryRun := strings.Contains(operation, "dry")
			skip := strings.HasSuffix(operation, "skip")
			if assignment || skip {
				if _, err := s.ImportConsoleAccounts(t.Context(), batch, false); err != nil {
					t.Fatal(err)
				}
			}
			if assignment && skip {
				if _, err := s.TransferConsoleAssignments(t.Context(), transfer, false); err != nil {
					t.Fatal(err)
				}
			}
			invoke := func() (ConsoleImportReport, error) {
				if assignment {
					return s.TransferConsoleAssignments(t.Context(), transfer, dryRun)
				}
				return s.ImportConsoleAccounts(t.Context(), batch, dryRun)
			}
			fault := installFaultDriver(t, s, file)
			fault.discover = true
			_, err := invoke()
			if (!dryRun && !errors.Is(err, errStorageFault)) || (dryRun && err != nil) {
				t.Fatal("discovery did not preserve intended transaction boundary", err)
			}
			trace := append([]string(nil), fault.trace...)
			fault.discover = false
			before, changed := databaseState(t, s), s.Changes()
			for i := range trace {
				fault.count, fault.at, fault.trace = 0, i+1, nil
				report, err := invoke()
				fault.at = 0
				if !errors.Is(err, errStorageFault) || len(report.Rows) != 0 {
					t.Fatalf("storage boundary %d did not fail without a partial report", i+1)
				}
				if databaseState(t, s) != before || s.Changes() != changed {
					t.Fatalf("storage boundary %d committed partial import state", i+1)
				}
			}
			report, err := invoke()
			if err != nil || (skip && report.Skipped != 1) || (!skip && report.Created != 1) {
				t.Fatal("import retry did not complete the intended operation", err)
			}
			if dryRun && databaseState(t, s) != before {
				t.Fatal("successful dry-run wrote state")
			}
		})
	}
}

func TestConsoleImportCanceledSnapshotCannotMutateState(t *testing.T) {
	s, _, _ := testStore(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	before := databaseState(t, s)
	_, err := s.ImportConsoleAccounts(ctx, importBatch(), true)
	if !errors.Is(err, context.Canceled) || databaseState(t, s) != before {
		t.Fatal("canceled dry-run changed state or lost cancellation")
	}
}
