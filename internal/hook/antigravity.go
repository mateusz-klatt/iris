package hook

import (
	"encoding/json"
	"strconv"
)

type antigravityAdapter struct{}

func (antigravityAdapter) Decode(eventName string, payload []byte) (Event, error) {
	var p struct {
		ConversationID string   `json:"conversationId"`
		WorkspacePaths []string `json:"workspacePaths"`
		StepIdx        *int     `json:"stepIdx"`
		ToolCall       *struct {
			Name string          `json:"name"`
			Args json.RawMessage `json:"args"`
		} `json:"toolCall"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return Event{}, err
	}
	ev := Event{
		Client:     ClientAntigravity,
		Kind:       kindFrom(eventName, ""),
		SessionID:  p.ConversationID,
		Workspaces: p.WorkspacePaths,
	}
	// The hook process runs in .agents/. The repository is workspacePaths.
	if len(p.WorkspacePaths) > 0 {
		ev.CWD = p.WorkspacePaths[0]
	}
	if p.StepIdx != nil {
		ev.ToolCallID = strconv.Itoa(*p.StepIdx)
	}
	if p.ToolCall != nil && p.ToolCall.Name != "" {
		applyTool(&ev, p.ToolCall.Name, p.ToolCall.Args, false)
	}
	if ev.SessionID == "" {
		return Event{}, errNoSession
	}
	return ev, nil
}

func (antigravityAdapter) Encode(ev Event, d Decision) Output {
	return encodeAntigravity(ev, d)
}
