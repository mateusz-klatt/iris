package integrate

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// agy denies every Iris call in a headless run unless the user's settings allow the Iris
// server. Install adds exactly that rule, keeps the user's settings, and remove restores them.
func TestAntigravityAllowsOnlyTheIrisServerAndRemovesExactlyThat(t *testing.T) {
	l := testLayout(t)
	agy, _ := ByName(Antigravity)
	files, _ := agy.Files(l)
	settings := files[2]
	if err := os.MkdirAll(filepath.Dir(settings), 0o700); err != nil {
		t.Fatal(err)
	}
	mine := "{\n  \"permissions\": {\n    \"allow\": [\n      \"write_file(/work/*)\"\n    ]\n  },\n  \"trustedWorkspaces\": [\n    \"/work\"\n  ]\n}\n"
	if err := os.WriteFile(settings, []byte(mine), 0o600); err != nil {
		t.Fatal(err)
	}
	for range 2 { // idempotent
		if err := agy.Install(l, nil); err != nil {
			t.Fatal(err)
		}
	}
	got := string(mustRead(t, settings))
	if strings.Count(got, AgyAllowRule) != 1 || !strings.Contains(got, "write_file(/work/*)") || !strings.Contains(got, "trustedWorkspaces") {
		t.Fatalf("settings after install\n%s", got)
	}
	if err := agy.Remove(l, nil); err != nil {
		t.Fatal(err)
	}
	if got := string(mustRead(t, settings)); got != mine {
		t.Fatalf("remove did not restore the user's settings\n got %q\nwant %q", got, mine)
	}
}

func TestAntigravitySettingsCreatedByIrisGoAway(t *testing.T) {
	l := testLayout(t)
	agy, _ := ByName(Antigravity)
	files, _ := agy.Files(l)
	if err := agy.Install(l, nil); err != nil {
		t.Fatal(err)
	}
	if got := string(mustRead(t, files[2])); !strings.Contains(got, AgyAllowRule) {
		t.Fatal(got)
	}
	if err := agy.Remove(l, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(files[2]); !os.IsNotExist(err) {
		t.Fatalf("created settings left behind: %v", err)
	}
}

func TestAllowRulesKeepOtherRulesAndRefuseWrongTypes(t *testing.T) {
	doc := map[string]any{"permissions": map[string]any{"allow": []any{"a", AgyAllowRule}, "deny": []any{"b"}}}
	if !RemoveAllowRule(doc, AgyAllowRule) || RemoveAllowRule(doc, AgyAllowRule) {
		t.Fatal("remove should report once")
	}
	perms := doc["permissions"].(map[string]any)
	if len(perms["allow"].([]any)) != 1 || perms["deny"] == nil {
		t.Fatal(doc)
	}
	only := map[string]any{"permissions": map[string]any{"allow": []any{AgyAllowRule}}}
	if !RemoveAllowRule(only, AgyAllowRule) || len(only) != 0 {
		t.Fatalf("empty containers left: %v", only)
	}
	for _, bad := range []map[string]any{{"permissions": "x"}, {"permissions": map[string]any{"allow": "x"}}} {
		if err := AddAllowRule(bad, AgyAllowRule); !errors.Is(err, ErrBroken) {
			t.Fatalf("%v: %v", bad, err)
		}
	}
}
