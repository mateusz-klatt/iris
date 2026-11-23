package hub

import (
	"context"
	"testing"

	"github.com/mateusz-klatt/iris/internal/store"
)

func TestSucceedRouteDispatchesExactProof(t *testing.T) {
	h, server := setup(t)
	var operation string
	var seen store.Request
	h.call = func(_ context.Context, op string, r store.Request) (map[string]any, error) {
		operation, seen = op, r
		return map[string]any{"execution_id": "ex_successor", "generation": int64(2)}, nil
	}
	out := request(t, server, "POST", "/v1/sessions/succeed", map[string]any{
		"client": "claude", "host": "host", "session_id": "S", "source": "shim",
		"prior_generation": 1, "prior_execution_id": "ex_prior",
	}, 200)
	if out["execution_id"] != "ex_successor" || operation != "succeed" {
		t.Fatalf("route response=%v operation=%q", out, operation)
	}
	if seen.Client != "claude" || seen.Host != "host" || seen.SessionID != "S" || seen.Source != "shim" || seen.PriorGeneration != 1 || seen.PriorExecutionID != "ex_prior" {
		t.Fatalf("succession request lost its identity or proof: %+v", seen)
	}
}
