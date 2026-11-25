package hook

import (
	"strings"
	"testing"

	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/proc"
)

func TestGrokAgentAliasRegisters(t *testing.T) {
	for _, exe := range []string{
		"/home/user/.grok/downloads/grok-1.0.92-linux-x86_64",
		"/Users/user/.grok/downloads/grok-1.0.92-macos-x86_64",
		"/home/user/.grok/bin/agent",
		`C:\Users\me\.grok\downloads\grok-1.0.92-windows-x86_64.exe`,
	} {
		t.Run(exe, func(t *testing.T) {
			var registered hubclient.SessionRequest
			out := grokRun(t, grokGetenv(t, map[string]string{
				"CURSOR_PROJECT_DIR":     "/outer/cursor",
				"CURSOR_VERSION":         "2026.11.22-e9042b2",
				"CLAUDE_CODE_SESSION_ID": "inherited-from-claude",
			}), func() (proc.Ancestor, error) {
				return proc.Ancestor{PID: 4242, StartMS: 1700000000000, Name: "agent", Exe: exe}, nil
			}, "SessionStart", `{"session_id":"`+grokSession+`","cwd":"ROOT","hook_event_name":"SessionStart"}`, scriptHub{sess: &registered})
			if out.Exit != 0 || !strings.Contains(string(out.Stdout), "Iris handle:") {
				t.Fatalf("grok agent did not register: %#v", out)
			}
			if registered.Client != "grok" || registered.SessionID != grokSession || registered.ClientPID != 4242 {
				t.Fatalf("registration %+v", registered)
			}
		})
	}
}

func TestUnidentifiedAgentHookAssignsNoClient(t *testing.T) {
	root := gitRepo(t)
	ancestor := func() (proc.Ancestor, error) {
		return proc.Ancestor{PID: 42, StartMS: 7, Name: "/opt/agent"}, nil
	}
	for _, tc := range []struct {
		name, client, event, payload string
		allow                        Client
	}{
		{
			name: "decoded claude hook", client: "claude", event: "PreToolUse", allow: ClientClaude,
			payload: editPayload(ClientClaude, root),
		},
		{
			name: "undecodable claude hook", client: "claude", event: "PreToolUse", allow: ClientClaude,
			payload: "{",
		},
		{
			name: "decoded cursor hook", client: "cursor", event: "sessionStart", allow: ClientCursor,
			payload: `{"session_id":"s1","hook_event_name":"sessionStart","workspace_roots":["/workspace/project"]}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			token := failureEnv(t)("IRIS_TOKEN_FILE")
			calls := 0
			out := Runner{
				ClientName: tc.client,
				EventName:  tc.event,
				Stdin:      strings.NewReader(tc.payload),
				Getenv: func(key string) string {
					if key == "IRIS_TOKEN_FILE" {
						return token
					}
					if key == "CLAUDE_CODE_SESSION_ID" {
						return "inherited-parent-claude"
					}
					return ""
				},
				Ancestor: ancestor,
				Hub:      &countingHub{fn: func() { calls++ }},
				after:    never,
				logAfter: never,
			}.Run()
			assertAllow(t, tc.allow, out)
			if calls != 0 {
				t.Fatalf("unidentified agent made %d hub calls", calls)
			}
			if got := observedOnly(t, token); got.Error != unidentifiedAgentObservation {
				t.Fatalf("%+v", got)
			}
		})
	}
	runner, cross := (Runner{Ancestor: ancestor}).crossLoad("s1", ClientClaude)
	if !cross || runner != "" {
		t.Fatalf("runner %q cross %v", runner, cross)
	}
	runner, cross = (Runner{
		Getenv: func(key string) string {
			return map[string]string{"CURSOR_PROJECT_DIR": "/project", "CURSOR_VERSION": "2026.11.22"}[key]
		},
		Ancestor: ancestor,
	}).crossLoad("s1", ClientCursor)
	if cross || runner != "" {
		t.Fatalf("Cursor markers on an unresolved agent: runner %q cross %v", runner, cross)
	}
}

func TestGrokNameIsUnchanged(t *testing.T) {
	runner, cross := (Runner{Ancestor: func() (proc.Ancestor, error) {
		return proc.Ancestor{Name: "grok"}, nil
	}}).crossLoad("s1", ClientGrok)
	if cross || runner != "" {
		t.Fatalf("grok was treated as a cross-load: %q %v", runner, cross)
	}
	runner, cross = (Runner{Ancestor: func() (proc.Ancestor, error) {
		return proc.Ancestor{Name: "cursor-agent"}, nil
	}}).crossLoad("s1", ClientClaude)
	if !cross || runner != ClientCursor {
		t.Fatalf("cursor-agent: %q %v", runner, cross)
	}
}
