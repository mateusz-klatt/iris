package hook

import "strings"

func kindFrom(argv, payload string) Kind {
	if k, ok := parseKind(argv); ok {
		return k
	}
	if k, ok := parseKind(payload); ok {
		return k
	}
	return KindOther
}

func parseKind(event string) (Kind, bool) {
	switch compact(event) {
	case "sessionstart":
		return KindSessionStart, true
	case "sessionend":
		return KindSessionEnd, true
	case "userpromptsubmit", "userpromptsubmitted":
		return KindPrompt, true
	case "pretooluse", "pretool":
		return KindPreTool, true
	case "posttooluse", "posttool":
		return KindPostTool, true
	case "posttoolusefailure":
		return KindPostToolFailure, true
	case "stop", "agentstop":
		return KindStop, true
	case "subagentstart":
		return KindSubagentStart, true
	case "subagentstop":
		return KindSubagentStop, true
	case "preinvocation":
		return KindPreInvocation, true
	case "postinvocation":
		return KindPostInvocation, true
	case "beforeshellexecution", "beforereadfile", "beforemcpexecution":
		return KindPreTool, true
	case "aftershellexecution", "afterfileedit", "aftermcpexecution":
		return KindPostTool, true
	default:
		return "", false
	}
}

func compact(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if r == '_' || r == '-' || r == ' ' {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}
