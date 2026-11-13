package integrate

import (
	"path/filepath"
	"strings"
)

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
	events := []string{"sessionStart", "sessionEnd", "stop", "preToolUse", "postToolUse", "postToolUseFailure", "beforeShellExecution", "afterShellExecution", "afterFileEdit", "beforeMCPExecution"}
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

const cursorWakeFile = "iris-wake"

// InstallCursorWake installs Cursor and writes an explicit wake marker. Plain Install does not
// touch the marker, so a missing file stays on and an explicit off survives reintegration.
func InstallCursorWake(l Layout, enabled bool) error {
	path, err := cursorWakePath(l)
	if err != nil {
		return err
	}
	if err := (cursorClient{}).Install(l, nil); err != nil {
		return err
	}
	body := "on\n"
	if !enabled {
		body = "off\n"
	}
	return l.update(Cursor, path, func([]byte) ([]byte, error) {
		return []byte(body), nil
	})
}

// CursorWake reports the marker. A missing file is on. This does not prove a stop hook ran.
func CursorWake(l Layout) (bool, error) {
	path, err := cursorWakePath(l)
	if err != nil {
		return false, err
	}
	raw, absent, err := readExisting(path)
	if err != nil {
		return false, err
	}
	if absent {
		return true, nil
	}
	return strings.TrimSpace(string(raw)) != "off", nil
}

func cursorWakePath(l Layout) (string, error) {
	dir, err := l.underHome(".cursor")
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, cursorWakeFile), nil
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
