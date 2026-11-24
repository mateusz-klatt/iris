package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/buildinfo"
	"github.com/mateusz-klatt/iris/internal/doctor"
	"github.com/mateusz-klatt/iris/internal/hook"
	"github.com/mateusz-klatt/iris/internal/integrate"
)

// All client binaries and the hub are local stand-ins. Only the injected runner
// supplies live results, so these command tests cannot start a billable session.
func TestDoctorLiveExitReflectsLiveResults(t *testing.T) {
	binDir := t.TempDir()
	for _, name := range []string{"iris", "grok", "codex"} {
		if _, err := integrate.PlaceFake(binDir, name); err != nil {
			t.Fatal(err)
		}
	}
	home := t.TempDir()
	for _, key := range []string{"HOME", "USERPROFILE", "AppData", "LOCALAPPDATA", "XDG_CONFIG_HOME", "XDG_STATE_HOME"} {
		t.Setenv(key, home)
	}
	for _, key := range []string{"CODEX_HOME", "COPILOT_HOME", "KIMI_CODE_HOME", "IRIS_STATE_DIR"} {
		t.Setenv(key, filepath.Join(home, key))
	}
	t.Setenv("GROK_HOME", filepath.Join(home, ".grok"))
	t.Setenv("PATH", binDir)
	t.Setenv("IRIS_FAKE_STDOUT", "{}")
	t.Setenv("IRIS_FAKE_EXIT", "0")
	t.Setenv("IRIS_FAKE_NO_RECEIPT", "0")
	grok, _ := integrate.ByName(integrate.Grok)
	if err := grok.Install(integrate.Layout{}, nil); err != nil {
		t.Fatal(err)
	}
	codex, _ := integrate.ByName(integrate.Codex)
	if err := codex.Install(integrate.Layout{}, nil); err != nil {
		t.Fatal(err)
	}
	token := filepath.Join(home, "hub.token")
	if err := os.WriteFile(token, []byte("test-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("IRIS_TOKEN_FILE", token)
	for _, tc := range []struct {
		name, client                        string
		live, grades, codexSlow, pass       bool
		hubUp, tokenOK                      bool
		damagedMarker, incompleteMarkerScan bool
		want                                int
	}{
		{name: "selected live pass despite unrelated grades", live: true, client: integrate.Grok, grades: true, pass: true, hubUp: true, tokenOK: true},
		{name: "all selected live clients pass despite grades", live: true, grades: true, pass: true, hubUp: true, tokenOK: true},
		{name: "explicit needs human with slow history warns of original step", live: true, client: integrate.Codex, codexSlow: true, pass: true, hubUp: true, tokenOK: true},
		{name: "explicit needs human with silent history warns of original step", live: true, client: integrate.Codex, grades: true, codexSlow: true, pass: true, hubUp: true, tokenOK: true},
		{name: "live failure with healthy offline grades", live: true, client: integrate.Grok, hubUp: true, tokenOK: true, want: 1},
		{name: "hub failure despite live pass", live: true, client: integrate.Grok, pass: true, tokenOK: true, want: 1},
		{name: "token failure despite live pass", live: true, client: integrate.Grok, pass: true, hubUp: true, want: 1},
		{name: "current marker damage despite live pass", live: true, client: integrate.Grok, pass: true, hubUp: true, tokenOK: true, damagedMarker: true, want: 1},
		{name: "incomplete marker scan despite live pass", live: true, client: integrate.Grok, pass: true, hubUp: true, tokenOK: true, incompleteMarkerScan: true, want: 1},
		{name: "plain doctor still fails unrelated grades", grades: true, hubUp: true, tokenOK: true, want: 1},
		{name: "plain doctor still passes healthy grades", hubUp: true, tokenOK: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sessionsDir := hook.NewSessions(token).Dir()
			if err := os.RemoveAll(sessionsDir); err != nil {
				t.Fatal(err)
			}
			if tc.damagedMarker {
				if err := os.Mkdir(sessionsDir, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(sessionsDir, "damaged.ended"), []byte("broken\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if tc.incompleteMarkerScan {
				if err := os.WriteFile(sessionsDir, []byte("not a directory"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			var log string
			if tc.grades {
				when := time.Now().UTC().Format(time.RFC3339)
				log = fmt.Sprintf(`{"time":%q,"client":"codex","event":"PreToolUse","stage":"touch","error":"test failure"}`+"\n", when)
				log += strings.Repeat(fmt.Sprintf(`{"time":%q,"client":"copilot","event":"PostToolUse","stage":"shell status","error":"shell reconciliation passed its deadline"}`+"\n", when), 11)
			}
			if tc.codexSlow {
				log += strings.Repeat(fmt.Sprintf(`{"time":%q,"client":"codex","event":"PostToolUse","stage":"shell status","error":"shell reconciliation passed its deadline"}`+"\n", time.Now().UTC().Format(time.RFC3339)), 11)
			}
			if err := os.WriteFile(filepath.Join(home, "hook-failures.log"), []byte(log), 0o600); err != nil {
				t.Fatal(err)
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case !tc.hubUp:
					w.WriteHeader(http.StatusServiceUnavailable)
				case r.URL.Path == "/v1/agents" && !tc.tokenOK:
					w.WriteHeader(http.StatusUnauthorized)
				default:
					_, _ = w.Write([]byte(`{}`))
				}
			}))
			defer srv.Close()
			args := []string{"--hub-url", srv.URL}
			if tc.live {
				args = append(args, "--live", "--yes")
			}
			if tc.client != "" {
				args = append(args, "--client", tc.client)
			}
			ran := 0
			live := func(_ context.Context, _ doctor.LiveOptions, p doctor.LivePlan) doctor.LiveResult {
				ran++
				wantClient := tc.client
				if wantClient == "" {
					wantClient = integrate.Grok
				}
				if p.Client != wantClient || filepath.Dir(p.Binary) != binDir {
					t.Fatalf("unexpected live client: %+v", p)
				}
				return doctor.LiveResult{LivePlan: p, HookFired: true, ClaimReached: true, ToolsAnswered: true, FileEdited: tc.pass}
			}
			var out, errOut bytes.Buffer
			if code := runDoctorIO(context.Background(), args, strings.NewReader(""), &out, &errOut, live); code != tc.want {
				t.Fatalf("exit %d, want %d\n%s\n%s", code, tc.want, out.String(), errOut.String())
			}
			wantRan := 0
			if tc.live {
				wantRan = 1
			}
			if ran != wantRan || !strings.Contains(out.String(), "grok\tok\t") {
				t.Fatalf("ran %d, want %d\n%s\n%s", ran, wantRan, out.String(), errOut.String())
			}
			if tc.grades && (!strings.Contains(out.String(), "codex\tsilent\t") || !strings.Contains(out.String(), "copilot\tslow\t")) {
				t.Fatalf("offline grades missing:\n%s", out.String())
			}
			if tc.codexSlow && !tc.grades && !strings.Contains(out.String(), "codex\tslow\t") {
				t.Fatalf("slow history missing:\n%s", out.String())
			}
			warning := "codex: doctor could not confirm this step; the live run depends on it: Review hooks once in /hooks"
			if warned := strings.Contains(errOut.String(), warning); warned != (tc.client == integrate.Codex) {
				t.Fatalf("original unconfirmed readiness step warning = %v:\n%s", warned, errOut.String())
			}
			if explained := strings.Contains(out.String(), "exit status reflects live results and hub/token/end-marker health; offline client grades are reported separately"); explained != tc.live {
				t.Fatalf("live exit semantics explained = %v, live = %v:\n%s", explained, tc.live, out.String())
			}
		})
	}
}

func TestLiveStageAsksBeforeSpending(t *testing.T) {
	plans := []doctor.LivePlan{{Client: "claude", Binary: "/bin/claude", Model: "haiku"}, {Client: "codex", Binary: "/bin/codex", Model: "gpt-6.1-terra", Requires: "Review hooks once in /hooks"}}
	var ran []string
	var limits []time.Duration
	live := func(_ context.Context, opt doctor.LiveOptions, p doctor.LivePlan) doctor.LiveResult {
		ran = append(ran, p.Client)
		limits = append(limits, opt.Timeout)
		ok := p.Client == "claude"
		return doctor.LiveResult{LivePlan: p, HookFired: ok, ClaimReached: ok, ToolsAnswered: ok, FileEdited: ok}
	}
	for _, answer := range []string{"", "n\n", "maybe\n"} {
		var out, errOut bytes.Buffer
		if code := liveStage(context.Background(), plans, []string{"codex: Review hooks once in /hooks"}, false, 0, strings.NewReader(answer), &out, &errOut, live); code != 1 {
			t.Fatalf("%q: code %d", answer, code)
		}
		if len(ran) != 0 || !strings.Contains(errOut.String(), "claude (/bin/claude), model haiku") || !strings.Contains(errOut.String(), "nothing was run") || !strings.Contains(errOut.String(), "skips codex: Review hooks once in /hooks") {
			t.Fatalf("%q: ran %v\n%s", answer, ran, errOut.String())
		}
		if !strings.Contains(errOut.String(), "codex: doctor could not confirm this step; the live run depends on it: Review hooks once in /hooks") {
			t.Fatalf("the unconfirmed step must be shown before consent: %s", errOut.String())
		}
	}
	var out, errOut bytes.Buffer
	if code := liveStage(context.Background(), plans, nil, false, time.Minute, strings.NewReader("Yes\n"), &out, &errOut, live); code != 1 {
		t.Fatalf("a failed client must fail the run: %d", code)
	}
	if strings.Join(ran, ",") != "claude,codex" || limits[0] != time.Minute {
		t.Fatalf("ran %v limits %v", ran, limits)
	}
	if !strings.Contains(out.String(), "claude (/bin/claude, model haiku): ok") || !strings.Contains(out.String(), "codex (/bin/codex, model gpt-6.1-terra): failed") {
		t.Fatal(out.String())
	}
	ran = nil
	out.Reset()
	errOut.Reset()
	if code := liveStage(context.Background(), plans[:1], nil, true, 0, strings.NewReader(""), &out, &errOut, live); code != 0 {
		t.Fatalf("code %d\n%s", code, errOut.String())
	}
	if strings.Contains(errOut.String(), "Proceed?") || len(ran) != 1 {
		t.Fatalf("--yes still asked: %s", errOut.String())
	}
}

func TestDoctorWithoutLiveNeverOffersToSpend(t *testing.T) {
	var out, errOut bytes.Buffer
	never := func(context.Context, doctor.LiveOptions, doctor.LivePlan) doctor.LiveResult {
		t.Fatal("live ran")
		return doctor.LiveResult{}
	}
	if code := runDoctorIO(context.Background(), []string{"--hub-url", "http://127.0.0.1:1"}, strings.NewReader("y\n"), &out, &errOut, never); code != 1 {
		t.Fatalf("an unreachable hub must fail: %d", code)
	}
	if !strings.Contains(out.String(), "hub: unreachable") || strings.Contains(errOut.String(), "charge") {
		t.Fatalf("out %q err %q", out.String(), errOut.String())
	}
}

// --bundle prints the bundle as JSON instead of the report, keeps the report's exit status,
// names no hub, and cannot be combined with --live, which spends.
func TestDoctorBundlePrintsJSONAndNoHub(t *testing.T) {
	never := func(context.Context, doctor.LiveOptions, doctor.LivePlan) doctor.LiveResult {
		t.Fatal("live ran")
		return doctor.LiveResult{}
	}
	var out, errOut bytes.Buffer
	if code := runDoctorIO(context.Background(), []string{"--bundle", "--hub-url", "http://127.0.0.1:1"}, strings.NewReader(""), &out, &errOut, never); code != 1 {
		t.Fatalf("an unreachable hub must still fail: %d", code)
	}
	var got doctor.Bundle
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("not a bundle: %v\n%s", err, out.String())
	}
	if got.Hub.Source != "command line" || got.Hub.Up || got.Iris.Version != buildinfo.String() || strings.Contains(out.String(), "127.0.0.1") {
		t.Fatalf("%s", out.String())
	}
	out.Reset()
	if code := runDoctorIO(context.Background(), []string{"--bundle", "--live"}, strings.NewReader(""), &out, &errOut, never); code != 2 {
		t.Fatalf("--bundle with --live: %d", code)
	}
}

func TestDoctorShowsLocalGenerationRecoveryAndKeepsBundlePathsPrivate(t *testing.T) {
	home := t.TempDir()
	for _, key := range []string{"HOME", "USERPROFILE", "AppData", "LOCALAPPDATA", "XDG_CONFIG_HOME", "XDG_STATE_HOME", "PATH"} {
		t.Setenv(key, home)
	}
	for _, key := range []string{"CODEX_HOME", "COPILOT_HOME", "GROK_HOME", "KIMI_CODE_HOME", "IRIS_STATE_DIR"} {
		t.Setenv(key, filepath.Join(home, key))
	}
	token := filepath.Join(home, "hub.token")
	if err := os.WriteFile(token, []byte("test-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("IRIS_TOKEN_FILE", token)
	sessions := hook.NewSessions(token)
	if err := os.Mkdir(sessions.Dir(), 0o700); err != nil {
		t.Fatal(err)
	}

	const client = "claude"
	cases := []struct {
		session, content, suffix, category string
	}{
		{"ended", "2\n", ".ended", hook.EndedGenerationRefused},
		{"live", "3\n", "", hook.BindingGenerationRefused},
		{"damaged", "invalid\n", ".ended", hook.DamagedGenerationProof},
		{"floor", "4\n", ".ended", hook.RestoreFloorRefused},
	}
	paths := make(map[string]string, len(cases))
	for _, tc := range cases {
		sum := sha256.Sum256([]byte(client + "\x00" + tc.session))
		path := filepath.Join(sessions.Dir(), hex.EncodeToString(sum[:16])+tc.suffix)
		paths[tc.category] = path
		if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	sessions.ObserveBindingRefusal(client, "ended", true)
	sessions.ObserveBindingRefusal(client, "live", false)
	hook.Observer(os.Getenv, client)(hook.RestoreFloorRefused + " at " + paths[hook.RestoreFloorRefused])
	if _, _, err := sessions.RecallBindingGeneration(client, "damaged"); !errors.Is(err, hook.ErrInvalidGeneration) {
		t.Fatalf("damaged marker must prevent binding: %v", err)
	}

	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/ui/doctor" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		_, _ = fmt.Fprint(w, `{}`)
	}))
	defer hub.Close()
	never := func(context.Context, doctor.LiveOptions, doctor.LivePlan) doctor.LiveResult {
		t.Fatal("live ran")
		return doctor.LiveResult{}
	}
	var out, errOut bytes.Buffer
	args := []string{"--hub-url", hub.URL}
	if code := runDoctorIO(t.Context(), args, strings.NewReader(""), &out, &errOut, never); code != 1 {
		t.Fatalf("still-damaged marker should fail doctor: exit %d\n%s\n%s", code, out.String(), errOut.String())
	}
	report := out.String()
	for category, path := range paths {
		if !strings.Contains(report, category+" at "+path) {
			t.Errorf("local report omitted %q at %s:\n%s", category, path, report)
		}
	}
	if !strings.Contains(report, "start a new client session to obtain fresh proof") || !strings.Contains(report, "verify the configured hub and its session history before archiving any named file") {
		t.Fatalf("local report omitted recovery guidance:\n%s", report)
	}
	if !strings.Contains(report, "Verify the configured hub and its restore history") {
		t.Fatalf("local report omitted restore floor recovery guidance:\n%s", report)
	}

	out.Reset()
	if code := runDoctorIO(t.Context(), append(args, "--bundle"), strings.NewReader(""), &out, &errOut, never); code != 1 {
		t.Fatalf("bundle with still-damaged marker exit %d, want 1\n%s\n%s", code, out.String(), errOut.String())
	}
	var bundle doctor.Bundle
	if err := json.Unmarshal(out.Bytes(), &bundle); err != nil {
		t.Fatalf("parse bundle: %v", err)
	}
	for category, path := range paths {
		want := doctor.BundleCount{Client: client, Stage: "observed", Category: category, Count: 1}
		fileID := strings.TrimSuffix(filepath.Base(path), ".ended")
		if !slices.Contains(bundle.Observations, want) || strings.Contains(out.String(), fileID) {
			t.Errorf("bundle must count %q without a local path: %v", category, bundle.Observations)
		}
	}
	if len(bundle.Failures) != 0 {
		t.Fatalf("local observations must not count as hook failures: %v", bundle.Failures)
	}
}
