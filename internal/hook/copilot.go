package hook

import "encoding/json"

type copilotAdapter struct{}

func (copilotAdapter) Decode(eventName string, payload []byte) (Event, error) {
	var p struct {
		SessionID string          `json:"sessionId"`
		CWD       string          `json:"cwd"`
		ToolName  string          `json:"toolName"`
		ToolArgs  json.RawMessage `json:"toolArgs"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return Event{}, err
	}
	ev := Event{
		Client:    ClientCopilot,
		Kind:      kindFrom(eventName, ""),
		SessionID: p.SessionID,
		CWD:       p.CWD,
	}
	if p.ToolName != "" {
		applyTool(&ev, p.ToolName, p.ToolArgs, false)
	}
	if ev.SessionID == "" {
		return Event{}, errNoSession
	}
	return ev, nil
}

func (copilotAdapter) Encode(ev Event, d Decision) Output {
	return encodeCopilot(ev, d)
}
