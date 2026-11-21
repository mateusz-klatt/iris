package hubclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TouchFiles sends one tool call's paths grouped by checkout to the grouped route and reads the
// decision; an answer without an action is an error, as for a plain touch.
func TestTouchFilesSendsTheGroupsToTheGroupedRoute(t *testing.T) {
	var got map[string]any
	answer := `{"action":"deny","message":"held"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/claims/touch/files" {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(answer))
	}))
	defer srv.Close()
	c := &Client{Config: staticConfig{url: srv.URL, token: "tok"}}
	req := TouchFilesRequest{ExecutionID: "ex_1", Generation: 2, ToolCallID: "call", Groups: []ClaimGroup{{Worktree: "/src/app/frontend", CommonDir: "/src/app/.git/modules/frontend", RemoteURL: "https://example.com/frontend.git", Paths: []string{"src/x.ts"}}}}
	out, err := c.TouchFiles(context.Background(), req)
	if err != nil || out.Action != "deny" || out.Message != "held" {
		t.Fatalf("%+v %v", out, err)
	}
	group := got["groups"].([]any)[0].(map[string]any)
	if got["tool_call_id"] != "call" || group["worktree"] != "/src/app/frontend" || group["paths"].([]any)[0] != "src/x.ts" || group["worktree_id"] != nil {
		t.Fatalf("sent %v", got)
	}
	answer = `{"message":"no action"}`
	if _, err := c.TouchFiles(context.Background(), req); !errors.Is(err, ErrNoAction) {
		t.Fatalf("an answer without an action: %v", err)
	}
	srv.Close()
	if _, err := c.TouchFiles(context.Background(), req); err == nil {
		t.Fatal("a closed hub answered")
	}
}

// A hub names file_claims when it takes grouped touches.
func TestFileClaimsCapableReadsTheRegistrationAnswer(t *testing.T) {
	if !FileClaimsCapable([]string{"shell_scope", "file_claims"}) || FileClaimsCapable([]string{"shell_scope"}) || FileClaimsCapable(nil) {
		t.Fatal("capability")
	}
}

// A reconcile that names its checkout sends it; one that does not sends what it always did, so
// an older hub reads the same request.
func TestReconcileNamesItsCheckoutOnlyWhenKnown(t *testing.T) {
	var got []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		got = append(got, body)
		_, _ = w.Write([]byte(`{"message":"","conflicts":[]}`))
	}))
	defer srv.Close()
	c := &Client{Config: staticConfig{url: srv.URL, token: "tok"}}
	for _, req := range []ReconcileRequest{
		{ExecutionID: "ex_1", Generation: 1, Kind: "after", Status: map[string]string{}, Worktree: "/src/app", WorktreeID: "dev:1", RemoteURL: "https://example.com/app.git", RootCommit: "root"},
		{ExecutionID: "ex_1", Generation: 1, Kind: "after", Status: map[string]string{}},
	} {
		if _, err := c.Reconcile(context.Background(), req); err != nil {
			t.Fatal(err)
		}
	}
	if got[0]["worktree"] != "/src/app" || got[0]["worktree_id"] != "dev:1" || got[0]["remote_url"] != "https://example.com/app.git" || got[0]["root_commit"] != "root" {
		t.Fatalf("named checkout: %v", got[0])
	}
	if len(got[1]) != 4 {
		t.Fatalf("unnamed checkout sent %v", got[1])
	}
}
