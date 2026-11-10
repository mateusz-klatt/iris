package mcpserver

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/mateusz-klatt/iris/internal/binding"
	"github.com/mateusz-klatt/iris/internal/hubclient"
)

func TestWhoamiUsesAuthoritativeHeartbeatBinding(t *testing.T) {
	f := &fakeHub{reply: `{"execution_id":"ex","generation":3,"repository_id":"merged","agent":{"id":"agent","name":"original-name"},"lease_ms":60000}`}
	srv := httptest.NewServer(f)
	defer srv.Close()
	b := binding.Bound{ExecutionID: "ex", Generation: 3, RepositoryID: "retired", Agent: hubclient.Agent{ID: "agent", Name: "original-name"}, Rung: 4}
	out, err := call(context.Background(), &hubclient.Client{Config: config{srv.URL}}, b, true, "whoami", Input{})
	if err != nil || out["repository_id"] != "merged" || out["execution_id"] != "ex" || out["generation"] != float64(3) || out["binding_rung"] != float64(4) || out["agent"].(map[string]any)["name"] != "original-name" {
		t.Fatal("whoami returned its cached identity", out, err)
	}
}
