package hubclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// HandOverExecution ends the Execution through its end route, naming the Execution its mail
// goes to.
func TestHandOverExecutionEndsAndNamesTheReceiver(t *testing.T) {
	var path string
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	c := &Client{Config: staticConfig{url: srv.URL, token: "tok"}}
	if err := c.HandOverExecution(context.Background(), "ex_stray", 2, "ex_hook"); err != nil {
		t.Fatal(err)
	}
	if path != "/v1/executions/ex_stray/end" || got["generation"] != float64(2) || got["hand_over"] != "ex_hook" {
		t.Fatalf("%s %v", path, got)
	}
}
