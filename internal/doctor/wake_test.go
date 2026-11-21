package doctor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
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
	}{{"absent", false, false, nil, "no Codex Iris MCP entry"}, {"explicitly disabled", true, false, nil, "hooks; idle wake explicitly disabled"}, {"default on", true, true, nil, "idle wake (default); current arming and delivery are unverified"}, {"unknown", false, false, errors.New("PRIVATE_ERROR"), "unknown; Codex configuration could not be read"}} {
		wakeConfiguration = func(integrate.Layout) (bool, bool, error) { return tc.configured, tc.enabled, tc.err }
		lines := wakeReport(integrate.Layout{}, "token", time.Now())
		text := strings.Join(lines, "\n")
		if !strings.Contains(text, tc.want) || strings.Contains(text, "PRIVATE_ERROR") {
			t.Fatalf("%s: %s", tc.name, text)
		}
		if tc.enabled {
			for _, want := range []string{"one automatic wake per attention identity", "one shared retry after a known queue failure", "Retained legacy attempts count toward that same allowance", "exec probes do not prove idle wake delivery"} {
				if !strings.Contains(text, want) {
					t.Fatalf("wake limits omit %q: %s", want, text)
				}
			}
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
	base := wake.Inspection{Path: "/private/guard\nname", Status: "usable", Client: "codex", Attempts: 1, Kind: "queue", Outcome: "unknown", Outstanding: true, StartedMS: now.Add(-11 * time.Minute).UnixMilli(), Slot0: 2}
	for _, tc := range []struct {
		name   string
		change func(*wake.Inspection)
		want   string
	}{{"old", func(*wake.Inspection) {}, "old outstanding queue wake"}, {"recent", func(g *wake.Inspection) { g.StartedMS = now.Add(-time.Minute).UnixMilli() }, "outstanding queue wake (unknown), age 1m0s"}, {"future clock", func(g *wake.Inspection) { g.StartedMS = now.Add(time.Minute).UnixMilli() }, "age 0s"}, {"consumed", func(g *wake.Inspection) { g.Outstanding = false; g.ConsumedMS = now.UnixMilli() }, "consumed queue wake; its automatic wake allowance remains spent"}, {"binary failure", func(g *wake.Inspection) {
		g.Outstanding = false
		g.Kind = "binary_check"
		g.Outcome = "failed"
		g.Slot0 = 1
	}, "known binary check failure"}, {"abort", func(g *wake.Inspection) { g.Outstanding = false; g.Outcome = "aborted" }, "queue attempt (aborted)"}, {"empty", func(g *wake.Inspection) { g.Outstanding = false; g.Kind = "" }, "no attempts"}, {"malformed", func(g *wake.Inspection) { g.Status, g.Client = "unusable", "" }, "unusable; client not recorded; submissions remain blocked"}, {"unreadable", func(g *wake.Inspection) { g.Status, g.Client = "unreadable", "" }, "unreadable; client not recorded; submissions remain blocked"}} {
		g := base
		tc.change(&g)
		line := wakeGuardLine(g, now)
		if !strings.Contains(line, tc.want) || strings.Contains(line, "\n") {
			t.Fatalf("%s: unsafe guard summary %q", tc.name, line)
		}
		if g.Status != "usable" {
			if !strings.Contains(line, neutralWakeRecovery) || strings.Contains(line, wakeRecovery) {
				t.Fatalf("an unreadable guard got another client's recovery: %s", line)
			}
		} else if g.Outstanding {
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
	for _, want := range []string{`path "/codex\npath"`, `binary version "version\nvalue"`, `client version "client\tvalue"`, "last recorded failure", "retained attempts 1/5; initial accounting spent, retained legacy accounting unused; automatic wake allowance spent"} {
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

func TestDoctorWakeSharesItsSingleAllowanceWithRetainedLegacyAttempts(t *testing.T) {
	for _, tc := range []struct {
		name                       string
		slot0, slot1               int
		initial, legacy, allowance string
	}{
		{"unused", 0, 0, "unused", "unused", "unused"},
		{"one initial failure", 1, 0, "one known failed attempt", "unused", "has one retry"},
		{"one retained legacy failure", 0, 1, "unused", "one known failed attempt", "has one retry"},
		{"two failures across both ledgers", 1, 1, "one known failed attempt", "one known failed attempt", "spent"},
		{"consumed initial wake", 2, 0, "spent", "unused", "spent"},
		{"retained legacy wake", 0, 2, "unused", "spent", "spent"},
		{"both ledgers spent", 2, 2, "spent", "spent", "spent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			line := wakeGuardLine(wake.Inspection{Path: "guard", Status: "usable", Client: "codex", Slot0: tc.slot0, Slot1: tc.slot1}, time.Now())
			want := "initial accounting " + tc.initial + ", retained legacy accounting " + tc.legacy + "; automatic wake allowance " + tc.allowance
			if !strings.Contains(line, want) || strings.Contains(line, "repeat wake allowance") {
				t.Fatalf("doctor offers a separate or incorrect wake allowance: %s", line)
			}
			if tc.allowance == "spent" && strings.Contains(line, "retry") {
				t.Fatalf("spent combined allowance offers another retry: %s", line)
			}
		})
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
	if !strings.Contains(text, res.Wake[0]+"\n") {
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

func TestDoctorWakeReadsActualDefaultAndRetainedGuardWithoutSecrets(t *testing.T) {
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
	if err := os.WriteFile(config, []byte("[mcp_servers.iris]\ncommand = \"/expected/iris\"\nargs = [\"mcp\", \"--client\", \"codex\"]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	sessions := hook.NewSessions(token)
	guard := sessions.WakePath("codex", "session")
	if err := os.MkdirAll(filepath.Dir(guard), 0700); err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"version":1,"rev":1,"client":"codex","attempts":[{"id":"00000000000000000000000000000001","kind":"queue","stream":"stream","cursor":1,"slot":0,"started_ms":100,"token":"PRIVATE_TOKEN","message":"PRIVATE_PROMPT","answer":"PRIVATE_OUTCOME","answered_ms":101,"detail":"PRIVATE_DETAIL"}],"last_failure":{"at_ms":102,"detail":"PRIVATE_FAILURE"}}`)
	if err := os.WriteFile(guard, body, 0600); err != nil {
		t.Fatal(err)
	}
	lines := wakeReport(layout, token, time.Unix(1800000000, 0))
	text := strings.Join(lines, "\n")
	for _, want := range []string{"idle wake (default); current arming and delivery are unverified", strconv.Quote(guard), "old outstanding queue wake (unknown)", "permanently closing that session and stopping its MCP child"} {
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

func TestDoctorWakeReadsExplicitDisableAfterOrdinaryReintegration(t *testing.T) {
	home := t.TempDir()
	layout := integrate.Layout{Home: home, StateDir: filepath.Join(home, "state"), Getenv: func(string) string { return "" }}
	if err := integrate.InstallCodexWake(layout, false); err != nil {
		t.Fatal(err)
	}
	client, _ := integrate.ByName(integrate.Codex)
	if err := client.Install(layout, nil); err != nil {
		t.Fatal(err)
	}
	lines := wakeReport(layout, filepath.Join(home, "hub.token"), time.Now())
	if len(lines) != 1 || !strings.Contains(lines[0], "idle wake explicitly disabled") {
		t.Fatalf("doctor disagrees with the preserved MCP opt-out: %v", lines)
	}
}

func TestDoctorWakeLabelsClientsAndOrdersTheirDiagnostics(t *testing.T) {
	config, inspect := wakeConfiguration, inspectWakes
	t.Cleanup(func() { wakeConfiguration, inspectWakes = config, inspect })
	wakeConfiguration = func(integrate.Layout) (bool, bool, error) { return true, true, nil }
	now := time.Unix(1800000000, 0)
	inspectWakes = func(string) ([]wake.Inspection, error) {
		return []wake.Inspection{
			{Path: "antigravity", Status: "usable", Client: "antigravity", Kind: "queue", Outcome: "unknown", Outstanding: true, StartedMS: now.UnixMilli()},
			{Path: "unknown", Status: "usable", Client: "unrecognized", Kind: "queue", Outcome: "unknown", Outstanding: true, StartedMS: now.UnixMilli()},
			{Path: "codex", Status: "usable", Client: "codex", Kind: "queue", Outstanding: true, StartedMS: now.UnixMilli()},
			{Path: "old", Status: "unusable"},
		}, nil
	}
	lines := wakeReport(integrate.Layout{}, "token", now)
	if len(lines) != 6 {
		t.Fatalf("wake report lines: %v", lines)
	}
	for i, prefix := range []string{
		"codex wake: configured tier:",
		`codex wake: guard "codex":`,
		"codex wake: Configured idle wake",
		`antigravity wake: guard "antigravity": outstanding queue wake (unknown), age 0s; retained`,
		`wake guard: "old": unusable; client not recorded; submissions remain blocked.`,
		`wake guard: "unknown": outstanding queue wake (unknown), age 0s; client not recognized; retained`,
	} {
		if !strings.HasPrefix(lines[i], prefix) {
			t.Fatalf("line %d = %q, want prefix %q; all lines: %v", i, lines[i], prefix, lines)
		}
	}
	for _, want := range []string{"Continue that agy conversation", "notification never reached it", "conversation is permanently closed and its wake helper has stopped"} {
		if !strings.Contains(lines[3], want) {
			t.Fatalf("Antigravity recovery lacks %q: %s", want, lines[3])
		}
	}
	if !strings.Contains(lines[4], neutralWakeRecovery) || !strings.Contains(lines[4], "submissions remain blocked") || strings.Contains(lines[4], "queue entries") {
		t.Fatalf("an unusable guard blocks submissions whichever client wrote it: %s", lines[4])
	}
	if !strings.Contains(lines[5], neutralWakeRecovery) || strings.Contains(lines[5], "submissions remain blocked") || strings.Contains(lines[5], wakeRecovery) {
		t.Fatalf("a guard of an unrecognized client got a recovery it cannot support: %s", lines[5])
	}
	if !strings.Contains(lines[1], wakeRecovery) {
		t.Fatalf("Codex recovery changed: %s", lines[1])
	}
	if text := (Result{Wake: lines}).Format(); !strings.Contains(text, strings.Join(lines, "\n")+"\n") {
		t.Fatalf("doctor changed complete wake lines: %s", text)
	}
	wakeConfiguration = func(integrate.Layout) (bool, bool, error) { return true, false, nil }
	for _, line := range wakeReport(integrate.Layout{}, "token", now) {
		if strings.Contains(line, wakeLimits) {
			t.Fatalf("disabled Codex wake printed its limits: %s", line)
		}
	}
}

func TestDoctorWakeDiagnosticsNeverEnterTheHubReport(t *testing.T) {
	layout := doctorLayout(t)
	token := filepath.Join(layout.Home, "hub.token")
	if err := os.WriteFile(token, []byte("secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	guard := hook.NewSessions(token).WakePath("codex", "session")
	if err := os.MkdirAll(filepath.Dir(guard), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(guard, []byte(`{"version":1,"rev":0,"attempts":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	var stored []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/ui/doctor" {
			stored, _ = io.ReadAll(r.Body)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	res, err := Run(context.Background(), Options{Layout: layout, HubURL: srv.URL, TokenFile: token, HTTP: srv.Client()})
	if err != nil || !res.Stored {
		t.Fatalf("doctor did not store its report: %+v, %v", res, err)
	}
	if len(res.Wake) != 2 || !strings.Contains(res.Wake[1], "client not recorded") {
		t.Fatalf("local guard diagnostic missing: %v", res.Wake)
	}
	want, err := json.Marshal(res.Report)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stored, want) || bytes.Contains(stored, []byte("client not recorded")) || bytes.Contains(stored, []byte(guard)) {
		t.Fatalf("hub report differs from Report or contains local wake diagnostics: %s", stored)
	}
}

func TestDoctorWakeReportsDeletedAntigravityExecutableWithRestartAdvice(t *testing.T) {
	home := t.TempDir()
	token := filepath.Join(home, "hub.token")
	layout := integrate.Layout{Home: home, Getenv: func(string) string { return "" }}
	guard := hook.NewSessions(token).WakePath("antigravity", "e28a6164-6409-4810-b8ee-df8202a16a0a")
	if err := os.MkdirAll(filepath.Dir(guard), 0o700); err != nil {
		t.Fatal(err)
	}
	const wantReason = "the running agy executable was deleted, commonly by an in-place update; restart agy if wake stays unavailable"
	body := []byte(`{"version":1,"rev":1,"client":"antigravity","binary":{"path":"/home/user/.local/bin/agy.1795283135508816615.old (deleted)","checked_ms":1000},"last_failure":{"at_ms":2000,"detail":"` + wantReason + `"},"attempts":[]}`)
	if err := os.WriteFile(guard, body, 0o600); err != nil {
		t.Fatal(err)
	}
	lines := wakeReport(layout, token, time.Unix(1800000000, 0))
	text := strings.Join(lines, "\n")
	if !strings.Contains(text, `antigravity wake: guard`) || !strings.Contains(text, ": "+wantReason) {
		t.Fatalf("doctor did not render deleted-exe Antigravity wake guidance:\n%s", text)
	}
	if strings.Contains(text, "Unrecognized wake failure") {
		t.Fatalf("deleted-exe failure fell back to unrecognized:\n%s", text)
	}
}
