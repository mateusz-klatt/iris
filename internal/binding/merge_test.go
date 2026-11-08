package binding

import (
	"context"
	"net/url"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/store"
)

// A running MCP or monitor already knows its Execution before the operator
// repairs a split. That cached binding must follow the preserved Execution.
func TestBoundResolverFollowsRepositoryMerge(t *testing.T) {
	for _, refresh := range []string{"resolve", "heartbeat"} {
		t.Run(refresh, func(t *testing.T) {
			h := newSplitHub(t)
			ctx := context.Background()
			register := func(host string) map[string]any {
				out, err := h.s.Call(ctx, "sessions", store.Request{Client: "codex", Host: host, SessionID: "running", Cwd: "/" + host, RootCommit: "same-root"})
				if err != nil {
					t.Fatal(err)
				}
				return out
			}
			source, destination := register("source"), register("destination")
			client := &hubclient.Client{Config: config{h.url}}
			r := &Resolver{Hub: client, Client: "codex", Host: "source", CWD: "/source", Getenv: func(key string) string {
				if key == "IRIS_EXECUTION" {
					return source["execution_id"].(string)
				}
				return ""
			}}
			before, err := r.Resolve(ctx, "")
			if err != nil || before.RepositoryID != source["repository_id"] {
				t.Fatalf("initial bind: %+v %v", before, err)
			}
			if _, err := h.s.Call(ctx, "claim", store.Request{ExecutionID: destination["execution_id"].(string), Generation: destination["generation"].(int64), Key: "dest", Kind: "exact", Path: "shared.go"}); err != nil {
				t.Fatal(err)
			}
			var result map[string]any
			if err := client.Do(ctx, "POST", "/v1/admin/merge", map[string]any{"from_repository_id": source["repository_id"], "into_repository_id": destination["repository_id"]}, &result); err != nil {
				t.Fatal(err)
			}
			var after Bound
			if refresh == "resolve" {
				after, err = r.Resolve(ctx, "")
				if err != nil {
					t.Fatal(err)
				}
			} else {
				run, stop := context.WithCancel(ctx)
				done := make(chan struct{})
				go func() {
					r.Maintain(run, "shim", time.Millisecond, func() bool { return true }, func() { t.Error("a repository merge stopped the client") })
					close(done)
				}()
				defer func() { stop(); <-done }()
				deadline := time.Now().Add(time.Second)
				for {
					r.mu.Lock()
					after = r.bound
					r.mu.Unlock()
					if after.RepositoryID == destination["repository_id"] || time.Now().After(deadline) {
						break
					}
					time.Sleep(time.Millisecond)
				}
			}
			if after.RepositoryID != destination["repository_id"] || after.ExecutionID != before.ExecutionID || after.Generation != before.Generation || after.Agent != before.Agent || after.Rung != before.Rung {
				t.Fatalf("cached binding stayed split or changed identity: before %+v, after %+v, destination %v", before, after, destination["repository_id"])
			}
			q := url.Values{"repository_id": {after.RepositoryID}}
			if err := client.Do(ctx, "GET", "/v1/agents?"+q.Encode(), nil, &result); err != nil || len(result["agents"].([]any)) != 2 {
				t.Fatalf("refreshed peers: %v %v", result, err)
			}
			if err := client.Do(ctx, "GET", "/v1/claims?"+q.Encode(), nil, &result); err != nil || len(result["claims"].([]any)) != 1 {
				t.Fatalf("refreshed claims: %v %v", result, err)
			}
		})
	}
}
