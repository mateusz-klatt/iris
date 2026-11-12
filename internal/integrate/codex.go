package integrate

import (
	"fmt"
	"path/filepath"
)

func init() { register(codexClient{}) }

type codexClient struct{}

func (codexClient) Name() string { return Codex }

func (codexClient) Human() string { return "Review hooks once in /hooks" }

// codexMCPPolicy is the rest of the iris server table.
//
// Codex starts MCP servers with a filtered environment, and its hooks with the full one. Without
// env_vars an IRIS_HUB_URL override reached the hooks but not `iris mcp`, so claims went to one hub
// and messages to another (measured on 0.167.0).
// CODEX_HOME also keeps the wake command on the session's configured Codex home (ADR 017).
//
// default_tools_approval_mode applies to this server only. Without it every Iris tool call asks,
// and with approval_policy "never" each one is refused. Iris tools act only on the hub, never on
// files or a shell. "writes" was measured to refuse even whoami, because the tools carry no
// read-only annotation.
const codexMCPPolicy = "env_vars = [\"IRIS_HUB_URL\", \"IRIS_TOKEN_FILE\", \"IRIS_EXECUTION\", \"CODEX_HOME\"]\n" +
	"default_tools_approval_mode = \"approve\"\n"

func (codexClient) home(l Layout) (string, error) {
	return l.envDir("CODEX_HOME", ".codex")
}

// CodexConfig is Codex's config.toml, where Codex records trusted directories.
func CodexConfig(l Layout) (string, error) {
	files, err := codexClient{}.Files(l)
	if err != nil {
		return "", err
	}
	return files[1], nil
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
	block := wrapBlock(fmt.Sprintf("[mcp_servers.iris]\ncommand = %s\nargs = [\"mcp\", \"--client\", \"codex\"]\n%s", tomlString(iris), codexMCPPolicy))
	return l.update(Codex, files[1], func(existing []byte) ([]byte, error) {
		next, err := upsertBlock(string(existing), block)
		if err != nil {
			return nil, err
		}
		return []byte(next), nil
	})
}

func (c codexClient) Remove(l Layout, _ Plugins) error { return l.unwire(Codex, stripClient(Codex)) }

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
			return Invocation{Shell: shell, Kind: ShellCmd}, nil
		}
	}
	shell, ok := findShell(doc, "SessionStart")
	if !ok {
		return Invocation{}, ErrNotConfigured
	}
	return Invocation{Shell: shell, Kind: clientShell(l.goos(), Codex)}, nil
}
