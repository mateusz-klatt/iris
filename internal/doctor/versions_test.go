package doctor

import (
	"strings"
	"testing"
)

func TestVersionBaselines(t *testing.T) {
	cases := []struct {
		client, version, warning string
		legacy                   bool
	}{
		{"claude", "2.1.318 (Claude Code)", "", false},
		{"codex", "codex-cli 0.163.0", "0.166.0", false},
		{"copilot", "GitHub Copilot CLI 1.0.105.", "", false},
		{"cursor", "2026.08.11-e8db854", "2026.11.05", false},
		{"antigravity", "1.1.18", "1.5.1", false},
		{"grok", "grok 1.0.5 (5115b46bc9) [stable]", "1.0.77", false},
		{"kimi", "0.38.0", "", true},
		{"kimi", "Kimi Code 1.9.99", "", true},
		{"kimi", "Kimi Code 2.0.0", "2.5.1", false},
		{"kimi", "Kimi Code 2.5.1", "", false},
		{"grok", "grok 1.0.100", "", false},
		{"grok", "development build", "", false},
		{"kimi", "development build", "", false},
		{"grok", "99999999999999999999999999.0.0", "", false},
		{"unmeasured", "1.2.3", "", false},
	}
	for _, tc := range cases {
		for _, status := range []string{StatusOK, StatusSilent, StatusNeedsHuman, StatusNotConfigured} {
			t.Run(tc.client+"/"+tc.version+"/"+status, func(t *testing.T) {
				row := ClientReport{Client: tc.client, Version: tc.version, Status: status, Detail: "existing detail"}
				applyVersion(&row)
				if tc.legacy {
					if row.Status != StatusNeedsHuman || row.Detail != "Kimi Code older than version 2; Iris needs version 2 or later" {
						t.Fatalf("%+v", row)
					}
					return
				}
				want := "existing detail"
				if tc.warning != "" {
					want += "; older than the tested " + tc.warning
				}
				if row.Status != status || row.Detail != want {
					t.Fatalf("%+v want detail %s", row, want)
				}
			})
		}
	}
}
func TestTokenFormat(t *testing.T) {
	for _, tc := range []struct {
		up, valid bool
		want      string
	}{{false, false, "not checked"}, {true, false, "not valid"}, {true, true, "ok"}} {
		got := (Result{HubUp: tc.up, TokenOK: tc.valid}).Format()
		if !strings.Contains(got, "token: "+tc.want+"\n") {
			t.Fatal(got)
		}
	}
}
