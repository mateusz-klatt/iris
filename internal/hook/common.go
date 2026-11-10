package hook

import (
	"bytes"
	"encoding/json"
	"errors"
)

var errNoSession = errors.New("hook payload has no session id")

// commonPayload is the Claude-shaped stdin shared by Claude, Codex, Grok and Kimi.
// Grok sends each field twice, in camelCase and snake_case.
type commonPayload struct {
	SessionID      string          `json:"session_id"`
	SessionIDCamel string          `json:"sessionId"`
	CWD            string          `json:"cwd"`
	HookEvent      string          `json:"hook_event_name"`
	HookEventCamel string          `json:"hookEventName"`
	ToolName       string          `json:"tool_name"`
	ToolNameCamel  string          `json:"toolName"`
	ToolInput      json.RawMessage `json:"tool_input"`
	ToolInputCamel json.RawMessage `json:"toolInput"`
	ToolUseID      string          `json:"tool_use_id"`
	ToolUseCamel   string          `json:"toolUseId"`
	ToolCallID     string          `json:"tool_call_id"`
	MCP            json.RawMessage `json:"mcp_server"`
	SubagentID     string          `json:"subagentId"`
	SubagentSnake  string          `json:"subagent_id"`
	// Prompt is not always text: Kimi sends an array. Only Codex's is read (ADR 017).
	Prompt json.RawMessage `json:"prompt"`
}

func decodeCommon(client Client, eventName string, payload []byte) (Event, error) {
	var p commonPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return Event{}, err
	}
	ev := Event{
		Client:     client,
		Kind:       kindFrom(eventName, first(p.HookEvent, p.HookEventCamel)),
		SessionID:  first(p.SessionID, p.SessionIDCamel),
		CWD:        p.CWD,
		ToolCallID: first(p.ToolUseID, p.ToolUseCamel, p.ToolCallID),
	}
	if name := first(p.ToolName, p.ToolNameCamel); name != "" {
		raw := p.ToolInput
		if len(bytes.TrimSpace(raw)) == 0 {
			raw = p.ToolInputCamel
		}
		applyTool(&ev, name, raw, mcpPresent(p.MCP))
	}
	if client == ClientCodex && ev.Kind == KindPrompt {
		_ = json.Unmarshal(p.Prompt, &ev.Prompt) // a prompt that is not text matches no wake
	}
	if sub := first(p.SubagentID, p.SubagentSnake); ev.Kind == KindSubagentStart && sub != "" && sub != ev.SessionID {
		ev.ParentSessionID = ev.SessionID
		ev.SessionID = sub
	}
	if ev.SessionID == "" {
		return Event{}, errNoSession
	}
	return ev, nil
}

func mcpPresent(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	return len(raw) > 0 && !bytes.Equal(raw, []byte("null"))
}

func first(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func peekSessionID(payload []byte) string {
	var m map[string]any
	if json.Unmarshal(payload, &m) != nil {
		return ""
	}
	for _, key := range []string{"session_id", "sessionId", "conversationId", "conversation_id"} {
		if s, ok := m[key].(string); ok && s != "" {
			return s
		}
	}
	return ""
}
