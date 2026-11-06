package integrate

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const (
	claudePluginID    = "iris@iris"
	claudeMarketName  = "iris"
	notificationPlain = "An Iris notification means new mail or a claim someone is waiting on."
)

func init() { register(claudeClient{}) }

type claudeClient struct{}

func (claudeClient) Name() string  { return Claude }
func (claudeClient) Human() string { return "" }

func (c claudeClient) root(l Layout) (string, error) {
	dir, err := l.state()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "plugins", "claude-marketplace"), nil
}

func (c claudeClient) Files(l Layout) ([]string, error) {
	root, err := c.root(l)
	if err != nil {
		return nil, err
	}
	plugin := filepath.Join(root, "plugins", "iris")
	return []string{
		filepath.Join(root, ".claude-plugin", "marketplace.json"),
		filepath.Join(plugin, ".claude-plugin", "plugin.json"),
		filepath.Join(plugin, "hooks", "hooks.json"),
		filepath.Join(plugin, ".mcp.json"),
		filepath.Join(plugin, "monitors", "monitors.json"),
		filepath.Join(plugin, "skills", "iris", "SKILL.md"),
	}, nil
}

func (c claudeClient) Install(l Layout, p Plugins) error {
	if p == nil {
		return errors.New("claude plugin installer is not configured")
	}
	root, err := c.root(l)
	if err != nil {
		return err
	}
	iris := l.Iris()
	monitor := shellQuote(iris) + " watch --client claude"
	replacer := strings.NewReplacer("__IRIS__", jsonEscape(iris), "__MONITOR__", jsonEscape(monitor))
	err = fs.WalkDir(claudeFS, "claudeplugin/marketplace", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel := strings.TrimPrefix(path, "claudeplugin/marketplace/")
		rel = filepath.FromSlash(rel)
		data, err := claudeFS.ReadFile(path)
		if err != nil {
			return err
		}
		rendered := []byte(replacer.Replace(string(data)))
		dest := filepath.Join(root, rel)
		return l.update(Claude, dest, func(existing []byte) ([]byte, error) {
			return mergeClaude(rel, existing, rendered)
		})
	})
	if err != nil {
		return err
	}
	if err := p.MarketplaceAdd(root); err != nil {
		return err
	}
	return p.Install(claudePluginID)
}

func (c claudeClient) Remove(l Layout, p Plugins) error {
	if p == nil {
		return errors.New("claude plugin installer is not configured")
	}
	var uninstall error
	if err := p.Uninstall(claudePluginID); err != nil {
		uninstall = err
	}
	if err := p.MarketplaceRemove(claudeMarketName); err != nil && uninstall == nil {
		uninstall = err
	}
	m, err := l.loadManifest()
	if err != nil {
		return err
	}
	createdOnly := true
	for _, rec := range m.Files {
		if rec.Client == Claude && !rec.Created {
			createdOnly = false
		}
	}
	if err := l.restore(Claude); err != nil {
		return err
	}
	if !createdOnly {
		return uninstall
	}
	root, err := c.root(l)
	if err != nil {
		return err
	}
	if err := removeStateTree(l, root); err != nil {
		return err
	}
	return uninstall
}

func (c claudeClient) Canary(l Layout) (Invocation, error) {
	files, err := c.Files(l)
	if err != nil {
		return Invocation{}, err
	}
	var hooks string
	for _, f := range files {
		if strings.HasSuffix(f, filepath.Join("hooks", "hooks.json")) {
			hooks = f
		}
	}
	doc, err := readJSON(hooks)
	if err != nil {
		return Invocation{}, err
	}
	if in, ok := findArgv(doc, "SessionStart"); ok {
		return in, nil
	}
	return Invocation{}, ErrNotConfigured
}

func mergeClaude(rel string, existing, rendered []byte) ([]byte, error) {
	if !json.Valid(rendered) && !strings.HasSuffix(rel, ".md") {
		return nil, fmt.Errorf("rendered %s is not valid JSON", rel)
	}
	switch {
	case strings.HasSuffix(rel, ".md"):
		return mergeText(existing, rendered), nil
	case strings.HasSuffix(rel, "monitors.json"):
		return mergeMonitors(existing, rendered)
	case strings.HasSuffix(rel, "hooks.json"):
		return mergeClaudeHooks(existing, rendered)
	case strings.HasSuffix(rel, ".mcp.json"):
		return mergeMCPFile(existing, rendered)
	default:
		return mergeObjects(existing, rendered)
	}
}

func mergeText(existing, rendered []byte) []byte {
	if len(bytes.TrimSpace(existing)) == 0 {
		if !bytes.HasSuffix(rendered, []byte("\n")) {
			return append(rendered, '\n')
		}
		return rendered
	}
	if bytes.Contains(existing, []byte(notificationPlain)) {
		return existing
	}
	out := existing
	if !bytes.HasSuffix(out, []byte("\n")) {
		out = append(out, '\n')
	}
	return append(out, rendered...)
}

func mergeObjects(existing, rendered []byte) ([]byte, error) {
	base := map[string]any{}
	if len(bytes.TrimSpace(existing)) != 0 {
		var err error
		base, err = readObject(existing)
		if err != nil {
			return nil, err
		}
	}
	var add map[string]any
	if err := json.Unmarshal(rendered, &add); err != nil {
		return nil, err
	}
	for k, v := range add {
		base[k] = v
	}
	return marshalJSON(base)
}

func mergeMCPFile(existing, rendered []byte) ([]byte, error) {
	base := map[string]any{}
	if len(bytes.TrimSpace(existing)) != 0 {
		var err error
		base, err = readObject(existing)
		if err != nil {
			return nil, err
		}
	}
	var add map[string]any
	if err := json.Unmarshal(rendered, &add); err != nil {
		return nil, err
	}
	for k, v := range add {
		if k == "mcpServers" {
			continue
		}
		if _, ok := base[k]; !ok {
			base[k] = v
		}
	}
	servers, _ := add["mcpServers"].(map[string]any)
	if iris, ok := servers["iris"]; ok {
		cur, _ := base["mcpServers"].(map[string]any)
		if cur == nil {
			cur = map[string]any{}
			base["mcpServers"] = cur
		}
		cur["iris"] = iris
	}
	return marshalJSON(base)
}

func mergeClaudeHooks(existing, rendered []byte) ([]byte, error) {
	base := map[string]any{}
	if len(bytes.TrimSpace(existing)) != 0 {
		var err error
		base, err = readObject(existing)
		if err != nil {
			return nil, err
		}
	}
	var add map[string]any
	if err := json.Unmarshal(rendered, &add); err != nil {
		return nil, err
	}
	hooks, _ := add["hooks"].(map[string]any)
	for event, group := range hooks {
		arr, _ := group.([]any)
		if len(arr) == 0 {
			continue
		}
		item, _ := arr[0].(map[string]any)
		inner, _ := item["hooks"].([]any)
		if len(inner) == 0 {
			continue
		}
		hook, _ := inner[0].(map[string]any)
		if err := putNestedHook(base, event, hook, func(v any) bool {
			return matchesExec(v, Claude, event)
		}); err != nil {
			return nil, err
		}
	}
	return marshalJSON(base)
}

func mergeMonitors(existing, rendered []byte) ([]byte, error) {
	var ours []any
	if err := json.Unmarshal(rendered, &ours); err != nil {
		return nil, err
	}
	var arr []any
	if len(bytes.TrimSpace(existing)) != 0 {
		if !json.Valid(existing) {
			return nil, ErrBroken
		}
		if err := json.Unmarshal(existing, &arr); err != nil {
			return nil, ErrBroken
		}
	}
	replaced := false
	for i, item := range arr {
		m, _ := item.(map[string]any)
		if m["name"] == "iris" {
			if len(ours) > 0 {
				arr[i] = ours[0]
			}
			replaced = true
		}
	}
	if !replaced {
		arr = append(arr, ours...)
	}
	return marshalJSON(arr)
}

func jsonEscape(s string) string {
	b, _ := json.Marshal(s)
	if len(b) < 2 {
		return ""
	}
	return string(b[1 : len(b)-1])
}

func removeStateTree(l Layout, dir string) error {
	state, err := l.state()
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(state, dir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return errors.New("refusing to remove a directory outside the state directory")
	}
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return os.RemoveAll(dir)
}

// ExecPlugins runs the Claude Code plugin CLI. It does not edit files itself.
type ExecPlugins struct {
	Run func(name string, args ...string) error
}

func (e ExecPlugins) run(args ...string) error {
	if e.Run != nil {
		return e.Run(args[0], args[1:]...)
	}
	cmd := exec.Command(args[0], args[1:]...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			return err
		}
		return fmt.Errorf("%w: %s", err, msg)
	}
	return nil
}

func (e ExecPlugins) MarketplaceAdd(dir string) error {
	return e.run("claude", "plugin", "marketplace", "add", dir)
}

func (e ExecPlugins) Install(id string) error {
	return e.run("claude", "plugin", "install", id)
}

func (e ExecPlugins) Uninstall(id string) error {
	return e.run("claude", "plugin", "uninstall", id)
}

func (e ExecPlugins) MarketplaceRemove(name string) error {
	return e.run("claude", "plugin", "marketplace", "remove", name)
}
