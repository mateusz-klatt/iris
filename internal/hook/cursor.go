package hook

import "encoding/json"

type cursorAdapter struct{}

func (cursorAdapter) Decode(eventName string, payload []byte) (Event, error) {
	var p struct {
		ConversationID string          `json:"conversation_id"`
		SessionID      string          `json:"session_id"`
		HookEventName  string          `json:"hook_event_name"`
		CWD            string          `json:"cwd"`
		WorkspaceRoots []string        `json:"workspace_roots"`
		ToolName       string          `json:"tool_name"`
		ToolInput      json.RawMessage `json:"tool_input"`
		ToolUseID      string          `json:"tool_use_id"`
		Command        string          `json:"command"`
		FilePath       string          `json:"file_path"`
		MCPServer      string          `json:"mcp_server_name"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return Event{}, err
	}
	spec := compact(eventName)
	if spec == "" {
		spec = compact(p.HookEventName)
	}
	ev := Event{
		Client:     ClientCursor,
		Kind:       kindFrom(spec, ""),
		SessionID:  first(p.SessionID, p.ConversationID),
		CWD:        p.CWD,
		Workspaces: p.WorkspaceRoots,
		ToolCallID: p.ToolUseID,
	}
	if ev.CWD == "" && len(p.WorkspaceRoots) > 0 {
		ev.CWD = p.WorkspaceRoots[0]
	}
	switch spec {
	case "beforeshellexecution", "aftershellexecution":
		ev.ToolName = "Shell"
		ev.ToolClass = ClassShell
		ev.Command = p.Command
	case "beforereadfile":
		ev.ToolName = "Read"
		ev.ToolClass = ClassRead
		if p.FilePath != "" {
			ev.Paths = []string{p.FilePath}
		}
	case "afterfileedit":
		ev.ToolName = "edit"
		ev.ToolClass = ClassEdit
		if p.FilePath != "" {
			ev.Paths = []string{p.FilePath}
		}
	case "beforemcpexecution", "aftermcpexecution":
		ev.ToolName = p.ToolName
		ev.ToolClass = ClassMCP
	default:
		if p.ToolName != "" {
			applyTool(&ev, p.ToolName, p.ToolInput, p.MCPServer != "")
		}
	}
	if ev.SessionID == "" {
		return Event{}, errNoSession
	}
	return ev, nil
}

func (cursorAdapter) Encode(ev Event, d Decision) Output {
	return encodeCursor(ev, d)
}
