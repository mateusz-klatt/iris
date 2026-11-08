package integrate

import "path/filepath"

func init() { register(cursorClient{}) }

type cursorClient struct{}

func (cursorClient) Name() string  { return Cursor }
func (cursorClient) Human() string { return "" }

func (c cursorClient) Files(l Layout) ([]string, error) {
	dir, err := l.underHome(".cursor")
	if err != nil {
		return nil, err
	}
	return []string{filepath.Join(dir, "hooks.json"), filepath.Join(dir, "mcp.json")}, nil
}

func (c cursorClient) Install(l Layout, _ Plugins) error {
	files, err := c.Files(l)
	if err != nil {
		return err
	}
	iris := l.Iris()
	events := []string{"sessionStart", "sessionEnd", "preToolUse", "postToolUse", "postToolUseFailure", "beforeShellExecution", "afterShellExecution", "afterFileEdit", "beforeMCPExecution"}
	if err := l.update(Cursor, files[0], func(existing []byte) ([]byte, error) {
		return writeObject(existing, func(doc map[string]any) error {
			if err := requireVersion1(doc); err != nil {
				return err
			}
			for _, event := range events {
				hook := map[string]any{
					"command": cursorCommand(l.goos(), iris, event),
					"timeout": eventTimeout(Cursor, event),
				}
				if err := putFlatHook(doc, event, hook, "--client cursor --event "+event); err != nil {
					return err
				}
			}
			return nil
		})
	}); err != nil {
		return err
	}
	return l.update(Cursor, files[1], func(existing []byte) ([]byte, error) {
		return writeObject(existing, func(doc map[string]any) error {
			mcpServers(doc, iris, Cursor)
			return nil
		})
	})
}

func (c cursorClient) Remove(l Layout, _ Plugins) error { return l.unwire(Cursor, stripClient(Cursor)) }

func (c cursorClient) Canary(l Layout) (Invocation, error) {
	files, err := c.Files(l)
	if err != nil {
		return Invocation{}, err
	}
	doc, err := readJSON(files[0])
	if err != nil {
		return Invocation{}, err
	}
	shell, ok := findShell(doc, "sessionStart")
	if !ok {
		return Invocation{}, ErrNotConfigured
	}
	return Invocation{Shell: shell, Kind: clientShell(l.goos(), Cursor)}, nil
}

func cursorCommand(goos, iris, event string) string {
	if goos == "windows" {
		return "& " + psQuote(iris) + " hook --client cursor --event " + event
	}
	return hookShell(iris, Cursor, event)
}
