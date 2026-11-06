package integrate

import "strings"

func init() { register(antigravityClient{}) }

type antigravityClient struct{}

func (antigravityClient) Name() string  { return Antigravity }
func (antigravityClient) Human() string { return "" }

// Files uses the documented user-level locations. The measured plugin layout
// is project-scoped, which Iris does not write by default. Global hooks and
// MCP are the user paths in the precedence and config lists.
func (c antigravityClient) Files(l Layout) ([]string, error) {
	hooks, err := l.underHome(".gemini", "config", "hooks.json")
	if err != nil {
		return nil, err
	}
	mcp, err := l.underHome(".gemini", "config", "mcp_config.json")
	if err != nil {
		return nil, err
	}
	return []string{hooks, mcp}, nil
}

func (c antigravityClient) Install(l Layout, _ Plugins) error {
	files, err := c.Files(l)
	if err != nil {
		return err
	}
	iris := l.Iris()
	if err := l.update(Antigravity, files[0], func(existing []byte) ([]byte, error) {
		return writeObject(existing, func(doc map[string]any) error {
			group := map[string]any{}
			group["SessionStart"] = []any{agyCommand(l, iris, "SessionStart", false)}
			group["PreToolUse"] = []any{agyMatched(l, iris, "PreToolUse")}
			group["PostToolUse"] = []any{agyMatched(l, iris, "PostToolUse")}
			group["PreInvocation"] = []any{agyCommand(l, iris, "PreInvocation", false)}
			group["PostInvocation"] = []any{agyCommand(l, iris, "PostInvocation", false)}
			group["Stop"] = []any{agyCommand(l, iris, "Stop", false)}
			doc["iris"] = group
			return nil
		})
	}); err != nil {
		return err
	}
	return l.update(Antigravity, files[1], func(existing []byte) ([]byte, error) {
		return writeObject(existing, func(doc map[string]any) error {
			mcpServers(doc, iris, Antigravity)
			return nil
		})
	})
}

func agyCommand(l Layout, iris, event string, matched bool) map[string]any {
	_ = matched
	return map[string]any{
		"type":    "command",
		"command": Guard(l.goos(), Antigravity, iris, event),
		"timeout": eventTimeout(Antigravity, event),
	}
}

func agyMatched(l Layout, iris, event string) map[string]any {
	return map[string]any{
		"matcher": ".*",
		"hooks":   []any{agyCommand(l, iris, event, true)},
	}
}

func (c antigravityClient) Remove(l Layout, _ Plugins) error { return l.restore(Antigravity) }

func (c antigravityClient) Canary(l Layout) (Invocation, error) {
	files, err := c.Files(l)
	if err != nil {
		return Invocation{}, err
	}
	doc, err := readJSON(files[0])
	if err != nil {
		return Invocation{}, err
	}
	root, _ := doc.(map[string]any)
	group, _ := root["iris"].(map[string]any)
	arr, _ := group["PreToolUse"].([]any)
	if len(arr) == 0 {
		return Invocation{}, ErrNotConfigured
	}
	item, _ := arr[0].(map[string]any)
	hooks, _ := item["hooks"].([]any)
	if len(hooks) == 0 {
		return Invocation{}, ErrNotConfigured
	}
	hook, _ := hooks[0].(map[string]any)
	shell, _ := hook["command"].(string)
	if !strings.Contains(shell, "--event PreToolUse") {
		return Invocation{}, ErrNotConfigured
	}
	return Invocation{Shell: shell, Kind: clientShell(l.goos(), Antigravity)}, nil
}
