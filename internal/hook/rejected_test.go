package hook

import (
	"strings"
	"testing"

	"github.com/mateusz-klatt/iris/internal/hubclient"
)

// A rejected token still fails open, as D3 requires, but the model is told, because a silent
// allow looked exactly like protection when a host's token was revoked.
func TestARejectedTokenAllowsAndTellsTheModel(t *testing.T) {
	root := gitRepo(t)
	refused := &hubclient.Error{Code: "unauthorized", Message: "revoked token"}
	edit := `{"session_id":"s","cwd":` + quote(root) + `,"tool_name":"Write","tool_input":{"file_path":"a.go"}}`
	start := `{"session_id":"s","cwd":` + quote(root) + `}`
	for name, tc := range map[string]struct {
		hub     scriptHub
		event   string
		payload string
	}{
		"edit, at session": {scriptHub{regErr: refused}, "PreToolUse", edit},
		"edit, at touch":   {scriptHub{touchErr: refused}, "PreToolUse", edit},
		"session start":    {scriptHub{regErr: refused}, "SessionStart", start},
	} {
		t.Run(name, func(t *testing.T) {
			env := failureEnv(t)
			out := run(tc.hub, tc.event, tc.payload, env)
			text := string(out.Stdout)
			if !strings.Contains(text, "rejected this host's token") || strings.Contains(text, "deny") || out.Exit != 0 {
				t.Fatalf("%s", text)
			}
			if strings.Contains(text, "revoked token") {
				t.Fatal("the hub's message reached the model")
			}
			if got := waitForFailure(t, env("IRIS_TOKEN_FILE")); !strings.Contains(got.Error, "unauthorized") {
				t.Fatalf("the failure was not recorded: %+v", got)
			}
		})
	}
	// Any other hub failure keeps the plain allow.
	out := run(scriptHub{regErr: &hubclient.Error{Code: "unavailable"}}, "PreToolUse", edit, failureEnv(t))
	assertAllow(t, ClientClaude, out)
}
