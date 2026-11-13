package integrate

import (
	"flag"
	"io"
	"path/filepath"
	"strings"
)

// InstallKimiWake installs Kimi's integration with an explicit wake preference.
func InstallKimiWake(l Layout, enabled bool) error {
	return (kimiClient{}).install(l, &enabled)
}

// KimiWake reads the effective preference without starting a server or proving delivery.
func KimiWake(l Layout) (configured, enabled bool, err error) {
	files, err := (kimiClient{}).Files(l)
	if err != nil {
		return false, false, err
	}
	raw, _, err := readExisting(files[1])
	if err != nil {
		return false, false, err
	}
	doc, err := readObject(raw)
	if err != nil {
		return false, false, err
	}
	configured, enabled = kimiWake(doc, l.Iris())
	return configured, enabled, nil
}

func kimiWake(doc map[string]any, iris string) (bool, bool) {
	servers, _ := doc["mcpServers"].(map[string]any)
	server, _ := servers["iris"].(map[string]any)
	command, _ := server["command"].(string)
	name := strings.ToLower(filepath.Base(strings.ReplaceAll(command, `\`, "/")))
	if command == "" || command != iris && name != "iris" && name != "iris.exe" {
		return false, false
	}
	values, _ := server["args"].([]any)
	args := make([]string, 0, len(values))
	for _, value := range values {
		arg, ok := value.(string)
		if !ok {
			return false, false
		}
		args = append(args, arg)
	}
	if len(args) == 0 || args[0] != "mcp" {
		return false, false
	}
	flags := flag.NewFlagSet("Kimi Iris MCP", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	client := flags.String("client", "", "")
	wake := flags.Bool("wake", false, "")
	if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 || *client != Kimi {
		return false, false
	}
	return true, *wake
}

func kimiMCPConfig(doc map[string]any, iris, home string, preference *bool) error {
	configured, enabled := kimiWake(doc, iris)
	if !configured {
		enabled = false
	}
	if preference != nil {
		enabled = *preference
	}
	args := []any{"mcp", "--client", Kimi}
	if enabled {
		args = append(args, "--wake")
	} else {
		args = append(args, "--wake=false")
	}
	servers, _ := doc["mcpServers"].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
		doc["mcpServers"] = servers
	}
	server, _ := servers["iris"].(map[string]any)
	if server == nil {
		server = map[string]any{}
	}
	server["command"] = iris
	server["args"] = args
	env, _ := server["env"].(map[string]any)
	if env == nil {
		env = map[string]any{}
	}
	env["KIMI_CODE_HOME"] = home
	server["env"] = env
	servers["iris"] = server
	return nil
}
