package hub

import (
	"encoding/json"
	"testing"
)

// GET /v1/ui/deliveries serves the store's delivery-problems list, filtered by the repository
// in the query, with the thresholds it used.
func TestDeliveryProblemsRoute(t *testing.T) {
	h, s := setup(t)
	h.console = newConsoleAuth(make([]byte, 32))
	_, cookie, csrf := routeConsoleAccount(t, h, "Delivery operator", "operator")
	ui := func(method, path string, body any, status int) map[string]any {
		t.Helper()
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		return consoleRoute(t, h, s, cookie, csrf, method, path, string(raw), status)
	}
	a := register(t, s, 1)
	repo := a["repository_id"].(string)
	ui("POST", "/v1/ui/messages", map[string]any{"key": "k", "repository_id": repo, "to": []string{a["agent"].(map[string]any)["name"].(string)}, "subject": "s", "body": "b"}, 200)
	request(t, s, "POST", "/v1/executions/"+a["execution_id"].(string)+"/end", authority(a), 200)
	got := ui("GET", "/v1/ui/deliveries?repository_id="+repo, nil, 200)
	messages := got["messages"].([]any)
	if len(messages) != 1 || got["wait_s"] != float64(900) || got["window_s"] != float64(7*24*3600) || got["more"] != false {
		t.Fatal(got)
	}
	ui("GET", "/v1/ui/deliveries?repository_id=rp_other", nil, 404)
}
