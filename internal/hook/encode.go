package hook

import (
	"bytes"
	"encoding/json"
	"strings"
)

func jsonLine(v any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return []byte("{}\n")
	}
	return buf.Bytes()
}

func withNL(s string) []byte {
	if s == "" {
		return nil
	}
	if strings.HasSuffix(s, "\n") {
		return []byte(s)
	}
	return []byte(s + "\n")
}

func message(d Decision) string {
	if d.Reason != "" {
		return d.Reason
	}
	return d.Context
}

// supportsAsk reports clients whose pre-tool hook can ask the human.
// Codex has no ask on this hook, and Kimi can only block.
func supportsAsk(c Client) bool {
	switch c {
	case ClientClaude, ClientCopilot, ClientCursor, ClientAntigravity, ClientGrok:
		return true
	default:
		return false
	}
}

const askHuman = "Ask the human how to proceed."

// normalize turns ask into a deny on clients that cannot ask, so the policy
// is never silently weakened to an allow.
func normalize(c Client, d Decision) Decision {
	if d.Action != ActionAsk || supportsAsk(c) {
		return d
	}
	d.Action = ActionDeny
	switch {
	case d.Reason == "":
		d.Reason = askHuman
	case !strings.Contains(d.Reason, askHuman):
		d.Reason = strings.TrimRight(d.Reason, " ") + " " + askHuman
	}
	return d
}

// FallbackAllow is a valid allow for client when the hook must fail open
// and does not know a more specific event.
func FallbackAllow(c Client) Output {
	switch c {
	case ClientKimi:
		return Output{}
	case ClientCursor:
		return Output{Stdout: []byte("{\"permission\":\"allow\"}\n")}
	case ClientAntigravity:
		return Output{Stdout: []byte("{\"decision\":\"allow\"}\n")}
	default:
		return Output{Stdout: []byte("{}\n")}
	}
}

type hookOut struct {
	HookSpecificOutput hookBody `json:"hookSpecificOutput"`
}

type hookBody struct {
	HookEventName            string `json:"hookEventName"`
	PermissionDecision       string `json:"permissionDecision,omitempty"`
	PermissionDecisionReason string `json:"permissionDecisionReason,omitempty"`
	AdditionalContext        string `json:"additionalContext,omitempty"`
}

func claudeEventName(k Kind) string {
	switch k {
	case KindSessionStart:
		return "SessionStart"
	case KindSessionEnd:
		return "SessionEnd"
	case KindPrompt:
		return "UserPromptSubmit"
	case KindPostTool:
		return "PostToolUse"
	case KindPostToolFailure:
		return "PostToolUseFailure"
	case KindStop:
		return "Stop"
	case KindSubagentStart:
		return "SubagentStart"
	case KindSubagentStop:
		return "SubagentStop"
	default:
		return "PreToolUse"
	}
}

func encodeHookSpecific(ev Event, d Decision) Output {
	d = normalize(ev.Client, d)
	name := claudeEventName(ev.Kind)
	text := message(d)
	switch d.Action {
	case ActionDeny:
		return Output{Stdout: jsonLine(hookOut{HookSpecificOutput: hookBody{
			HookEventName: name, PermissionDecision: "deny", PermissionDecisionReason: text,
		}})}
	case ActionAsk:
		return Output{Stdout: jsonLine(hookOut{HookSpecificOutput: hookBody{
			HookEventName: name, PermissionDecision: "ask", PermissionDecisionReason: text,
		}})}
	}
	if text != "" {
		return Output{Stdout: jsonLine(hookOut{HookSpecificOutput: hookBody{
			HookEventName: name, AdditionalContext: text,
		}})}
	}
	return Output{Stdout: []byte("{}\n")}
}

type copilotOut struct {
	PermissionDecision       string `json:"permissionDecision,omitempty"`
	PermissionDecisionReason string `json:"permissionDecisionReason,omitempty"`
	AdditionalContext        string `json:"additionalContext,omitempty"`
}

func encodeCopilot(ev Event, d Decision) Output {
	d = normalize(ClientCopilot, d)
	text := message(d)
	switch d.Action {
	case ActionDeny:
		return Output{Stdout: jsonLine(copilotOut{PermissionDecision: "deny", PermissionDecisionReason: text})}
	case ActionAsk:
		return Output{Stdout: jsonLine(copilotOut{PermissionDecision: "ask", PermissionDecisionReason: text})}
	}
	if text != "" {
		return Output{Stdout: jsonLine(copilotOut{AdditionalContext: text})}
	}
	return Output{Stdout: []byte("{}\n")}
}

type cursorPerm struct {
	Permission   string `json:"permission"`
	UserMessage  string `json:"user_message,omitempty"`
	AgentMessage string `json:"agent_message,omitempty"`
}

type cursorCtx struct {
	AdditionalContext string `json:"additional_context"`
}

type cursorFollow struct {
	Followup string `json:"followup_message"`
}

func encodeCursor(ev Event, d Decision) Output {
	d = normalize(ClientCursor, d)
	text := message(d)
	if ev.Kind == KindStop {
		if d.Action == ActionAllow && text != "" {
			return Output{Stdout: jsonLine(cursorFollow{Followup: text})}
		}
		return Output{Stdout: []byte("{}\n")}
	}
	if ev.Kind == KindPreTool || d.Action == ActionDeny || d.Action == ActionAsk {
		perm := "allow"
		switch d.Action {
		case ActionDeny:
			perm = "deny"
		case ActionAsk:
			perm = "ask"
		}
		body := cursorPerm{Permission: perm}
		if d.Action == ActionWarn || (text != "" && perm != "allow") {
			body.UserMessage = text
			body.AgentMessage = text
		} else if text != "" {
			body.AgentMessage = text
		}
		return Output{Stdout: jsonLine(body)}
	}
	if text != "" {
		return Output{Stdout: jsonLine(cursorCtx{AdditionalContext: text})}
	}
	return Output{Stdout: []byte("{}\n")}
}

type agyDec struct {
	Decision string `json:"decision"`
	Reason   string `json:"reason,omitempty"`
}

type agyCtx struct {
	InjectSteps []agyStep `json:"injectSteps"`
}

type agyStep struct {
	SystemMessage agyMsg `json:"systemMessage"`
}

type agyMsg struct {
	SystemMessage string `json:"systemMessage"`
}

func encodeAntigravity(ev Event, d Decision) Output {
	d = normalize(ClientAntigravity, d)
	text := message(d)
	if ev.Kind == KindPreTool {
		dec := "allow"
		switch d.Action {
		case ActionDeny:
			dec = "deny"
		case ActionAsk:
			dec = "ask"
		}
		return Output{Stdout: jsonLine(agyDec{Decision: dec, Reason: text})}
	}
	if text != "" && (ev.Kind == KindSessionStart || ev.Kind == KindPreInvocation || d.Action == ActionWarn) {
		return Output{Stdout: jsonLine(agyCtx{InjectSteps: []agyStep{{SystemMessage: agyMsg{SystemMessage: text}}}})}
	}
	if d.Action == ActionDeny {
		return Output{Stdout: jsonLine(agyDec{Decision: "deny", Reason: text})}
	}
	return Output{Stdout: []byte("{}\n")}
}

type grokDec struct {
	Decision          string `json:"decision,omitempty"`
	Reason            string `json:"reason,omitempty"`
	SystemMessage     string `json:"systemMessage,omitempty"`
	AdditionalContext string `json:"additionalContext,omitempty"`
}

func encodeGrok(ev Event, d Decision) Output {
	d = normalize(ClientGrok, d)
	text := message(d)
	switch d.Action {
	case ActionDeny:
		return Output{Stdout: jsonLine(grokDec{Decision: "deny", Reason: text}), Exit: 2}
	case ActionAsk:
		return Output{Stdout: jsonLine(grokDec{Decision: "ask", Reason: text})}
	}
	if text != "" && ev.Kind == KindSessionStart {
		return Output{Stdout: jsonLine(grokDec{SystemMessage: text})}
	}
	if text != "" {
		return Output{Stdout: jsonLine(grokDec{AdditionalContext: text})}
	}
	return Output{Stdout: []byte("{}\n")}
}

func encodeKimi(ev Event, d Decision) Output {
	d = normalize(ClientKimi, d)
	text := message(d)
	if d.Action == ActionDeny {
		return Output{Stderr: withNL(text), Exit: 2}
	}
	if text != "" {
		return Output{Stdout: withNL(text)}
	}
	return Output{}
}
