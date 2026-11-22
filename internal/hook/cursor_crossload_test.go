package hook

import (
	"strings"
	"testing"

	"github.com/mateusz-klatt/iris/internal/proc"
)

// These inputs are synthetic. The isolated Cursor run yielded no readable payload; no fixture is claimed.
func TestCursorCrossLoadedClaudeHookNeedsNoConversationVariable(t *testing.T) {
	for _, tc := range []struct {
		name     string
		ancestor proc.Ancestor
		markers  bool
	}{
		{name: "cursor ancestor", ancestor: proc.Ancestor{PID: 42, StartMS: 7, Name: "/opt/cursor-agent"}},
		{name: "cursor environment", markers: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			token := failureEnv(t)("IRIS_TOKEN_FILE")
			calls := 0
			out := Runner{
				ClientName: "claude",
				EventName:  "PreToolUse",
				Stdin:      strings.NewReader(editPayload(ClientClaude, gitRepo(t))),
				Getenv: func(key string) string {
					values := map[string]string{"IRIS_TOKEN_FILE": token}
					if tc.markers {
						values["CURSOR_PROJECT_DIR"] = "/scratch/project"
						values["CURSOR_VERSION"] = "2026.11.22-e9042b2"
					}
					return values[key]
				},
				Ancestor: func() (proc.Ancestor, error) { return tc.ancestor, nil },
				Hub:      &countingHub{fn: func() { calls++ }},
				after:    never,
				logAfter: never,
			}.Run()
			assertAllow(t, ClientCursor, out)
			if calls != 0 {
				t.Fatalf("cross-loaded hook made %d hub calls without CURSOR_CONVERSATION_ID", calls)
			}
			got := observedOnly(t, token)
			if got.Client != "claude" || got.Error != "cross-load: ran in a session of cursor" {
				t.Fatalf("%+v", got)
			}
		})
	}
}

func TestCursorMarkersDoNotDisplaceANativeClaudeHook(t *testing.T) {
	calls := 0
	out := Runner{
		ClientName: "claude",
		EventName:  "PreToolUse",
		Stdin:      strings.NewReader(editPayload(ClientClaude, gitRepo(t))),
		Getenv: func(key string) string {
			return map[string]string{
				"CURSOR_PROJECT_DIR": "/outer/cursor/project",
				"CURSOR_VERSION":     "2026.11.22-e9042b2",
			}[key]
		},
		Ancestor: func() (proc.Ancestor, error) {
			return proc.Ancestor{PID: 43, StartMS: 8, Name: "claude"}, nil
		},
		Hub:   denyHub{calls: &calls},
		after: never,
	}.Run()
	if calls == 0 || !denied(ClientClaude, out) {
		t.Fatalf("native Claude was displaced: calls %d, output %+v", calls, out)
	}
}
