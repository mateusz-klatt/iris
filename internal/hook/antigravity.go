package hook

import (
	"encoding/json"
	"strconv"
)

// AgyStop is the part of an Antigravity Stop payload that decides whether to ask for a
// wake helper and whether the conversation is idle (ADR 028).
type AgyStop struct {
	FullyIdle          bool
	TerminationReason  string
	ExecutionNum       int
	ContinuationActive bool
}

type antigravityAdapter struct{}

func (antigravityAdapter) Decode(eventName string, payload []byte) (Event, error) {
	var p struct {
		ConversationID     string   `json:"conversationId"`
		WorkspacePaths     []string `json:"workspacePaths"`
		TranscriptPath     string   `json:"transcriptPath"`
		StepIdx            *int     `json:"stepIdx"`
		FullyIdle          *bool    `json:"fullyIdle"`
		TerminationReason  string   `json:"terminationReason"`
		ExecutionNum       *int     `json:"executionNum"`
		ContinuationActive *bool    `json:"continuationActive"`
		ToolCall           *struct {
			Name string          `json:"name"`
			Args json.RawMessage `json:"args"`
		} `json:"toolCall"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return Event{}, err
	}
	ev := Event{
		Client:         ClientAntigravity,
		Kind:           kindFrom(eventName, ""),
		SessionID:      p.ConversationID,
		Workspaces:     p.WorkspacePaths,
		TranscriptPath: p.TranscriptPath,
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
	if ev.Kind == KindStop {
		execNum := -1
		if p.ExecutionNum != nil {
			execNum = *p.ExecutionNum
		}
		ev.AgyStop = &AgyStop{
			FullyIdle:          p.FullyIdle != nil && *p.FullyIdle,
			TerminationReason:  p.TerminationReason,
			ExecutionNum:       execNum,
			ContinuationActive: p.ContinuationActive != nil && *p.ContinuationActive,
		}
	}
	if ev.SessionID == "" {
		return Event{}, errNoSession
	}
	return ev, nil
}

func (antigravityAdapter) Encode(ev Event, d Decision) Output {
	return encodeAntigravity(ev, d)
}
