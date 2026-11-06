package hook

import (
	"bytes"
	"encoding/json"
	"strings"
)

func classifyTool(name string, mcp bool) (ToolClass, bool) {
	low := strings.ToLower(name)
	if mcp || strings.HasPrefix(low, "mcp__") || strings.HasPrefix(low, "mcp:") {
		return ClassMCP, false
	}
	switch low {
	case "write", "edit", "multiedit", "notebookedit", "apply_patch", "write_file", "edit_file",
		"search_replace", "hashline_edit", "delete", "delete_file", "move_file", "strreplace", "str_replace":
		return ClassEdit, false
	case "bash", "shell", "run_terminal_command", "run_command":
		return ClassShell, false
	case "read", "view", "grep", "glob", "ls":
		return ClassRead, false
	case "toolsearch", "spawn_subagent", "task", "todowrite", "webfetch", "websearch", "skill":
		return ClassOther, false
	case "":
		return "", false
	default:
		return ClassOther, true
	}
}

func applyTool(ev *Event, name string, raw json.RawMessage, mcp bool) {
	ev.ToolName = name
	ev.ToolClass, ev.UnknownTool = classifyTool(name, mcp)
	m, str, isStr := parseRaw(raw)
	if isStr {
		if looksLikePatch(str) {
			ev.Paths, ev.RenameFrom, ev.RenameTo = parsePatch(str)
			return
		}
		if ev.ToolClass == ClassShell || ev.ToolClass == ClassOther {
			ev.Command = str
		}
		return
	}
	cmd := mapString(m, "command", "commandline", "cmd")
	if looksLikePatch(cmd) && (ev.ToolClass == ClassEdit || strings.EqualFold(name, "apply_patch")) {
		ev.Paths, ev.RenameFrom, ev.RenameTo = parsePatch(cmd)
		return
	}
	if ev.ToolClass == ClassShell {
		ev.Command = cmd
		return
	}
	ev.Paths, ev.RenameFrom, ev.RenameTo = pathsFromMap(name, m)
}

func parseRaw(raw json.RawMessage) (map[string]any, string, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil, "", false
	}
	if raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return nil, s, true
		}
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) == nil {
		return m, "", false
	}
	return nil, "", false
}

func mapString(m map[string]any, keys ...string) string {
	if len(m) == 0 {
		return ""
	}
	lower := make(map[string]string, len(m))
	for k, v := range m {
		s, ok := v.(string)
		if !ok || s == "" {
			continue
		}
		lower[strings.ToLower(k)] = s
	}
	for _, k := range keys {
		if s := lower[strings.ToLower(k)]; s != "" {
			return s
		}
	}
	return ""
}

func pathsFromMap(tool string, m map[string]any) (paths []string, from, to string) {
	if len(m) == 0 {
		return nil, "", ""
	}
	lower := make(map[string]string, len(m))
	for k, v := range m {
		s, ok := v.(string)
		if !ok || s == "" {
			continue
		}
		lower[strings.ToLower(k)] = s
	}
	for _, k := range []string{"file_path", "filepath", "path", "target_file", "notebook_path"} {
		if s := lower[k]; s != "" {
			paths = append(paths, s)
		}
	}
	move := isMoveTool(tool) || lower["rename_from"] != "" || lower["rename_to"] != "" ||
		lower["source_path"] != "" || lower["destination_path"] != ""
	if move {
		for _, k := range []string{"rename_from", "source_path", "old_path", "source"} {
			if s := lower[k]; s != "" {
				from = s
				break
			}
		}
		for _, k := range []string{"rename_to", "destination_path", "new_path", "destination"} {
			if s := lower[k]; s != "" {
				to = s
				break
			}
		}
		if from != "" {
			paths = append(paths, from)
		}
		if to != "" {
			paths = append(paths, to)
		}
	}
	return dedupe(paths), from, to
}

func isMoveTool(tool string) bool {
	low := strings.ToLower(tool)
	return strings.Contains(low, "move") || strings.Contains(low, "rename")
}

func looksLikePatch(s string) bool {
	return strings.Contains(s, "*** Add File") || strings.Contains(s, "*** Update File") ||
		strings.Contains(s, "*** Delete File") || strings.Contains(s, "*** Move to") ||
		strings.Contains(s, "*** Begin Patch")
}

// parsePatch reads Codex and Copilot apply_patch text.
// Both ends of a move are returned. RenameFrom and RenameTo are set for one move.
func parsePatch(s string) (paths []string, from, to string) {
	var last string
	moves := 0
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimRight(line, "\r")
		op, rest, ok := cutPatchOp(line)
		if !ok {
			continue
		}
		switch op {
		case "move":
			if last != "" {
				moves++
				if moves == 1 {
					from, to = last, rest
				}
			}
			paths = append(paths, rest)
		default:
			paths = append(paths, rest)
			last = rest
		}
	}
	return dedupe(paths), from, to
}

func cutPatchOp(line string) (op, rest string, ok bool) {
	const prefix = "*** "
	if !strings.HasPrefix(line, prefix) {
		return "", "", false
	}
	body := strings.TrimSpace(line[len(prefix):])
	specs := []struct{ prefix, op string }{
		{"Add File:", "add"},
		{"Update File:", "update"},
		{"Delete File:", "delete"},
		{"Move to:", "move"},
	}
	for _, spec := range specs {
		if strings.HasPrefix(body, spec.prefix) {
			return spec.op, strings.TrimSpace(body[len(spec.prefix):]), true
		}
	}
	return "", "", false
}

func dedupe(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	var out []string
	seen := make(map[string]bool, len(in))
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}
