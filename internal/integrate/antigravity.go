package integrate

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
)

func init() { register(antigravityClient{}) }

type antigravityClient struct{}

func (antigravityClient) Name() string  { return Antigravity }
func (antigravityClient) Human() string { return "" }

// Files uses the documented user-level locations. The measured plugin layout
// is project-scoped, which Iris does not write by default. Global hooks and
// MCP are the user paths in the precedence and config lists.
func (c antigravityClient) Files(l Layout) ([]string, error) {
	gemini, err := l.underHome(".gemini")
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(gemini, "config")
	settings := filepath.Join(gemini, "antigravity-cli", "settings.json")
	return []string{filepath.Join(dir, "hooks.json"), filepath.Join(dir, "mcp_config.json"), settings}, nil
}

// AgyAllowRule lets a headless agy call the Iris tools. Without it agy 1.5.3 denies every Iris
// call in print mode ("a tool required the "mcp" permission that headless mode cannot prompt for,
// so it was auto-denied") and stops. agy reads the rule only from the user's
// antigravity-cli/settings.json, not from a workspace file (measured). The rule covers the Iris
// server alone, and Iris tools act only on the hub, never on local files or a shell.
const AgyAllowRule = "mcp(iris/*)"

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
	if err := l.update(Antigravity, files[1], func(existing []byte) ([]byte, error) {
		return writeObject(existing, func(doc map[string]any) error {
			mcpServers(doc, iris, Antigravity)
			return nil
		})
	}); err != nil {
		return err
	}
	return l.update(Antigravity, files[2], func(existing []byte) ([]byte, error) {
		// A rule the user already has stays theirs: the file is returned untouched, whatever its
		// formatting, so it never enters the manifest and remove never strips it.
		if doc, err := readObject(existing); err == nil && hasAllowRule(doc, AgyAllowRule) {
			return existing, nil
		}
		return writeObject(existing, func(doc map[string]any) error {
			return AddAllowRule(doc, AgyAllowRule)
		})
	})
}

// AddAllowRule puts rule into agy's permissions.allow once, keeping every other setting.
func AddAllowRule(doc map[string]any, rule string) error {
	perms, ok := doc["permissions"].(map[string]any)
	if !ok {
		if doc["permissions"] != nil {
			return fmt.Errorf("%w: permissions is not an object", ErrBroken)
		}
		perms = map[string]any{}
		doc["permissions"] = perms
	}
	allow, ok := perms["allow"].([]any)
	if !ok && perms["allow"] != nil {
		return fmt.Errorf("%w: permissions.allow is not a list", ErrBroken)
	}
	for _, r := range allow {
		if r == rule {
			return nil
		}
	}
	perms["allow"] = append(allow, rule)
	return nil
}

func hasAllowRule(doc map[string]any, rule string) bool {
	perms, _ := doc["permissions"].(map[string]any)
	allow, _ := perms["allow"].([]any)
	return slices.Contains(allow, any(rule))
}

// RemoveAllowRule takes rule out of agy's permissions.allow, and the containers it leaves empty.
func RemoveAllowRule(doc map[string]any, rule string) bool {
	perms, _ := doc["permissions"].(map[string]any)
	allow, _ := perms["allow"].([]any)
	kept := make([]any, 0, len(allow))
	for _, r := range allow {
		if r != rule {
			kept = append(kept, r)
		}
	}
	if len(kept) == len(allow) {
		return false
	}
	if len(kept) > 0 {
		perms["allow"] = kept
	} else {
		delete(perms, "allow")
	}
	if len(perms) == 0 {
		delete(doc, "permissions")
	}
	return true
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

func (c antigravityClient) Remove(l Layout, _ Plugins) error {
	return l.unwire(Antigravity, stripClient(Antigravity))
}

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
