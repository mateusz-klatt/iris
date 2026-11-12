package doctor

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/hook"
	"github.com/mateusz-klatt/iris/internal/integrate"
	"github.com/mateusz-klatt/iris/internal/wake"
)

func TestDoctorWakeConfigurationNeverClaimsArmingOrDelivery(t *testing.T) {
	config, inspect := wakeConfiguration, inspectWakes
	t.Cleanup(func() { wakeConfiguration, inspectWakes = config, inspect })
	inspectWakes = func(string) ([]wake.Inspection, error) { return nil, nil }
	for _, tc := range []struct {
		name                string
		configured, enabled bool
		err                 error
		want                string
	}{{"absent", false, false, nil, "no Codex Iris MCP entry"}, {"default off", true, false, nil, "hooks; idle wake off (default)"}, {"opt in", true, true, nil, "opt-in idle wake; current arming and delivery are unverified"}, {"unknown", false, false, errors.New("PRIVATE_ERROR"), "unknown; Codex configuration could not be read"}} {
		wakeConfiguration = func(integrate.Layout) (bool, bool, error) { return tc.configured, tc.enabled, tc.err }
		lines := wakeReport(integrate.Layout{}, "token", time.Now())
		text := strings.Join(lines, "\n")
		if !strings.Contains(text, tc.want) || strings.Contains(text, "PRIVATE_ERROR") {
			t.Fatalf("%s: %s", tc.name, text)
		}
		if tc.enabled && !strings.Contains(text, "exec probes do not prove idle wake delivery") {
			t.Fatal("exec live probe became an idle wake proof")
		}
	}
	wakeConfiguration = func(integrate.Layout) (bool, bool, error) { return true, true, nil }
	for _, err := range []error{hook.ErrSessionsLocked, errors.New("PRIVATE_IO_ERROR")} {
		inspectWakes = func(string) ([]wake.Inspection, error) {
			return []wake.Inspection{{Path: "guard", Status: "unusable"}}, err
		}
		text := strings.Join(wakeReport(integrate.Layout{}, "token", time.Now()), "\n")
		want := "inspection unavailable"
		if errors.Is(err, hook.ErrSessionsLocked) {
			want = "inspection busy"
		}
		if !strings.Contains(text, want) || !strings.Contains(text, "submissions remain blocked") || strings.Contains(text, "PRIVATE_IO_ERROR") {
			t.Fatalf("unsafe error report: %s", text)
		}
	}
}

func TestDoctorWakeGuardSummariesAreBoundedAndRecoveryRequiresClosedSession(t *testing.T) {
	now := time.Unix(1800000000, 0)
	base := wake.Inspection{Path: "/private/guard\nname", Status: "usable", Attempts: 1, Kind: "queue", Outcome: "unknown", Outstanding: true, StartedMS: now.Add(-11 * time.Minute).UnixMilli(), Slot0: 2}
	for _, tc := range []struct {
		name   string
		change func(*wake.Inspection)
		want   string
	}{{"old", func(*wake.Inspection) {}, "old outstanding queue wake"}, {"recent", func(g *wake.Inspection) { g.StartedMS = now.Add(-time.Minute).UnixMilli() }, "outstanding queue wake (unknown), age 1m0s"}, {"future clock", func(g *wake.Inspection) { g.StartedMS = now.Add(time.Minute).UnixMilli() }, "age 0s"}, {"consumed", func(g *wake.Inspection) { g.Outstanding = false; g.ConsumedMS = now.UnixMilli() }, "consumed queue wake; its slot remains spent"}, {"binary failure", func(g *wake.Inspection) {
		g.Outstanding = false
		g.Kind = "binary_check"
		g.Outcome = "failed"
		g.Slot0 = 1
	}, "known binary check failure"}, {"abort", func(g *wake.Inspection) { g.Outstanding = false; g.Outcome = "aborted" }, "queue attempt (aborted)"}, {"empty", func(g *wake.Inspection) { g.Outstanding = false; g.Kind = "" }, "no attempts"}, {"malformed", func(g *wake.Inspection) { g.Status = "unusable" }, "unusable; submissions remain blocked"}, {"unreadable", func(g *wake.Inspection) { g.Status = "unreadable" }, "unreadable; submissions remain blocked"}} {
		g := base
		tc.change(&g)
		line := wakeGuardLine(g, now)
		if !strings.Contains(line, tc.want) || strings.Contains(line, "\n") {
			t.Fatalf("%s: unsafe guard summary %q", tc.name, line)
		}
		if g.Outstanding || g.Status != "usable" {
			for _, limit := range []string{"Resume a live session", "only after permanently closing that session and stopping its MCP child", "removing a live guard can admit duplicate queue entries"} {
				if !strings.Contains(line, limit) {
					t.Fatalf("unsafe recovery guidance lacks %q: %s", limit, line)
				}
			}
		}
	}
	metadata := base
	metadata.CheckedMS = now.UnixMilli()
	metadata.FailureMS = now.Add(-time.Second).UnixMilli()
	metadata.BinaryPath = "/codex\npath"
	metadata.BinaryVersion = "version\nvalue"
	metadata.ClientVersion = "client\tvalue"
	line := wakeGuardLine(metadata, now)
	for _, want := range []string{`path "/codex\npath"`, `binary version "version\nvalue"`, `client version "client\tvalue"`, "last recorded failure", "retained attempts 1/5; initial wake allowance spent, repeat wake allowance unused"} {
		if !strings.Contains(line, want) {
			t.Fatalf("missing quoted metadata %q: %s", want, line)
		}
	}
	if strings.Contains(line, "\n") {
		t.Fatal("metadata injected a diagnostic line")
	}
	if !strings.Contains(line, "last recorded failure "+time.UnixMilli(metadata.FailureMS).UTC().Format(time.RFC3339)+": Unrecognized wake failure") {
		t.Fatal("failure time omitted its fixed fallback reason")
	}
	for _, reason := range []string{"Codex executable is not available on PATH", "Codex executable and initialized client versions differ"} {
		metadata.FailureReason = reason
		if line := wakeGuardLine(metadata, now); !strings.Contains(line, ": "+reason) {
			t.Fatalf("known failure reason was omitted: %s", line)
		}
	}
}

func TestDoctorWakeReadOnlyDiagnosticsAreLocalAndKeepOfflineExit(t *testing.T) {
	home := t.TempDir()
	token := filepath.Join(home, "hub.token")
	layout := integrate.Layout{Home: home, StateDir: filepath.Join(home, "state"), Getenv: func(string) string { return "" }, LookPath: func(string) (string, error) { return "", os.ErrNotExist }}
	res, err := Run(context.Background(), Options{Layout: layout, TokenFile: token, HubURL: "http://127.0.0.1:1", Dir: home})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Wake) != 1 || !strings.Contains(res.Wake[0], "no Codex Iris MCP entry") || res.ExitCode() != 1 {
		t.Fatalf("offline diagnostics changed exit behavior: %+v", res)
	}
	if _, err := os.Stat(filepath.Join(home, "sessions")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("offline doctor created wake state: %v", err)
	}
	text := res.Format()
	if !strings.Contains(text, "codex wake: "+res.Wake[0]+"\n") {
		t.Fatal("local wake diagnosis was omitted")
	}
	if strings.Contains((Result{}).Format(), "codex wake:") {
		t.Fatal("empty local diagnostics printed a wake proof")
	}
	if code := (Result{HubUp: true, TokenOK: true, Wake: []string{"guard is unusable"}}).ExitCode(); code != 0 {
		t.Fatal("wake diagnostics changed existing offline exit grading")
	}
	stored, err := json.Marshal(res.Report)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(stored), "codex wake:") || strings.Contains(string(stored), "Wake") {
		t.Fatal("local diagnostics changed the stored hub schema")
	}
}

func TestDoctorWakeReadsActualOptInAndRetainedGuardWithoutSecrets(t *testing.T) {
	home := t.TempDir()
	token := filepath.Join(home, "hub.token")
	layout := integrate.Layout{Home: home, Getenv: func(string) string { return "" }}
	config, err := integrate.CodexConfig(layout)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(config), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config, []byte("[mcp_servers.iris]\ncommand = \"/expected/iris\"\nargs = [\"mcp\", \"--client\", \"codex\", \"--wake\"]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	sessions := hook.NewSessions(token)
	guard := sessions.WakePath("codex", "session")
	if err := os.MkdirAll(filepath.Dir(guard), 0700); err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"version":1,"rev":1,"attempts":[{"id":"00000000000000000000000000000001","kind":"queue","stream":"stream","cursor":1,"slot":0,"started_ms":100,"token":"PRIVATE_TOKEN","message":"PRIVATE_PROMPT","answer":"PRIVATE_OUTCOME","answered_ms":101,"detail":"PRIVATE_DETAIL"}],"last_failure":{"at_ms":102,"detail":"PRIVATE_FAILURE"}}`)
	if err := os.WriteFile(guard, body, 0600); err != nil {
		t.Fatal(err)
	}
	lines := wakeReport(layout, token, time.Unix(1800000000, 0))
	text := strings.Join(lines, "\n")
	for _, want := range []string{"opt-in idle wake; current arming and delivery are unverified", strconv.Quote(guard), "old outstanding queue wake (unknown)", "permanently closing that session and stopping its MCP child"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing actual wake evidence %q: %s", want, text)
		}
	}
	if strings.Contains(text, "PRIVATE_") {
		t.Fatal("diagnostic exported guard secrets or untrusted detail")
	}
	if !strings.Contains(text, "Unrecognized wake failure") {
		t.Fatal("unknown stored failure had no safe diagnostic reason")
	}
	after, err := os.ReadFile(guard)
	if err != nil || string(after) != string(body) {
		t.Fatal("diagnostic changed retained guard")
	}
}
