package hook

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mateusz-klatt/iris/internal/proc"
)

// The two proofs that do not need CURSOR_CONVERSATION_ID. The stdin they accept
// is the real capture in TestCursorCrossLoadedClaudePluginCapture.
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

// TestCursorCrossLoadedClaudePluginCapture replays stdin captured from
// cursor-agent 2026.11.22-e9042b2 while it ran `iris hook --client claude`.
// The payload is Cursor-shaped and has no cwd. The hook environment had
// CURSOR_PROJECT_DIR and CURSOR_VERSION and did not have CURSOR_CONVERSATION_ID.
func TestCursorCrossLoadedClaudePluginCapture(t *testing.T) {
	for _, tc := range []struct {
		file, event, hook string
	}{
		{"claude-plugin-SessionStart.json", "SessionStart", "sessionStart"},
		{"claude-plugin-SessionEnd.json", "SessionEnd", "sessionEnd"},
	} {
		t.Run(tc.event, func(t *testing.T) {
			payload, err := os.ReadFile(filepath.Join("testdata", "cursor", tc.file))
			if err != nil {
				t.Fatal(err)
			}
			var raw struct {
				Hook    string `json:"hook_event_name"`
				CWD     string `json:"cwd"`
				Session string `json:"session_id"`
			}
			if err := json.Unmarshal(payload, &raw); err != nil {
				t.Fatal(err)
			}
			if raw.Hook != tc.hook || raw.CWD != "" || raw.Session != cursorPluginSession {
				t.Fatalf("capture shape hook %q cwd %q session %q", raw.Hook, raw.CWD, raw.Session)
			}
			ev, err := Decode(ClientClaude, tc.event, payload)
			if err != nil || ev.Kind != kindFrom(tc.event, "") || ev.SessionID != raw.Session || ev.CWD != "" {
				t.Fatalf("claude decode %+v err %v", ev, err)
			}
			token := failureEnv(t)("IRIS_TOKEN_FILE")
			calls := 0
			out := Runner{
				ClientName: "claude",
				EventName:  tc.event,
				Stdin:      strings.NewReader(string(payload)),
				Getenv: func(key string) string {
					switch key {
					case "IRIS_TOKEN_FILE":
						return token
					case "CURSOR_PROJECT_DIR":
						return cursorPluginRoot
					case "CURSOR_VERSION":
						return "2026.11.22-e9042b2"
					default:
						return ""
					}
				},
				Ancestor: func() (proc.Ancestor, error) {
					return proc.Ancestor{PID: 42, StartMS: 7, Name: "cursor-agent"}, nil
				},
				Hub:      &countingHub{fn: func() { calls++ }},
				after:    never,
				logAfter: never,
			}.Run()
			assertAllow(t, ClientCursor, out)
			if calls != 0 {
				t.Fatalf("cross-loaded hook made %d hub calls", calls)
			}
			got := observedOnly(t, token)
			if got.Client != "claude" || got.Error != "cross-load: ran in a session of cursor" {
				t.Fatalf("%+v", got)
			}
		})
	}
}
