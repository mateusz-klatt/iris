package integrate

import (
	"flag"
	"fmt"
	"io"
	"path"
	"reflect"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

// InstallCodexWake installs Codex's Iris integration with an explicit wake preference. Normal
// Client.Install preserves the preference already in Codex's Iris server table.
func InstallCodexWake(l Layout, enabled bool) error {
	return (codexClient{}).install(l, &enabled)
}

// CodexWake reports the configured Iris Codex invocation and its wake preference. It never
// starts a process or proves that a session is armed, a prompt ran or mail was delivered.
func CodexWake(l Layout) (configured, enabled bool, err error) {
	file, err := CodexConfig(l)
	if err != nil {
		return false, false, err
	}
	raw, _, err := readExisting(file)
	if err != nil {
		return false, false, err
	}
	state, err := codexWake(raw, l.Iris())
	return state.configured, state.enabled, err
}

type codexServerConfig struct {
	Servers map[string]any `toml:"mcp_servers"`
}

type codexWakeState struct {
	configured, enabled bool
	table               map[string]any
}

func codexWake(raw []byte, expectedCommand string) (codexWakeState, error) {
	var doc codexServerConfig
	if toml.Unmarshal(raw, &doc) != nil {
		return codexWakeState{}, fmt.Errorf("%w: invalid Codex TOML", ErrBroken)
	}
	table, _ := doc.Servers["iris"].(map[string]any)
	state := codexWakeState{table: table}
	command, _ := table["command"].(string)
	name := strings.ToLower(path.Base(strings.ReplaceAll(command, `\`, "/")))
	if command == "" || (command != expectedCommand && name != "iris" && name != "iris.exe" && !ownedCodexBlock(raw, table)) {
		return state, nil
	}
	values, _ := table["args"].([]any)
	var args []string
	for _, value := range values {
		arg, ok := value.(string)
		if !ok {
			return state, nil
		}
		args = append(args, arg)
	}
	if len(args) == 0 || args[0] != "mcp" {
		return state, nil
	}
	flags := flag.NewFlagSet("Codex Iris MCP", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	client := flags.String("client", "", "")
	wake := flags.Bool("wake", false, "")
	if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 || *client != Codex {
		return state, nil
	}
	state.configured, state.enabled = true, *wake
	return state, nil
}

// ownedCodexBlock requires the marked block to parse to the actual Iris table, with no Iris
// table left outside it. Marker text in a user's string establishes no ownership. The same
// proof permits preserving a managed custom command after the binary has moved.
func ownedCodexBlock(raw []byte, table map[string]any) bool {
	if table == nil {
		return false
	}
	text := string(raw)
	start, end := strings.Index(text, markerStart), strings.Index(text, markerEnd)
	if start < 0 || end < start || strings.Count(text, markerStart) != 1 || strings.Count(text, markerEnd) != 1 {
		return false
	}
	end += len(markerEnd)
	var inside, outside codexServerConfig
	if toml.Unmarshal([]byte(text[start:end]), &inside) != nil || toml.Unmarshal([]byte(text[:start]+text[end:]), &outside) != nil {
		return false
	}
	return reflect.DeepEqual(inside.Servers["iris"], table) && outside.Servers["iris"] == nil
}

// codexMCPConfig parses before editing, so multiline values and quoted tables cannot masquerade
// as the Iris command. Only Iris's marked block is replaced; unrelated configuration stays.
func codexMCPConfig(existing []byte, iris string, preference *bool) ([]byte, error) {
	state, err := codexWake(existing, iris)
	if err != nil {
		return nil, err
	}
	// Every edit of existing markers needs ownership proof, even when an unrelated server
	// happens to use the known Iris command name. Validate before hooks or backups are written.
	text := string(existing)
	if (strings.Contains(text, markerStart) || strings.Contains(text, markerEnd)) && !ownedCodexBlock(existing, state.table) {
		return nil, ErrBroken
	}
	enabled := state.enabled
	if preference != nil {
		enabled = *preference
	}
	args := `"mcp", "--client", "codex"`
	if enabled {
		args += `, "--wake"`
	}
	block := wrapBlock(fmt.Sprintf("[mcp_servers.iris]\ncommand = %s\nargs = [%s]\n%s", tomlString(iris), args, codexMCPPolicy))
	next, err := upsertBlock(string(existing), block)
	if err != nil {
		return nil, err
	}
	// Validate the generated command as well, so characters the writer cannot quote cannot
	// leave the client with broken TOML.
	verified, err := codexWake([]byte(next), iris)
	if err != nil || !verified.configured || verified.enabled != enabled {
		return nil, ErrBroken
	}
	return []byte(next), nil
}
