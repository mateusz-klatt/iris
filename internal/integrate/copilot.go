package integrate

import (
	"encoding/json"
	"path/filepath"
)

func init() { register(copilotClient{}) }

type copilotClient struct{}

func (copilotClient) Name() string  { return Copilot }
func (copilotClient) Human() string { return "" }

func (copilotClient) home(l Layout) (string, error) {
	return l.envDir("COPILOT_HOME", ".copilot")
}

func (c copilotClient) Files(l Layout) ([]string, error) {
	dir, err := c.home(l)
	if err != nil {
		return nil, err
	}
	return []string{filepath.Join(dir, "hooks", "iris.json"), filepath.Join(dir, "mcp-config.json")}, nil
}

func (c copilotClient) Install(l Layout, _ Plugins) error {
	files, err := c.Files(l)
	if err != nil {
		return err
	}
	iris := l.Iris()
	events := []string{"sessionStart", "userPromptSubmitted", "preToolUse", "postToolUse", "postToolUseFailure", "agentStop", "sessionEnd"}
	if err := l.update(Copilot, files[0], func(existing []byte) ([]byte, error) {
		return writeObject(existing, func(doc map[string]any) error {
			if err := requireVersion1(doc); err != nil {
				return err
			}
			for _, event := range events {
				hook := map[string]any{
					"type":       "command",
					"bash":       Guard("linux", Copilot, iris, event),
					"powershell": Guard("windows", Copilot, iris, event),
					"timeoutSec": eventTimeout(Copilot, event),
				}
				if err := putFlatHook(doc, event, hook, "--client copilot --event "+event); err != nil {
					return err
				}
			}
			return nil
		})
	}); err != nil {
		return err
	}
	return l.update(Copilot, files[1], func(existing []byte) ([]byte, error) {
		return writeObject(existing, func(doc map[string]any) error {
			mcpServers(doc, iris, Copilot)
			return nil
		})
	})
}

func (c copilotClient) Remove(l Layout, _ Plugins) error {
	return l.unwire(Copilot, stripClient(Copilot))
}

func (c copilotClient) Canary(l Layout) (Invocation, error) {
	files, err := c.Files(l)
	if err != nil {
		return Invocation{}, err
	}
	doc, err := readJSON(files[0])
	if err != nil {
		return Invocation{}, err
	}
	root, _ := doc.(map[string]any)
	hooks, _ := root["hooks"].(map[string]any)
	arr, _ := hooks["preToolUse"].([]any)
	if len(arr) == 0 {
		return Invocation{}, ErrNotConfigured
	}
	item, _ := arr[0].(map[string]any)
	field := "bash"
	if l.goos() == "windows" {
		field = "powershell"
	}
	shell, _ := item[field].(string)
	if shell == "" {
		return Invocation{}, ErrNotConfigured
	}
	return Invocation{Shell: shell, Kind: clientShell(l.goos(), Copilot)}, nil
}

func requireVersion1(doc map[string]any) error {
	v, ok := doc["version"]
	if !ok {
		doc["version"] = 1
		return nil
	}
	if n, ok := v.(json.Number); !ok || n.String() != "1" {
		return ErrBroken
	}
	return nil
}
