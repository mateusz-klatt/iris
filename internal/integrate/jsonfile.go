package integrate

import (
	"encoding/json"
	"strings"
)

func writeObject(existing []byte, edit func(map[string]any) error) ([]byte, error) {
	doc, err := readObject(existing)
	if err != nil {
		return nil, err
	}
	if err := edit(doc); err != nil {
		return nil, err
	}
	return marshalJSON(doc)
}

func mcpServers(doc map[string]any, iris, client string) {
	servers, _ := doc["mcpServers"].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
		doc["mcpServers"] = servers
	}
	servers["iris"] = map[string]any{
		"command": iris,
		"args":    []any{"mcp", "--client", client},
	}
}

// putNestedHook adds a Claude, Codex or Grok command group under hooks[event].
// match reports a group that already runs Iris for that event, so a re-run
// replaces it instead of appending a second copy.
func putNestedHook(doc map[string]any, event string, hook map[string]any, match func(any) bool) error {
	hooks, _ := doc["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
		doc["hooks"] = hooks
	}
	group := map[string]any{"hooks": []any{hook}}
	cur, exists := hooks[event]
	if !exists || cur == nil {
		hooks[event] = []any{group}
		return nil
	}
	arr, ok := cur.([]any)
	if !ok {
		return ErrBroken
	}
	replaced := false
	for i, item := range arr {
		if match(item) {
			arr[i] = group
			replaced = true
		}
	}
	if !replaced {
		arr = append(arr, group)
	}
	hooks[event] = arr
	return nil
}

func matchesExec(v any, client, event string) bool {
	switch t := v.(type) {
	case map[string]any:
		if args, ok := t["args"].([]any); ok && argsMatch(args, client, event) {
			return true
		}
		for _, child := range t {
			if matchesExec(child, client, event) {
				return true
			}
		}
	case []any:
		for _, child := range t {
			if matchesExec(child, client, event) {
				return true
			}
		}
	}
	return false
}

func argsMatch(args []any, client, event string) bool {
	var clientOK, eventOK bool
	for i := 0; i < len(args); i++ {
		s, _ := args[i].(string)
		if s == "--client" && i+1 < len(args) {
			if n, _ := args[i+1].(string); n == client {
				clientOK = true
			}
		}
		if s == "--event" && i+1 < len(args) {
			if n, _ := args[i+1].(string); n == event {
				eventOK = true
			}
		}
	}
	return clientOK && eventOK
}

func putFlatHook(doc map[string]any, event string, hook map[string]any, marker string) error {
	hooks, _ := doc["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
		doc["hooks"] = hooks
	}
	cur, exists := hooks[event]
	if !exists || cur == nil {
		hooks[event] = []any{hook}
		return nil
	}
	arr, ok := cur.([]any)
	if !ok {
		return ErrBroken
	}
	replaced := false
	for i, item := range arr {
		if treeContains(item, marker) {
			arr[i] = hook
			replaced = true
		}
	}
	if !replaced {
		arr = append(arr, hook)
	}
	hooks[event] = arr
	return nil
}

func treeContains(v any, needle string) bool {
	switch t := v.(type) {
	case string:
		return strings.Contains(t, needle)
	case map[string]any:
		for _, child := range t {
			if treeContains(child, needle) {
				return true
			}
		}
	case []any:
		for _, child := range t {
			if treeContains(child, needle) {
				return true
			}
		}
	}
	return false
}

func findArgv(v any, event string) (Invocation, bool) {
	switch t := v.(type) {
	case map[string]any:
		if args, ok := t["args"].([]any); ok {
			cmd, _ := t["command"].(string)
			argv := []string{}
			if cmd != "" {
				argv = append(argv, cmd)
			}
			matched := false
			for _, arg := range args {
				s, _ := arg.(string)
				argv = append(argv, s)
				if s == event {
					matched = true
				}
			}
			if matched && cmd != "" {
				return Invocation{Argv: argv}, true
			}
		}
		for _, child := range t {
			if in, ok := findArgv(child, event); ok {
				return in, true
			}
		}
	case []any:
		for _, child := range t {
			if in, ok := findArgv(child, event); ok {
				return in, true
			}
		}
	}
	return Invocation{}, false
}

func findShell(v any, event string) (string, bool) {
	needle := "--event " + event
	var found string
	var walk func(any)
	walk = func(v any) {
		if found != "" {
			return
		}
		switch t := v.(type) {
		case string:
			if strings.Contains(t, needle) && strings.Contains(t, "hook") {
				found = t
			}
		case map[string]any:
			for _, child := range t {
				walk(child)
			}
		case []any:
			for _, child := range t {
				walk(child)
			}
		}
	}
	walk(v)
	return found, found != ""
}

func readJSON(path string) (any, error) {
	data, err := readFile(path)
	if err != nil {
		return nil, err
	}
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, ErrBroken
	}
	return v, nil
}
