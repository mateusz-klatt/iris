package hook

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/mateusz-klatt/iris/internal/hubclient"
)

type decoded struct {
	kind    Kind
	session string
	parent  string
	cwd     string
	tool    string
	class   ToolClass
	paths   []string
	command string
	call    string
	unknown bool
}

func TestDecodeFixtures(t *testing.T) {
	root := "testdata"
	seen := 0
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".json") {
			return err
		}
		rel := strings.TrimPrefix(path, root+string(filepath.Separator))
		parts := strings.Split(rel, string(filepath.Separator))
		if len(parts) != 2 {
			t.Fatalf("unexpected path %s", rel)
		}
		client, file := parts[0], parts[1]
		want, ok := fixtureWant[client+"/"+file]
		if !ok {
			t.Fatalf("no expectation for %s", rel)
		}
		payload, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		ev, err := Decode(Canonical(client), argvEvent(client, file), payload)
		if err != nil {
			t.Fatalf("%s: %v", rel, err)
		}
		if ev.Client != Canonical(client) || ev.Kind != want.kind || ev.SessionID != want.session {
			t.Errorf("%s: client %s kind %s session %s", rel, ev.Client, ev.Kind, ev.SessionID)
		}
		if ev.ParentSessionID != want.parent || ev.CWD != want.cwd {
			t.Errorf("%s: parent %q cwd %q", rel, ev.ParentSessionID, ev.CWD)
		}
		if ev.ToolName != want.tool || ev.ToolClass != want.class || ev.UnknownTool != want.unknown {
			t.Errorf("%s: tool %s class %s unknown %v", rel, ev.ToolName, ev.ToolClass, ev.UnknownTool)
		}
		if ev.ToolCallID != want.call || ev.Command != want.command || !samePaths(ev.Paths, want.paths) {
			t.Errorf("%s: call %q command %q paths %q", rel, ev.ToolCallID, ev.Command, ev.Paths)
		}
		seen++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if seen != len(fixtureWant) {
		t.Fatalf("decoded %d fixtures, table has %d", seen, len(fixtureWant))
	}
}

func TestEncodeGolden(t *testing.T) {
	pre := Event{Kind: KindPreTool}
	start := Event{Kind: KindSessionStart}
	cases := []struct {
		client  Client
		allow   string
		deny    string
		context string
		denyEx  int
	}{
		{ClientClaude, "{}\n",
			"{\"hookSpecificOutput\":{\"hookEventName\":\"PreToolUse\",\"permissionDecision\":\"deny\",\"permissionDecisionReason\":\"held\"}}\n",
			"{\"hookSpecificOutput\":{\"hookEventName\":\"SessionStart\",\"additionalContext\":\"ctx\"}}\n", 0},
		{ClientCodex, "{}\n",
			"{\"hookSpecificOutput\":{\"hookEventName\":\"PreToolUse\",\"permissionDecision\":\"deny\",\"permissionDecisionReason\":\"held\"}}\n",
			"{\"hookSpecificOutput\":{\"hookEventName\":\"SessionStart\",\"additionalContext\":\"ctx\"}}\n", 0},
		{ClientCopilot, "{}\n",
			"{\"permissionDecision\":\"deny\",\"permissionDecisionReason\":\"held\"}\n",
			"{\"additionalContext\":\"ctx\"}\n", 0},
		{ClientCursor, "{\"permission\":\"allow\"}\n",
			"{\"permission\":\"deny\",\"user_message\":\"held\",\"agent_message\":\"held\"}\n",
			"{\"additional_context\":\"ctx\"}\n", 0},
		{ClientAntigravity, "{\"decision\":\"allow\"}\n",
			"{\"decision\":\"deny\",\"reason\":\"held\"}\n",
			"{\"injectSteps\":[{\"systemMessage\":{\"systemMessage\":\"ctx\"}}]}\n", 0},
		{ClientGrok, "{}\n",
			"{\"decision\":\"deny\",\"reason\":\"held\"}\n",
			"{\"systemMessage\":\"ctx\"}\n", 2},
		{ClientKimi, "", "", "ctx\n", 2},
	}
	for _, tc := range cases {
		pre.Client = tc.client
		start.Client = tc.client
		allow := Encode(pre, Decision{Action: ActionAllow})
		deny := Encode(pre, Decision{Action: ActionDeny, Reason: "held"})
		ctx := Encode(start, Decision{Action: ActionAllow, Context: "ctx"})
		if string(allow.Stdout) != tc.allow || allow.Exit != 0 {
			t.Errorf("%s allow %q exit %d", tc.client, allow.Stdout, allow.Exit)
		}
		if string(deny.Stdout) != tc.deny || deny.Exit != tc.denyEx {
			t.Errorf("%s deny stdout %q stderr %q exit %d", tc.client, deny.Stdout, deny.Stderr, deny.Exit)
		}
		if tc.client == ClientKimi {
			if string(deny.Stderr) != "held\n" || len(deny.Stdout) != 0 {
				t.Errorf("kimi deny stdout %q stderr %q", deny.Stdout, deny.Stderr)
			}
			if deny.Exit != 2 || ctx.Exit != 0 || string(ctx.Stdout) != "ctx\n" {
				t.Errorf("kimi context %+v", ctx)
			}
			continue
		}
		if string(ctx.Stdout) != tc.context || ctx.Exit != 0 {
			t.Errorf("%s context %q exit %d", tc.client, ctx.Stdout, ctx.Exit)
		}
	}
}

func TestAskBecomesDeny(t *testing.T) {
	codex := Encode(Event{Client: ClientCodex, Kind: KindPreTool}, Decision{Action: ActionAsk, Reason: "held"})
	want := "{\"hookSpecificOutput\":{\"hookEventName\":\"PreToolUse\",\"permissionDecision\":\"deny\",\"permissionDecisionReason\":\"held Ask the human how to proceed.\"}}\n"
	if string(codex.Stdout) != want || codex.Exit != 0 {
		t.Fatalf("codex ask %q exit %d", codex.Stdout, codex.Exit)
	}
	kimi := Encode(Event{Client: ClientKimi, Kind: KindPreTool}, Decision{Action: ActionAsk, Reason: "held"})
	if kimi.Exit != 2 || string(kimi.Stderr) != "held Ask the human how to proceed.\n" {
		t.Fatalf("kimi ask %+v", kimi)
	}
	cursor := Encode(Event{Client: ClientCursor, Kind: KindPreTool}, Decision{Action: ActionAsk, Reason: "held"})
	if string(cursor.Stdout) != "{\"permission\":\"ask\",\"user_message\":\"held\",\"agent_message\":\"held\"}\n" || cursor.Exit != 0 {
		t.Fatalf("cursor ask %q", cursor.Stdout)
	}
}

func TestEncodeDoesNotEscapeHTML(t *testing.T) {
	out := Encode(Event{Client: ClientClaude, Kind: KindPreTool}, Decision{Action: ActionDeny, Reason: "a<b&c"})
	if !strings.Contains(string(out.Stdout), "a<b&c") {
		t.Fatalf("%s", out.Stdout)
	}
}

func sessionWithManyPeers() hubclient.SessionResponse {
	s := hubclient.SessionResponse{
		ExecutionID: "ex_1",
		Handle:      "ex_1",
		Unread:      4,
		Claims: []hubclient.Claim{{
			Path: "src/a.go", Holder: "codex-host", Origin: "auto", ExpiresAt: "14:32",
		}},
	}
	s.Agent.Name = "claude-host-iris"
	for i := 0; i < 80; i++ {
		s.Peers = append(s.Peers, hubclient.Peer{
			Name: strings.Repeat("p", 40) + string(rune('a'+i%26)), Client: "codex", State: "active", Branch: "feature/long",
		})
	}
	return s
}

func TestSessionTextLimit(t *testing.T) {
	s := sessionWithManyPeers()
	text := SessionText(s)
	if len(text) > 2048 {
		t.Fatalf("len %d", len(text))
	}
	if !strings.Contains(text, "Iris handle: ex_1") || !strings.Contains(text, "untrusted data") || !strings.Contains(text, "Iris notification") {
		t.Fatalf("%s", text)
	}
	if !strings.Contains(text, "Unread messages: 4") {
		t.Fatalf("%s", text)
	}
}

func samePaths(got, want []string) bool {
	if len(got) == 0 && len(want) == 0 {
		return true
	}
	return reflect.DeepEqual(got, want)
}

func argvEvent(client, file string) string {
	name := strings.TrimSuffix(file, ".json")
	switch client {
	case "antigravity":
		switch {
		case strings.HasPrefix(name, "pre_tool_use"):
			return "PreToolUse"
		case strings.HasPrefix(name, "post_tool_use"):
			return "PostToolUse"
		case name == "pre_invocation":
			return "PreInvocation"
		case name == "post_invocation":
			return "PostInvocation"
		case name == "session_start":
			return "SessionStart"
		default:
			return "Stop"
		}
	case "copilot":
		switch {
		case strings.HasPrefix(name, "postToolUseFailure"):
			return "postToolUseFailure"
		case strings.HasPrefix(name, "postToolUse"):
			return "postToolUse"
		case strings.HasPrefix(name, "preToolUse"):
			return "preToolUse"
		default:
			return name
		}
	case "kimi":
		switch {
		case strings.HasPrefix(name, "pre_tool_use"):
			return "PreToolUse"
		case name == "posttooluse":
			return "PostToolUse"
		case name == "sessionstart":
			return "SessionStart"
		default:
			return "UserPromptSubmit"
		}
	case "cursor":
		return name
	default:
		if i := strings.IndexByte(name, '-'); i > 0 {
			return name[:i]
		}
		return name
	}
}

const (
	claudeSession = "b9f244d4-4326-45bf-8c3d-1d249f8f21b2"
	codexSession  = "01a1adf0-8c32-7dd1-915f-250ba6059254"
	codexJS       = "01a1b67e-ac49-7970-8edf-65c03ce13fac"
	copilotA      = "05a1f795-918d-4cfb-aeb2-f17be274e1b8"
	copilotB      = "5371905e-5625-4681-bcbc-73b28e2669bb"
	kimiSession   = "session_114df8a5-1263-4744-bd4a-17c39274ffd6"
	grokParent    = "01a1add0-deb3-7f23-969a-06a73e70b876"
	grokChild     = "01a1add0-f889-76d3-9b16-1775afd996e6"
	grokShell     = "01a1add0-8fa6-7132-9db2-85ef40676904"
	grokMain      = "01a1add1-712e-7e22-a2a9-97ad1e9005b9"
	agySession    = "4d437c2d-a30c-4479-bc04-0c09ca6ebdc5"
	cursorSession = "<SESSION_OR_TOOL_ID>"
	proj          = "/workspace/project"
	// Captured from Claude Code 2.1.319 on Windows 11, where the PowerShell tool exists.
	claudeWindowsSession = "a2a33065-7989-4987-bb5f-8f6866658a25"
	projWindows          = `C:\workspace\project`
	// Captured from Claude Code 2.1.319 on Windows 11 for its built-in Monitor, TaskStop and
	// ScheduleWakeup tools.
	claudeToolsSession  = "76d28899-a30f-4b4f-9598-1fce6bc60152"
	claudeWakeupSession = "af647fc5-1139-4ada-b6da-902ec2615848"
	// Captured from Codex 0.167.0 on Linux on 2026-11-10 (ADR 017's measurement B): a thread
	// the app-server daemon loaded, and the root of a session that spawned a sub-agent.
	codexDaemonRoot   = "01a1c5fb-ea8a-7842-b3cd-68cb0e300fd9"
	codexSubagentRoot = "01a1c616-e474-73a1-9c54-d71509806f1a"
)

var fixtureWant = map[string]decoded{
	"antigravity/post_invocation.json":      {kind: KindPostInvocation, session: agySession, cwd: proj},
	"antigravity/post_tool_use.json":        {kind: KindPostTool, session: agySession, cwd: proj, tool: "write_file", class: ClassEdit, paths: []string{"/workspace/project/test_edit.txt"}, call: "3"},
	"antigravity/pre_invocation.json":       {kind: KindPreInvocation, session: agySession, cwd: proj},
	"antigravity/pre_tool_use.json":         {kind: KindPreTool, session: agySession, cwd: proj, tool: "write_file", class: ClassEdit, paths: []string{"/workspace/project/test_edit.txt"}, call: "3"},
	"antigravity/pre_tool_use_command.json": {kind: KindPreTool, session: agySession, cwd: proj, tool: "run_command", class: ClassShell, command: "ls -l test_edit.txt", call: "5"},
	"antigravity/session_start.json":        {kind: KindSessionStart, session: agySession, cwd: proj},
	"antigravity/stop.json":                 {kind: KindStop, session: agySession, cwd: proj},

	"claude/PostToolUse-bash.json":                   {kind: KindPostTool, session: claudeSession, cwd: proj, tool: "Bash", class: ClassShell, command: "printf shell_ok", call: "toolu_01ECwF1dVgjgN9pZRc5Veud4"},
	"claude/PostToolUse-mcp__iris_probe__probe.json": {kind: KindPostTool, session: claudeSession, cwd: proj, tool: "mcp__iris-probe__probe", class: ClassMCP, call: "toolu_01VZQc8PJf7R4RUQzfncvpbL"},
	"claude/PostToolUse-toolsearch.json":             {kind: KindPostTool, session: claudeSession, cwd: proj, tool: "ToolSearch", class: ClassOther, call: "toolu_013NVHcqMWYvDzyw35RYC4o1"},
	"claude/PostToolUse-write.json":                  {kind: KindPostTool, session: claudeSession, cwd: proj, tool: "Write", class: ClassEdit, paths: []string{"/workspace/project/allowed.txt"}, call: "toolu_01C7qtJco6B2Sdj5LxibnJBp"},
	"claude/PreToolUse-bash.json":                    {kind: KindPreTool, session: claudeSession, cwd: proj, tool: "Bash", class: ClassShell, command: "printf shell_ok", call: "toolu_01ECwF1dVgjgN9pZRc5Veud4"},
	"claude/PostToolUse-powershell.json":             {kind: KindPostTool, session: claudeWindowsSession, cwd: projWindows, tool: "PowerShell", class: ClassShell, command: "Set-Content -Path note.txt -Value hi", call: "toolu_019CphLqu8vVcvbw3YHBShVL"},
	"claude/PreToolUse-powershell.json":              {kind: KindPreTool, session: claudeWindowsSession, cwd: projWindows, tool: "PowerShell", class: ClassShell, command: "Set-Content -Path note.txt -Value hi", call: "toolu_019CphLqu8vVcvbw3YHBShVL"},
	"claude/PreToolUse-mcp__iris_probe__probe.json":  {kind: KindPreTool, session: claudeSession, cwd: proj, tool: "mcp__iris-probe__probe", class: ClassMCP, call: "toolu_01VZQc8PJf7R4RUQzfncvpbL"},
	"claude/PreToolUse-read.json":                    {kind: KindPreTool, session: claudeSession, cwd: proj, tool: "Read", class: ClassRead, paths: []string{"/workspace/project/fixtures/denied-target.txt"}, call: "toolu_01RPA5VWZ24nnjpDeyBs5xBZ"},
	"claude/PreToolUse-toolsearch.json":              {kind: KindPreTool, session: claudeSession, cwd: proj, tool: "ToolSearch", class: ClassOther, call: "toolu_013NVHcqMWYvDzyw35RYC4o1"},
	"claude/PreToolUse-write.json":                   {kind: KindPreTool, session: claudeSession, cwd: proj, tool: "Write", class: ClassEdit, paths: []string{"/workspace/project/allowed.txt"}, call: "toolu_01C7qtJco6B2Sdj5LxibnJBp"},
	"claude/PostToolUse-monitor.json":                {kind: KindPostTool, session: claudeToolsSession, cwd: `C:\workspace\project`, tool: "Monitor", class: ClassShell, command: "sleep 120", call: "toolu_01F9im1LV2oGvR4nQHP6cvTD"},
	"claude/PreToolUse-monitor.json":                 {kind: KindPreTool, session: claudeToolsSession, cwd: `C:\workspace\project`, tool: "Monitor", class: ClassShell, command: "sleep 120", call: "toolu_01F9im1LV2oGvR4nQHP6cvTD"},
	"claude/PostToolUse-taskstop.json":               {kind: KindPostTool, session: claudeToolsSession, cwd: `C:\workspace\project`, tool: "TaskStop", class: ClassOther, call: "toolu_01ML52guJBoybuMwGLdoWYyV"},
	"claude/PreToolUse-taskstop.json":                {kind: KindPreTool, session: claudeToolsSession, cwd: `C:\workspace\project`, tool: "TaskStop", class: ClassOther, call: "toolu_01ML52guJBoybuMwGLdoWYyV"},
	"claude/PostToolUse-schedulewakeup.json":         {kind: KindPostTool, session: claudeWakeupSession, cwd: `C:\workspace\project`, tool: "ScheduleWakeup", class: ClassOther, call: "toolu_01Hdf8TP1aVtHQ33idivgnvE"},
	"claude/PreToolUse-schedulewakeup.json":          {kind: KindPreTool, session: claudeWakeupSession, cwd: `C:\workspace\project`, tool: "ScheduleWakeup", class: ClassOther, call: "toolu_01Hdf8TP1aVtHQ33idivgnvE"},
	"claude/SessionEnd.json":                         {kind: KindSessionEnd, session: claudeSession, cwd: proj},
	"claude/SessionStart.json":                       {kind: KindSessionStart, session: claudeSession, cwd: proj},
	"claude/Stop.json":                               {kind: KindStop, session: claudeSession, cwd: proj},
	"claude/UserPromptSubmit.json":                   {kind: KindPrompt, session: claudeSession, cwd: proj},

	"codex/PostToolUse-apply_patch.json":  {kind: KindPostTool, session: codexSession, cwd: proj, tool: "apply_patch", class: ClassEdit, paths: []string{"allowed.txt"}, call: "exec-9935dec0-9319-4b3f-a7d9-2dd761d9377b"},
	"codex/PostToolUse-bash.json":         {kind: KindPostTool, session: codexSession, cwd: proj, tool: "Bash", class: ClassShell, command: "pwd", call: "exec-64f22d9f-1034-4aed-8495-14643167a9ee"},
	"codex/PreToolUse-apply_patch.json":   {kind: KindPreTool, session: codexSession, cwd: proj, tool: "apply_patch", class: ClassEdit, paths: []string{"allowed.txt"}, call: "exec-9935dec0-9319-4b3f-a7d9-2dd761d9377b"},
	"codex/PreToolUse-bash.json":          {kind: KindPreTool, session: codexSession, cwd: proj, tool: "Bash", class: ClassShell, command: "pwd", call: "exec-64f22d9f-1034-4aed-8495-14643167a9ee"},
	"codex/PreToolUse-node_repl_js.json":  {kind: KindPreTool, session: codexJS, cwd: proj, tool: "mcp__node_repl__js", class: ClassShell, command: "const fs = await import(\"node:fs/promises\"); await fs.appendFile(\"free.txt\", \"viajs\\n\"); nodeRepl.write(\"appended\");", call: "exec-6efe95d3-edeb-434f-a54f-98108426ad80"},
	"codex/PostToolUse-node_repl_js.json": {kind: KindPostTool, session: codexJS, cwd: proj, tool: "mcp__node_repl__js", class: ClassShell, command: "const fs = await import(\"node:fs/promises\"); await fs.appendFile(\"free.txt\", \"viajs\\n\"); nodeRepl.write(\"appended\");", call: "exec-6efe95d3-edeb-434f-a54f-98108426ad80"},
	"codex/SessionEnd.json":               {kind: KindSessionEnd, session: codexSession, cwd: proj},
	"codex/SessionStart.json":             {kind: KindSessionStart, session: codexSession, cwd: proj},
	"codex/SessionStart-resume.json":      {kind: KindSessionStart, session: codexDaemonRoot, cwd: proj},
	"codex/Stop.json":                     {kind: KindStop, session: codexSession, cwd: proj},
	"codex/UserPromptSubmit.json":         {kind: KindPrompt, session: codexSession, cwd: proj},
	// A queued turn's prompt hook, from the run in docs/research/clients/codex-mcp-queue-2026-11-10.md:
	// its prompt is the queued text, unchanged.
	"codex/UserPromptSubmit-queued.json": {kind: KindPrompt, session: codexDaemonRoot, cwd: proj},
	// A sub-agent's hooks carry the root's session id and name the sub-agent as agent_id, which
	// the decoder does not read: they decode to the root session, with no parent (ADR 017).
	"codex/SubagentStart.json": {kind: KindSubagentStart, session: codexSubagentRoot, cwd: proj},
	"codex/SubagentStop.json":  {kind: KindSubagentStop, session: codexSubagentRoot, cwd: proj},

	"copilot/agentStop.json":                           {kind: KindStop, session: copilotA, cwd: proj},
	"copilot/postToolUse-apply_patch.json":             {kind: KindPostTool, session: copilotA, cwd: proj, tool: "apply_patch", class: ClassEdit, paths: []string{"allowed.txt"}},
	"copilot/postToolUse-bash.json":                    {kind: KindPostTool, session: copilotA, cwd: proj, tool: "bash", class: ClassShell, command: "pwd && git status --short && ls -la fixtures"},
	"copilot/postToolUse-iris_probe_probe.json":        {kind: KindPostTool, session: copilotA, cwd: proj, tool: "iris-probe-probe", class: ClassOther, unknown: true},
	"copilot/postToolUseFailure-iris_probe_probe.json": {kind: KindPostToolFailure, session: copilotB, cwd: proj, tool: "iris-probe-probe", class: ClassOther, unknown: true},
	"copilot/preToolUse-apply_patch.json":              {kind: KindPreTool, session: copilotA, cwd: proj, tool: "apply_patch", class: ClassEdit, paths: []string{"allowed.txt"}},
	"copilot/preToolUse-bash.json":                     {kind: KindPreTool, session: copilotA, cwd: proj, tool: "bash", class: ClassShell, command: "pwd && git status --short && ls -la fixtures"},
	"copilot/preToolUse-iris_probe_probe.json":         {kind: KindPreTool, session: copilotA, cwd: proj, tool: "iris-probe-probe", class: ClassOther, unknown: true},
	"copilot/preToolUse-view.json":                     {kind: KindPreTool, session: copilotB, cwd: proj, tool: "view", class: ClassRead, paths: []string{"/workspace/project/fixtures/denied-target.txt"}},
	"copilot/sessionEnd.json":                          {kind: KindSessionEnd, session: copilotA, cwd: proj},
	"copilot/sessionStart.json":                        {kind: KindSessionStart, session: copilotA, cwd: proj},
	"copilot/userPromptSubmitted.json":                 {kind: KindPrompt, session: copilotA, cwd: proj},

	"cursor/afterFileEdit.json":        {kind: KindPostTool, session: cursorSession, cwd: proj, tool: "edit", class: ClassEdit, paths: []string{"/workspace/project/fixtures/probe.txt"}},
	"cursor/afterMCPExecution.json":    {kind: KindPostTool, session: cursorSession, cwd: proj, tool: "iris_probe", class: ClassMCP, call: cursorSession},
	"cursor/afterShellExecution.json":  {kind: KindPostTool, session: cursorSession, cwd: proj, tool: "Shell", class: ClassShell, command: "echo shell-iris >> fixtures/probe.txt", call: cursorSession},
	"cursor/beforeMCPExecution.json":   {kind: KindPreTool, session: cursorSession, cwd: proj, tool: "iris_probe", class: ClassMCP, call: cursorSession},
	"cursor/beforeReadFile.json":       {kind: KindPreTool, session: cursorSession, cwd: proj, tool: "Read", class: ClassRead, paths: []string{"/workspace/project/fixtures/deny-target.txt"}, call: cursorSession},
	"cursor/beforeShellExecution.json": {kind: KindPreTool, session: cursorSession, cwd: proj, tool: "Shell", class: ClassShell, command: "echo shell-iris >> fixtures/probe.txt", call: cursorSession},
	"cursor/postToolUse.json":          {kind: KindPostTool, session: cursorSession, cwd: proj, tool: "Read", class: ClassRead, paths: []string{"/workspace/project/fixtures/deny-target.txt"}, call: cursorSession},
	"cursor/postToolUseFailure.json":   {kind: KindPostToolFailure, session: cursorSession, cwd: proj, tool: "Write", class: ClassEdit, paths: []string{"/workspace/project/fixtures/deny-target.txt"}, call: cursorSession},
	"cursor/preToolUse.json":           {kind: KindPreTool, session: cursorSession, cwd: proj, tool: "Write", class: ClassEdit, paths: []string{"/workspace/project/fixtures/deny-target.txt"}, call: cursorSession},
	"cursor/sessionEnd.json":           {kind: KindSessionEnd, session: cursorSession, cwd: proj},
	"cursor/sessionStart.json":         {kind: KindSessionStart, session: cursorSession, cwd: proj},

	"grok/PostToolUse-shell.json":         {kind: KindPostTool, session: grokShell, cwd: proj, tool: "run_terminal_command", class: ClassShell, command: "printf 'shell-ok\\n'", call: "call-cb61fada-d57a-4dba-a5b2-ce48672c998c-1"},
	"grok/PostToolUse.json":               {kind: KindPostTool, session: grokMain, cwd: proj, tool: "write", class: ClassEdit, paths: []string{"/workspace/project/note.txt"}, call: "call-1117010f-8145-4391-bd2d-ff25f95b0cc1-0"},
	"grok/PreToolUse-shell.json":          {kind: KindPreTool, session: grokShell, cwd: proj, tool: "run_terminal_command", class: ClassShell, command: "printf 'shell-ok\\n'", call: "call-cb61fada-d57a-4dba-a5b2-ce48672c998c-1"},
	"grok/PreToolUse-spawn_subagent.json": {kind: KindPreTool, session: grokParent, cwd: proj, tool: "spawn_subagent", class: ClassOther, call: "call-6bd1f14a-2df0-49c1-a6e9-8700e29d3b86-0"},
	"grok/PreToolUse.json":                {kind: KindPreTool, session: grokMain, cwd: proj, tool: "write", class: ClassEdit, paths: []string{"/workspace/project/note.txt"}, call: "call-1117010f-8145-4391-bd2d-ff25f95b0cc1-0"},
	"grok/SessionEnd-subagent.json":       {kind: KindSessionEnd, session: grokChild, cwd: proj},
	"grok/SessionEnd.json":                {kind: KindSessionEnd, session: grokMain, cwd: proj},
	"grok/SessionStart.json":              {kind: KindSessionStart, session: grokMain, cwd: proj},
	"grok/Stop-shutdown.json":             {kind: KindStop, session: grokMain, cwd: proj},
	"grok/Stop.json":                      {kind: KindStop, session: grokMain, cwd: proj},
	"grok/SubagentStart.json":             {kind: KindSubagentStart, session: grokChild, parent: grokParent, cwd: proj},
	"grok/SubagentStop.json":              {kind: KindSubagentStop, session: grokChild, cwd: proj},
	"grok/UserPromptSubmit.json":          {kind: KindPrompt, session: grokMain, cwd: proj},

	"kimi/posttooluse.json":               {kind: KindPostTool, session: kimiSession, cwd: "/workspace/project/hooklab", tool: "Bash", class: ClassShell, command: "pwd", call: "Bash_1_8163a792"},
	"kimi/pre_tool_use_bash.json":         {kind: KindPreTool, session: kimiSession, cwd: "/workspace/project/hooklab", tool: "Bash", class: ClassShell, command: "pwd", call: "Bash_1_8163a792"},
	"kimi/pre_tool_use_write_denied.json": {kind: KindPreTool, session: kimiSession, cwd: "/workspace/project/hooklab", tool: "Write", class: ClassEdit, paths: []string{"deny-me.txt"}, call: "Write_0_a89dd82f"},
	"kimi/sessionstart.json":              {kind: KindSessionStart, session: kimiSession, cwd: "/workspace/project/hooklab"},
	"kimi/userpromptsubmit.json":          {kind: KindPrompt, session: kimiSession, cwd: "/workspace/project/hooklab"},
}
