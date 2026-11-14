// Package hook is the iris hook path: one canonical event, one adapter per client.
package hook

import "strings"

// Client is the agent that owns a hook integration.
type Client string

const (
	ClientClaude      Client = "claude"
	ClientCodex       Client = "codex"
	ClientCopilot     Client = "copilot"
	ClientCursor      Client = "cursor"
	ClientAntigravity Client = "antigravity"
	ClientGrok        Client = "grok"
	ClientKimi        Client = "kimi"
)

// Clients is the stable order used by tests.
var Clients = []Client{
	ClientClaude, ClientCodex, ClientCopilot, ClientCursor, ClientAntigravity, ClientGrok, ClientKimi,
}

// Kind is the client-independent hook event.
type Kind string

const (
	KindSessionStart    Kind = "session_start"
	KindSessionEnd      Kind = "session_end"
	KindPrompt          Kind = "prompt"
	KindPreTool         Kind = "pre_tool"
	KindPostTool        Kind = "post_tool"
	KindPostToolFailure Kind = "post_tool_failure"
	KindStop            Kind = "stop"
	KindSubagentStart   Kind = "subagent_start"
	KindSubagentStop    Kind = "subagent_stop"
	KindPreInvocation   Kind = "pre_invocation"
	KindPostInvocation  Kind = "post_invocation"
	KindOther           Kind = "other"
)

// ToolClass is the coarse tool taxonomy the hook acts on.
type ToolClass string

const (
	ClassEdit  ToolClass = "edit"
	ClassShell ToolClass = "shell"
	ClassRead  ToolClass = "read"
	ClassMCP   ToolClass = "mcp"
	ClassOther ToolClass = "other"
)

// Event is the canonical hook input. Paths are as the client wrote them.
type Event struct {
	Client          Client
	Kind            Kind
	SessionID       string
	ParentSessionID string
	CWD             string
	ToolName        string
	ToolClass       ToolClass
	Paths           []string
	Command         string
	ToolCallID      string
	RenameFrom      string
	RenameTo        string
	UnknownTool     bool
	// Workspaces are extra roots. Antigravity's process cwd is not the repository.
	Workspaces []string
	// Prompt is a prompt event's whole prompt, for one comparison only: whether it is the message
	// of the session's own wake (ADR 017, decision 3). It is never logged and decides nothing else.
	Prompt string
	// Steer is set for a Kimi prompt typed into a turn that is still running (is_steer). Such a
	// prompt starts no turn, so it never asks for or judges Kimi's one-shot watcher (ADR 030).
	Steer bool
	// GrokStop is set only for a Grok Stop event. It decides whether to ask for a monitor (ADR 025).
	GrokStop *GrokStop
	// TranscriptPath is an Antigravity conversation's JSONL transcript path, used only for
	// local wake-guard consumption and idle offset recording (ADR 028).
	TranscriptPath string
	// AgyStop is set only for an Antigravity Stop event (ADR 028).
	AgyStop *AgyStop
	// LoopCount is a Cursor stop's follow-up generation. Zero is the turn that
	// just ended. Above zero, this stop is already a follow-up (ADR 029).
	LoopCount int
}

// Action is the canonical decision.
type Action string

const (
	ActionAllow Action = "allow"
	ActionDeny  Action = "deny"
	ActionWarn  Action = "warn"
	ActionAsk   Action = "ask"
)

// Decision is what the hub (or a failure) asks the adapter to tell the client.
type Decision struct {
	Action  Action
	Reason  string
	Context string
}

// Output is the bytes and status a client hook must produce.
type Output struct {
	Stdout []byte
	Stderr []byte
	Exit   int
}

// Canonical accepts the names used on the command line, including aliases.
func Canonical(name string) Client {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "claude", "claude-code":
		return ClientClaude
	case "codex":
		return ClientCodex
	case "copilot":
		return ClientCopilot
	case "cursor", "cursor-agent":
		return ClientCursor
	case "antigravity", "agy":
		return ClientAntigravity
	case "grok":
		return ClientGrok
	case "kimi", "kimi-code":
		return ClientKimi
	default:
		return ""
	}
}
