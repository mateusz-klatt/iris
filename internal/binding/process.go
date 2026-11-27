package binding

import (
	"context"
	"net/http"
	"net/url"

	"github.com/mateusz-klatt/iris/internal/hubclient"
)

// postProcessBound distinguishes a process lookup from a session-ID lookup of
// the same hook row without changing the request body.
func postProcessBound(ctx context.Context, h *hubclient.Client, path string, req, reply any) error {
	return h.Do(ctx, http.MethodPost, path+"?process_bound=true", req, reply)
}

// SendProcessHeartbeat renews an opted process binding. Unopted callers use the
// ordinary heartbeat route so the legacy request stays byte-for-byte stable.
func SendProcessHeartbeat(ctx context.Context, h *hubclient.Client, executionID string, req hubclient.HeartbeatRequest) (hubclient.Candidate, error) {
	var candidate hubclient.Candidate
	err := postProcessBound(ctx, h, "/v1/executions/"+url.PathEscape(executionID)+"/heartbeat", req, &candidate)
	return candidate, err
}
