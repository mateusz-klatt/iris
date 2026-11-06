package integrate

import (
	"fmt"
	"path/filepath"
)

func init() { register(grokClient{}) }

type grokClient struct{}

func (grokClient) Name() string  { return Grok }
func (grokClient) Human() string { return "" }

func (c grokClient) home(l Layout) (string, error) {
	return l.envDir("GROK_HOME", ".grok")
}

func (c grokClient) Files(l Layout) ([]string, error) {
	dir, err := c.home(l)
	if err != nil {
		return nil, err
	}
	return []string{filepath.Join(dir, "hooks", "iris.json"), filepath.Join(dir, "config.toml")}, nil
}

func (c grokClient) Install(l Layout, _ Plugins) error {
	files, err := c.Files(l)
	if err != nil {
		return err
	}
	iris := l.Iris()
	events := []string{"SessionStart", "PreToolUse", "PostToolUse", "Stop", "SessionEnd"}
	if err := l.update(Grok, files[0], func(existing []byte) ([]byte, error) {
		return writeObject(existing, func(doc map[string]any) error {
			for _, event := range events {
				hook := map[string]any{
					"type":    "command",
					"command": iris,
					"args":    []any{"hook", "--client", Grok, "--event", event},
					"timeout": eventTimeout(Grok, event),
				}
				if err := putNestedHook(doc, event, hook, func(v any) bool {
					return matchesExec(v, Grok, event)
				}); err != nil {
					return err
				}
			}
			return nil
		})
	}); err != nil {
		return err
	}
	block := wrapBlock(fmt.Sprintf("[mcp_servers.iris]\ncommand = %s\nargs = [\"mcp\", \"--client\", \"grok\"]\nenabled = true\n", tomlString(iris)))
	return l.update(Grok, files[1], func(existing []byte) ([]byte, error) {
		next, err := upsertBlock(string(existing), block)
		if err != nil {
			return nil, err
		}
		return []byte(next), nil
	})
}

func (c grokClient) Remove(l Layout, _ Plugins) error { return l.restore(Grok) }

func (c grokClient) Canary(l Layout) (Invocation, error) {
	files, err := c.Files(l)
	if err != nil {
		return Invocation{}, err
	}
	doc, err := readJSON(files[0])
	if err != nil {
		return Invocation{}, err
	}
	if in, ok := findArgv(doc, "SessionStart"); ok {
		return in, nil
	}
	return Invocation{}, ErrNotConfigured
}
