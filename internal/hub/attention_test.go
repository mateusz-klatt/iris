package hub

import (
	"fmt"
	"testing"
)

// An events read may name the generation it expects and ask for the attention block, as planned
// for Codex's wake (ADR 017). Without a generation it is unqualified, as it always was: iris watch
// and older shims send none. Without asking it carries no block, as before.
func TestAnEventsReadNamesAGenerationAndAsksForAttention(t *testing.T) {
	_, s := setup(t)
	registered := request(t, s, "POST", "/v1/sessions", map[string]any{"client": "codex", "session_id": "a", "host": "h", "cwd": "/repo", "source": "hook"}, 200)
	base := "/v1/events?execution_id=" + registered["execution_id"].(string)
	generation := int(registered["generation"].(float64))
	if plain := request(t, s, "GET", base, nil, 200); plain["attention"] != nil {
		t.Fatalf("a read that did not ask got attention: %v", plain)
	}
	asked := request(t, s, "GET", fmt.Sprintf("%s&generation=%d&attention=1", base, generation), nil, 200)
	attention, ok := asked["attention"].(map[string]any)
	if !ok {
		t.Fatalf("no attention block: %v", asked)
	}
	if identity, present := attention["identity"]; !present || identity != nil {
		t.Fatalf("with nothing pending the identity is present and null: %v", attention)
	}
	if attention["high_water"] != asked["high_water"] {
		t.Fatalf("the block's high_water %v is not the read's %v", attention["high_water"], asked["high_water"])
	}
	stale := request(t, s, "GET", fmt.Sprintf("%s&generation=%d", base, generation+1), nil, 409)
	if stale["error"].(map[string]any)["code"] != "lost" {
		t.Fatalf("a stale generation: %v", stale)
	}
}
