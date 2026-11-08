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
		line(integrate.Codex, "touch", "hook deadline exceeded", now.Add(-time.Hour)) +
		// Lines failureText never writes, as an older iris or another writer might have left.
		line(integrate.Codex, "touch", "unknown tool secret_old_tool", now.Add(-time.Hour)) +
		line(integrate.Codex, "secret_stage /Users/alice", "Post https://hub.private.example/v1: refused", now.Add(-time.Hour))
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
			{Client: integrate.Kimi, Status: "secret_status alice", Version: "built by alice"},
			{Client: "secret_client", Status: StatusOK},
		}},
	}
	raw, err := json.Marshal(NewBundle(res, "v1.2.3", now))
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, secret := range []string{"tok_SECRET_TOKEN", "hunter2", "alice", "hub.private.example", "/Users", "SECRET-MESSAGE-BODY", "secret_tool_name", "antigravity", "client.json", "pre-commit", "500", "secret_old_tool", "secret_stage", "secret_status", "secret_client", "refused", "built by"} {
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
		Clients: []BundleClient{{Client: integrate.Claude, Status: StatusSilent, Version: "2.1.319"}, {Client: integrate.Codex, Status: StatusOK, Version: "0.167.0"}, {Client: integrate.Kimi, Status: "other"}},
		// The older codex failure is two days old and outside the window.
		// Sorted by client, then stage; a name that is no client's is left out, and a stage or
		// text the hook never writes is "other".
		Failures: []BundleCount{
			{Client: integrate.Claude, Stage: "decode", Category: "invalid JSON", Count: 1},
			{Client: integrate.Claude, Stage: "session", Category: "hub request failed: lost", Count: 2},
			{Client: integrate.Codex, Stage: "other", Category: "other", Count: 1},
			{Client: integrate.Codex, Stage: "touch", Category: "hook deadline exceeded", Count: 1},
			{Client: integrate.Codex, Stage: "touch", Category: "other", Count: 1},
		},
		Observations: []BundleCount{{Client: integrate.Copilot, Stage: "observed", Category: "cross-load", Count: 1}, {Client: integrate.Copilot, Stage: "observed", Category: "unknown tool", Count: 1}},
		Guard:        "installed",
		CaseWarning:  true,
	}
	if got.Iris.OS == "" || got.Iris.Go == "" || fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("bundle\n%+v\nwant\n%+v", got, want)
	}
}

// A value the bundle does not know keeps no text at all: an observation kind, a guard line,
// a hub source or a client status formatted some other way later is "other".
func TestAnUnknownValueKeepsNoText(t *testing.T) {
	if got := observationKind("something new about secret_name"); got != "other" {
		t.Fatalf("observation %q", got)
	}
	b := NewBundle(Result{HubSource: "flag --hub https://secret.example", Guard: "secret state: /Users/alice/repo"}, "v1", time.Now())
	if b.Hub.Source != "other" || b.Guard != "other" {
		t.Fatalf("source %q, guard %q", b.Hub.Source, b.Guard)
	}
	for _, line := range []string{"not in a git repository", "unknown (exit status 128: /Users/alice)", "another pre-commit hook, without Iris (/r/.git/hooks/pre-commit); run `iris guard install`", "not installed (/r/.git/hooks/pre-commit); run `iris guard install`"} {
		if got := NewBundle(Result{Guard: line}, "v1", time.Now()).Guard; got == "other" || strings.Contains(got, "/") {
			t.Errorf("guard %q gave %q", line, got)
		}
	}
	if got := NewBundle(Result{HubSource: "environment (IRIS_HUB_URL)"}, "v1", time.Now()).Hub.Source; got != "environment" {
		t.Errorf("source %q", got)
	}
}
