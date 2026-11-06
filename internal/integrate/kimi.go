package integrate

import (
	"fmt"
	"path/filepath"
	"strings"
)

func init() { register(kimiClient{}) }

type kimiClient struct{}

func (kimiClient) Name() string  { return Kimi }
func (kimiClient) Human() string { return "" }

func (c kimiClient) home(l Layout) (string, error) {
	return l.envDir("KIMI_CODE_HOME", ".kimi-code")
}

func (c kimiClient) Files(l Layout) ([]string, error) {
	dir, err := c.home(l)
	if err != nil {
		return nil, err
	}
	return []string{filepath.Join(dir, "config.toml"), filepath.Join(dir, "mcp.json")}, nil
}

func (c kimiClient) Install(l Layout, _ Plugins) error {
	files, err := c.Files(l)
	if err != nil {
		return err
	}
	iris := l.Iris()
	events := []string{"SessionStart", "UserPromptSubmit", "PreToolUse", "PostToolUse", "Stop", "SessionEnd"}
	var body strings.Builder
	for _, event := range events {
		fmt.Fprintf(&body, "[[hooks]]\nevent = %s\ncommand = %s\ntimeout = %d\n\n",
			tomlString(event), tomlString(hookShell(iris, Kimi, event)), eventTimeout(Kimi, event))
	}
	block := wrapBlock(strings.TrimRight(body.String(), "\n"))
	if err := validateHookKeys(block); err != nil {
		return err
	}
	if err := l.update(Kimi, files[0], func(existing []byte) ([]byte, error) {
		next, err := upsertBlock(string(existing), block)
		if err != nil {
			return nil, err
		}
		return []byte(next), nil
	}); err != nil {
		return err
	}
	return l.update(Kimi, files[1], func(existing []byte) ([]byte, error) {
		return writeObject(existing, func(doc map[string]any) error {
			mcpServers(doc, iris, Kimi)
			return nil
		})
	})
}

func (c kimiClient) Remove(l Layout, _ Plugins) error { return l.restore(Kimi) }

func (c kimiClient) Canary(l Layout) (Invocation, error) {
	files, err := c.Files(l)
	if err != nil {
		return Invocation{}, err
	}
	data, err := readFile(files[0])
	if err != nil {
		return Invocation{}, err
	}
	shell := tomlCommand(string(data), "SessionStart")
	if shell == "" {
		return Invocation{}, ErrNotConfigured
	}
	return Invocation{Shell: shell}, nil
}

func tomlCommand(file, event string) string {
	start := strings.Index(file, markerStart)
	end := strings.Index(file, markerEnd)
	if start < 0 || end < start {
		return ""
	}
	body := file[start:end]
	if !strings.Contains(body, `event = "`+event+`"`) {
		return ""
	}
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "command = ") {
			continue
		}
		if strings.Contains(line, "--event "+event) {
			return unquoteTOML(strings.TrimPrefix(line, "command = "))
		}
	}
	return ""
}

func unquoteTOML(s string) string {
	s = strings.TrimSpace(s)
	if len(s) < 2 || s[0] != '"' || s[len(s)-1] != '"' {
		return ""
	}
	s = s[1 : len(s)-1]
	var b strings.Builder
	esc := false
	for _, r := range s {
		if esc {
			switch r {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			default:
				b.WriteRune(r)
			}
			esc = false
			continue
		}
		if r == '\\' {
			esc = true
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}
