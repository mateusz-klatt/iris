package integrate

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func assertKimiWake(t *testing.T, layout Layout, configured, enabled bool) {
	t.Helper()
	gotConfigured, gotEnabled, err := KimiWake(layout)
	if err != nil || gotConfigured != configured || gotEnabled != enabled {
		t.Fatalf("Kimi wake = %v/%v, %v; want %v/%v", gotConfigured, gotEnabled, err, configured, enabled)
	}
}

func TestKimiWakePreferenceSurvivesReintegrationAndExplicitChanges(t *testing.T) {
	layout := testLayout(t)
	customHome := filepath.Join(layout.Home, "custom-kimi")
	layout.Getenv = env(map[string]string{"KIMI_CODE_HOME": customHome})
	client := kimiClient{}
	assertKimiWake(t, layout, false, false)
	if err := client.Install(layout, nil); err != nil {
		t.Fatal(err)
	}
	assertKimiWake(t, layout, true, true)
	files, err := client.Files(layout)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	raw := readFiles(t, files)[1]
	if json.Unmarshal([]byte(raw), &doc) != nil {
		t.Fatalf("invalid MCP JSON: %s", raw)
	}
	server := doc["mcpServers"].(map[string]any)["iris"].(map[string]any)
	if !reflect.DeepEqual(server["args"], []any{"mcp", "--client", "kimi", "--wake"}) {
		t.Fatalf("fresh wake args: %+v", server["args"])
	}
	if server["env"].(map[string]any)["KIMI_CODE_HOME"] != customHome {
		t.Fatalf("custom Kimi home was not forwarded: %+v", server["env"])
	}
	server["startupTimeoutMs"] = 12345
	server["env"].(map[string]any)["USER_SETTING"] = "retained"
	encoded, _ := json.Marshal(doc)
	if err := os.WriteFile(files[1], encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := InstallKimiWake(layout, false); err != nil {
		t.Fatal(err)
	}
	assertKimiWake(t, layout, true, false)
	for range 2 {
		if err := client.Install(layout, nil); err != nil {
			t.Fatal(err)
		}
		assertKimiWake(t, layout, true, false)
	}
	if err := InstallKimiWake(layout, true); err != nil {
		t.Fatal(err)
	}
	assertKimiWake(t, layout, true, true)
	raw = readFiles(t, files)[1]
	if json.Unmarshal([]byte(raw), &doc) != nil {
		t.Fatal("invalid final JSON")
	}
	server = doc["mcpServers"].(map[string]any)["iris"].(map[string]any)
	if server["startupTimeoutMs"] != float64(12345) || server["env"].(map[string]any)["USER_SETTING"] != "retained" {
		t.Fatalf("unrelated Iris server settings were lost: %+v", server)
	}
	if drift, err := layout.Drift(client); err != nil || len(drift) != 0 {
		t.Fatalf("enabled Kimi wake drifted: %v, %v", drift, err)
	}
}

func TestKimiWakeReadRejectsBrokenFilesAndMissingHome(t *testing.T) {
	layout := testLayout(t)
	files, err := (kimiClient{}).Files(layout)
	if err != nil {
		t.Fatal(err)
	}
	seedFile(t, files[1], "{")
	if _, _, err := KimiWake(layout); err == nil {
		t.Fatal("broken JSON was accepted")
	}
	if _, _, err := KimiWake(homeless(t)); err == nil {
		t.Fatal("missing home was accepted")
	}
}

func TestKimiWakeParsesOnlyAValidKimiInvocation(t *testing.T) {
	for _, tc := range []struct {
		name       string
		server     map[string]any
		configured bool
		enabled    bool
	}{
		{"empty", nil, false, false},
		{"other command", map[string]any{"command": "other", "args": []any{"mcp", "--client", "kimi"}}, false, false},
		{"windows Iris", map[string]any{"command": `C:\tools\iris.exe`, "args": []any{"mcp", "--client", "kimi"}}, true, true},
		{"non-string arg", map[string]any{"command": "iris", "args": []any{1}}, false, false},
		{"no args", map[string]any{"command": "iris"}, false, false},
		{"other command args", map[string]any{"command": "iris", "args": []any{"other"}}, false, false},
		{"bad flag", map[string]any{"command": "iris", "args": []any{"mcp", "--wake=bad"}}, false, false},
		{"positional", map[string]any{"command": "iris", "args": []any{"mcp", "extra"}}, false, false},
		{"wrong client", map[string]any{"command": "iris", "args": []any{"mcp", "--client", "codex"}}, false, false},
		{"explicit off", map[string]any{"command": "iris", "args": []any{"mcp", "--client", "kimi", "--wake=false"}}, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc := map[string]any{}
			if tc.server != nil {
				doc["mcpServers"] = map[string]any{"iris": tc.server}
			}
			configured, enabled := kimiWake(doc, "iris")
			if configured != tc.configured || enabled != tc.enabled {
				t.Fatalf("parsed %v/%v; want %v/%v", configured, enabled, tc.configured, tc.enabled)
			}
		})
	}
}

func TestKimiInstallRejectsBrokenMCPBeforeWriting(t *testing.T) {
	layout := testLayout(t)
	files, err := (kimiClient{}).Files(layout)
	if err != nil {
		t.Fatal(err)
	}
	seedFile(t, files[0], "# user config\n")
	seedFile(t, files[1], "{")
	before := readFiles(t, files)
	if err := (kimiClient{}).Install(layout, nil); !errors.Is(err, ErrBroken) {
		t.Fatalf("broken MCP install error: %v", err)
	}
	after := readFiles(t, files)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("broken MCP file was changed")
	}
}

func TestKimiWakeAndInstallRefuseAnUnreadableMCPPath(t *testing.T) {
	layout := testLayout(t)
	files, err := (kimiClient{}).Files(layout)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(files[1], 0o700); err != nil {
		t.Fatal(err)
	}
	if _, _, err := KimiWake(layout); err == nil {
		t.Fatal("directory MCP path was accepted by the preference reader")
	}
	if err := (kimiClient{}).Install(layout, nil); err == nil {
		t.Fatal("directory MCP path was accepted by install")
	}
	if _, err := os.Stat(files[0]); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed MCP read wrote hooks: %v", err)
	}
}
