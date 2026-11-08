package doctor

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/integrate"
)

// A bundle is made to be pasted where others read it, so nothing private may reach it: a run
// whose every free-text field carries a secret, a path, a host or a message body gives a bundle
// with none of them, and with the states, versions and counts someone helping needs.
func TestABundleCarriesNoSecretOnlyStatesAndCounts(t *testing.T) {
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	token := filepath.Join(dir, "hub.token")
	if err := os.WriteFile(token, []byte("tok_SECRET_TOKEN"), 0o600); err != nil {
		t.Fatal(err)
	}
	line := func(client, stage, text string, at time.Time) string {
		return fmt.Sprintf(`{"time":%q,"client":%q,"event":"pre_tool","stage":%q,"error":%q}`+"\n", at.Format(time.RFC3339), client, stage, text)
	}
	failures := line(integrate.Claude, "session", "hub request failed: lost", now.Add(-time.Hour)) +
		line(integrate.Claude, "session", "hub request failed: lost", now.Add(-2*time.Hour)) +
		line(integrate.Codex, "decode", "invalid JSON at byte 7", now.Add(-48*time.Hour)) +
		line(integrate.Claude, "decode", "invalid JSON at byte 3", now.Add(-time.Hour)) +
		line("not-a-client", "session", "hub request failed", now.Add(-time.Hour)) +
		line(integrate.Codex, "touch", "hook deadline exceeded", now.Add(-time.Hour))
	observed := line(integrate.Copilot, "observed", "unknown tool secret_tool_name", now.Add(-time.Hour)) +
		line(integrate.Copilot, "observed", "cross-load: ran in a session of antigravity", now.Add(-time.Hour))
	if err := os.WriteFile(filepath.Join(dir, "hook-failures.log"), []byte(failures), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "hook-observed.log"), []byte(observed), 0o600); err != nil {
		t.Fatal(err)
	}
	res := Result{tokenFile: token,
		HubURL: "https://alice:hunter2@hub.private.example", HubSource: "file (/Users/alice/.config/iris/client.json)",
		HubUp: true, TokenOK: true, TokenDetail: "token tok_SECRET_TOKEN was rejected", HubDetail: "failure log: open /Users/alice/x: denied",
		DatabaseBytes: 4096, Stored: true, StoreError: "PUT https://hub.private.example: 500",
		Guard: "installed (/Users/alice/repo/.git/hooks/pre-commit)",
		Case:  "this repository is case-sensitive at the hub, but /Users/alice/repo folds case",
		Report: Report{Clients: []ClientReport{
			{Client: integrate.Claude, Status: StatusSilent, Version: "2.1.319 (Claude Code)", Detail: "recent hook failure: body was SECRET-MESSAGE-BODY in /Users/alice/repo", LastHookAt: "2026-11-08T11:00:00Z"},
			{Client: integrate.Codex, Status: StatusOK, Version: "codex-cli 0.167.0"},
		}},
	}
	raw, err := json.Marshal(NewBundle(res, "v1.2.3", now))
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, secret := range []string{"tok_SECRET_TOKEN", "hunter2", "alice", "hub.private.example", "/Users", "SECRET-MESSAGE-BODY", "secret_tool_name", "antigravity", "client.json", "pre-commit", "500"} {
		if strings.Contains(text, secret) {
			t.Errorf("the bundle carries %q:\n%s", secret, text)
		}
	}
	var got Bundle
	if err = json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	want := Bundle{
		Iris:    BundleIris{Version: "v1.2.3", OS: got.Iris.OS, Arch: got.Iris.Arch, Go: got.Iris.Go},
		Hub:     BundleHub{Source: "file", Up: true, TokenOK: true, Stored: true, DatabaseBytes: 4096},
		Clients: []BundleClient{{Client: integrate.Claude, Status: StatusSilent, Version: "2.1.319 (Claude Code)"}, {Client: integrate.Codex, Status: StatusOK, Version: "codex-cli 0.167.0"}},
		// The older codex failure is two days old and outside the window.
		// Sorted by client, then stage; a name that is no client's is left out.
		Failures:     []BundleCount{{Client: integrate.Claude, Stage: "decode", Category: "invalid JSON at byte 3", Count: 1}, {Client: integrate.Claude, Stage: "session", Category: "hub request failed: lost", Count: 2}, {Client: integrate.Codex, Stage: "touch", Category: "hook deadline exceeded", Count: 1}},
		Observations: []BundleCount{{Client: integrate.Copilot, Stage: "observed", Category: "cross-load", Count: 1}, {Client: integrate.Copilot, Stage: "observed", Category: "unknown tool", Count: 1}},
		Guard:        "installed",
		CaseWarning:  true,
	}
	if got.Iris.OS == "" || got.Iris.Go == "" || fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("bundle\n%+v\nwant\n%+v", got, want)
	}
}

// An observation of a kind the bundle does not know keeps no text at all.
func TestAnUnknownObservationKindKeepsNoText(t *testing.T) {
	if got := observationKind("something new about secret_name"); got != "other" {
		t.Fatalf("%q", got)
	}
}
