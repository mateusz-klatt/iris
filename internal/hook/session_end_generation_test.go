package hook

import (
	"context"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/hub"
	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/proc"
	"github.com/mateusz-klatt/iris/internal/store"
)

type delayedSessionEndHub struct {
	*hubclient.Client
	before      func(hubclient.SessionRequest) error
	generations []int
}

func (h *delayedSessionEndHub) RegisterSession(ctx context.Context, req hubclient.SessionRequest) (hubclient.SessionResponse, error) {
	h.generations = append(h.generations, req.Generation)
	if before := h.before; before != nil {
		h.before = nil
		if err := before(req); err != nil {
			return hubclient.SessionResponse{}, err
		}
	}
	return h.Client.RegisterSession(ctx, req)
}

// A SessionEnd that already read the old proof must not discard that proof and
// acquire the replacement session's authority after its registration was delayed.
func TestStaleSessionEndCannotEndAReplacementGeneration(t *testing.T) {
	ctx := context.Background()
	clock := &hookClock{wall: time.Unix(1800000000, 0)}
	s, err := store.Open(filepath.Join(t.TempDir(), "hub.db"), clock)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	server := httptest.NewUnstartedServer(nil)
	server.Config.Handler = hub.New(s, "secret", []string{server.Listener.Addr().String()})
	server.Start()
	defer server.Close()
	client := &hubclient.Client{Config: hubURL(server.URL)}
	root := gitRepo(t)
	req := hubclient.SessionRequest{Client: "claude", Host: "host", SessionID: "session", CWD: root, Worktree: root, Source: "hook"}
	old, err := client.RegisterSession(ctx, req)
	if err != nil || old.Generation != 1 {
		t.Fatalf("initial registration: %+v, err %v", old, err)
	}
	m := newFileMemory(t)
	m.Remember("claude", "session", old.Generation)
	var replacement hubclient.SessionResponse
	h := &delayedSessionEndHub{Client: client, before: func(captured hubclient.SessionRequest) error {
		if captured.Generation != old.Generation {
			return fmt.Errorf("SessionEnd did not capture the old proof: %d", captured.Generation)
		}
		// The old end request is in flight while activity legitimately replaces
		// its expired Execution and remembers generation 2 for the live session.
		clock.advance(16 * time.Minute)
		replacement, err = client.RegisterSession(ctx, req)
		if err != nil {
			return err
		}
		if replacement.Generation != 2 || replacement.ExecutionID == old.ExecutionID {
			return fmt.Errorf("session did not replace the expired generation: %+v", replacement)
		}
		m.Remember("claude", "session", replacement.Generation)
		return client.Do(ctx, "POST", "/v1/claims", map[string]any{"execution_id": replacement.ExecutionID, "generation": replacement.Generation, "key": "held", "kind": "exact", "path": "held.go"}, nil)
	}}
	payload := `{"session_id":"session","cwd":` + quote(root) + `}`
	out := (Runner{ClientName: "claude", EventName: "SessionEnd", Stdin: strings.NewReader(payload), Hub: h, Memory: m, Host: func() string { return "host" }, Ancestor: func() (proc.Ancestor, error) { return proc.Ancestor{}, nil }, Getenv: func(string) string { return "" }, after: never, logAfter: never}).Run()
	assertAllow(t, ClientClaude, out)
	// Before the fix the stale end retried without proof, ended generation 2,
	// and this next live registration advanced again to generation 3.
	after, err := client.RegisterSession(ctx, req)
	if err != nil || after.ExecutionID != replacement.ExecutionID || after.Generation != 2 {
		t.Fatalf("stale SessionEnd replaced a healthy generation: replacement %+v, after %+v, err %v; sent proofs %v", replacement, after, err, h.generations)
	}
	if len(h.generations) != 1 || h.generations[0] != old.Generation {
		t.Fatalf("stale SessionEnd dropped its proof: %v", h.generations)
	}
	if m.Recall("claude", "session") != 2 {
		t.Fatal("the stale end discarded the replacement session's proof")
	}
	if len(after.Claims) != 1 || after.Claims[0].Path != "held.go" || after.Claims[0].Holder != replacement.Agent.Name {
		t.Fatalf("the stale end released the replacement's claim: %+v", after.Claims)
	}
}
