package hook

import (
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/mateusz-klatt/iris/internal/hubclient"
)

// A payload Iris cannot decode says which tool and fields the client sent, never a value, so
// the next "no session id" names its source. doctor found three such lines from Claude Code's
// PostToolUse with nothing to say which tool they came from.
func TestADecodeFailureRecordsThePayloadShapeWithoutValues(t *testing.T) {
	env := failureEnv(t)
	payload := `{"hook_event_name":"PostToolUse","tool_name":"Mystery","tool_input":{"command":"secret-command"},"cwd":"/secret/path"}`
	out := run(scriptHub{}, "PostToolUse", payload, env)
	assertAllow(t, ClientClaude, out)
	got := waitForFailure(t, env("IRIS_TOKEN_FILE"))
	if got.Stage != "decode" || got.Error != errNoSession.Error() || got.Shape == nil {
		t.Fatalf("%+v", got)
	}
	if got.Shape.Tool != "Mystery" || !slices.Equal(got.Shape.Keys, []string{"cwd", "hook_event_name", "tool_input", "tool_name"}) {
		t.Fatalf("shape %+v", got.Shape)
	}
	if text := fmt.Sprintf("%+v", got); strings.Contains(text, "secret") {
		t.Fatalf("a value reached the log: %s", text)
	}
}

// Only an object has a shape. A tool name that is not a string is left out, and a payload with
// many fields keeps the first 32 names in order, each bounded.
func TestPayloadShapeIsBounded(t *testing.T) {
	for _, raw := range []string{`[1]`, `"text"`, `null`, `{`} {
		if shape := payloadShape([]byte(raw)); shape != nil {
			t.Errorf("%s: %+v", raw, shape)
		}
	}
	if shape := payloadShape([]byte(`{"tool_name":7,"toolName":"Second"}`)); shape.Tool != "Second" {
		t.Errorf("%+v", shape)
	}
	var fields []string
	for i := range 40 {
		fields = append(fields, fmt.Sprintf(`"k%02d":0`, i))
	}
	fields = append(fields, `"`+strings.Repeat("x", 300)+`":0`)
	shape := payloadShape([]byte("{" + strings.Join(fields, ",") + "}"))
	if len(shape.Keys) != shapeKeys || shape.Keys[0] != "k00" || shape.Keys[31] != "k31" {
		t.Fatalf("%d keys: %v", len(shape.Keys), shape.Keys[:3])
	}
	if long := payloadShape([]byte(`{"` + strings.Repeat("y", 300) + `":0}`)); len(long.Keys[0]) > 200 {
		t.Fatalf("a field name of %d bytes was kept", len(long.Keys[0]))
	}
}

// The hub client's own failures, which carry no hub error envelope, each get a category that
// says what failed: during a deploy a proxy answers 502, which was logged as
// "dependency failure (*errors.errorString)".
func TestHubClientFailuresHaveTheirOwnCategories(t *testing.T) {
	cases := map[error]string{
		&hubclient.StatusError{Path: "/v1/shell/reconcile", Code: 502, Status: "502 Bad Gateway"}: "hub answered HTTP 502",
		&url.Error{Op: "Post", URL: "https://secret.example/v1", Err: hubclient.ErrRedirect}:      failHubRedirect,
		fmt.Errorf("hub /v1/sessions: %w", hubclient.ErrResponseTooLarge):                         failHubTooLarge,
		hubclient.ErrNoExecution: failHubNoExecution,
		hubclient.ErrNoAction:    failHubNoAction,
	}
	for err, want := range cases {
		if got := FailureCategory(failureText(err)); got != want {
			t.Errorf("FailureCategory(failureText(%v)) = %q, want %q", err, got, want)
		}
	}
	for _, text := range []string{"hub answered HTTP 5020", "hub answered HTTP 50", "hub answered HTTP abc", "hub answered HTTP 099", "hub answered HTTP 600", "hub answered HTTP +50"} {
		if got := FailureCategory(text); got != "other" {
			t.Errorf("FailureCategory(%q) = %q, want other", text, got)
		}
	}
	if (&hubclient.StatusError{Path: "/v1/x", Status: "503 Service Unavailable"}).Error() != "hub /v1/x: 503 Service Unavailable" {
		t.Error("StatusError text changed")
	}
	if !errors.Is(fmt.Errorf("wrapped: %w", hubclient.ErrNoAction), hubclient.ErrNoAction) {
		t.Error("ErrNoAction does not unwrap")
	}
}
