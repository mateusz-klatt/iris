package integrate

import (
	"fmt"
	"path/filepath"
)

func init() { register(codexClient{}) }

type codexClient struct{}

func (codexClient) Name() string { return Codex }

func (codexClient) Human() string { return "Review hooks once in /hooks" }

func (codexClient) home(l Layout) (string, error) {
	return l.envDir("CODEX_HOME", ".codex")
}

func (c codexClient) Files(l Layout) ([]string, error) {
	dir, err := c.home(l)
	if err != nil {
		return nil, err
	}
	return []string{filepath.Join(dir, "hooks.json"), filepath.Join(dir, "config.toml")}, nil
}

func (c codexClient) Install(l Layout, _ Plugins) error {
	files, err := c.Files(l)
	if err != nil {
		return err
	}
	iris := l.Iris()
	if err := l.update(Codex, files[0], func(existing []byte) ([]byte, error) {
		return writeObject(existing, func(doc map[string]any) error {
			for _, event := range []string{"SessionStart", "UserPromptSubmit", "PreToolUse", "PostToolUse", "Stop", "SessionEnd"} {
				hook := map[string]any{
					"type":    "command",
					"command": hookShell(iris, Codex, event),
					"timeout": eventTimeout(Codex, event),
				}
				if l.goos() == "windows" {
					hook["commandWindows"] = hookCmd(iris, Codex, event)
				}
				if err := putNestedHook(doc, event, hook, func(v any) bool {
					return treeContains(v, "--client codex --event "+event)
				}); err != nil {
					return err
				}
			}
			return nil
		})
	}); err != nil {
		return err
	}
	block := wrapBlock(fmt.Sprintf("[mcp_servers.iris]\ncommand = %s\nargs = [\"mcp\", \"--client\", \"codex\"]\n", tomlString(iris)))
	return l.update(Codex, files[1], func(existing []byte) ([]byte, error) {
		next, err := upsertBlock(string(existing), block)
		if err != nil {
			return nil, err
		}
		return []byte(next), nil
	})
}

func (c codexClient) Remove(l Layout, _ Plugins) error { return l.restore(Codex) }

func (c codexClient) Canary(l Layout) (Invocation, error) {
	files, err := c.Files(l)
	if err != nil {
		return Invocation{}, err
	}
	doc, err := readJSON(files[0])
	if err != nil {
		return Invocation{}, err
	}
	if l.goos() == "windows" {
		if shell, ok := findNamedString(doc, "commandWindows", "--event SessionStart"); ok {
			return Invocation{Shell: shell, Comspec: true}, nil
		}
	}
	shell, ok := findShell(doc, "SessionStart")
	if !ok {
		return Invocation{}, ErrNotConfigured
	}
	return Invocation{Shell: shell}, nil
}
