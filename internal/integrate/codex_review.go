package integrate

import (
	"fmt"
	"os"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

// Reviewer is a client whose readiness depends on state only a person can
// create, so it is read at check time instead of reported as a fixed step.
type Reviewer interface {
	// Review returns the step still needed, or "" with a note on what was
	// and was not verified.
	Review(Layout) (step, note string)
}

// codexEvents are the hook events Iris installs, with the snake_case names
// Codex uses in hooks.state keys.
var codexEvents = []struct{ event, key string }{
	{"SessionStart", "session_start"},
	{"UserPromptSubmit", "user_prompt_submit"},
	{"PreToolUse", "pre_tool_use"},
	{"PostToolUse", "post_tool_use"},
	{"Stop", "stop"},
	{"SessionEnd", "session_end"},
}

// codexMCPKeys must be in the iris server table; an integration written
// before they existed keeps working, but sends MCP calls to the configured
// hub under an override and asks before every Iris tool.
var codexMCPKeys = []string{"env_vars", "default_tools_approval_mode"}

const codexReviewed = "hook review recorded; hash not verified; `iris doctor --live --client codex` proves the hooks run"

// Review reads what Codex records when a person trusts the hooks in /hooks:
// a [hooks.state."<hooks.json>:<event>:<group>:<hook>"] table with a
// trusted_hash, one per hook. Every Iris hook needs one, keyed by the
// hooks.json path Codex builds from HOME or CODEX_HOME, exactly as Iris
// wrote it. Keys are compared after TOML decoding: Codex writes a Windows
// path as a literal key, [hooks.state.'C:\...'], and others as basic strings.
// The hash is not checked: it could not be reproduced (measured on
// 0.167.0), so a table left by an earlier hook at the same position would
// also pass. Only a live run proves that the hooks run.
func (c codexClient) Review(l Layout) (step, note string) {
	files, err := c.Files(l)
	if err != nil {
		return c.Human(), ""
	}
	config, _ := os.ReadFile(files[1])
	var steps []string
	if missing := staleMCP(string(config)); len(missing) > 0 {
		steps = append(steps, fmt.Sprintf("the Iris MCP server table lacks %s: run `iris integrate codex`", strings.Join(missing, " and ")))
	}
	if !hooksReviewed(files[0], string(config)) {
		steps = append(steps, c.Human())
	}
	if len(steps) > 0 {
		return strings.Join(steps, "; "), ""
	}
	return "", codexReviewed
}

func staleMCP(config string) []string {
	start := strings.Index(config, markerStart)
	end := strings.Index(config, markerEnd)
	if start < 0 || end < start {
		return nil
	}
	block := config[start:end]
	var missing []string
	for _, key := range codexMCPKeys {
		if !strings.Contains(block, "\n"+key+" =") {
			missing = append(missing, key)
		}
	}
	return missing
}

func hooksReviewed(hooksFile, config string) bool {
	doc, err := readJSON(hooksFile)
	if err != nil {
		return false
	}
	root, _ := doc.(map[string]any)
	hooks, _ := root["hooks"].(map[string]any)
	trusted := trustedHooks(config)
	for _, e := range codexEvents {
		groups, _ := hooks[e.event].([]any)
		group := -1
		for i, g := range groups {
			if treeContains(g, "--client codex --event "+e.event) {
				group = i
			}
		}
		if group < 0 || !trusted[fmt.Sprintf("%s:%s:%d:0", hooksFile, e.key, group)] {
			return false
		}
	}
	return true
}

// trustedHooks returns the decoded hooks.state keys whose table carries a
// trusted_hash. A config that does not decode, including one with a
// hooks.state entry that is not a table, trusts nothing.
func trustedHooks(config string) map[string]bool {
	var doc struct {
		Hooks struct {
			State map[string]map[string]any `toml:"state"`
		} `toml:"hooks"`
	}
	out := map[string]bool{}
	if toml.Unmarshal([]byte(config), &doc) != nil {
		return out
	}
	for key, table := range doc.Hooks.State {
		if _, ok := table["trusted_hash"]; ok {
			out[key] = true
		}
	}
	return out
}
