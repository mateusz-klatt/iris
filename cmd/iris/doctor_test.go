package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/buildinfo"
	"github.com/mateusz-klatt/iris/internal/doctor"
	"github.com/mateusz-klatt/iris/internal/integrate"
)

// All client binaries and the hub are local stand-ins. Only the injected runner
// supplies live results, so these command tests cannot start a billable session.
func TestDoctorLiveExitReflectsLiveResults(t *testing.T) {
	binDir := t.TempDir()
	for _, name := range []string{"iris", "grok"} {
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
	token := filepath.Join(home, "hub.token")
	if err := os.WriteFile(token, []byte("test-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("IRIS_TOKEN_FILE", token)
	for _, tc := range []struct {
		name                     string
		live, only, grades, pass bool
		hubUp, tokenOK           bool
		want                     int
	}{
		{name: "selected live pass despite unrelated grades", live: true, only: true, grades: true, pass: true, hubUp: true, tokenOK: true},
		{name: "all selected live clients pass despite grades", live: true, grades: true, pass: true, hubUp: true, tokenOK: true},
		{name: "live failure with healthy offline grades", live: true, only: true, hubUp: true, tokenOK: true, want: 1},
		{name: "hub failure despite live pass", live: true, only: true, pass: true, tokenOK: true, want: 1},
		{name: "token failure despite live pass", live: true, only: true, pass: true, hubUp: true, want: 1},
		{name: "plain doctor still fails unrelated grades", grades: true, hubUp: true, tokenOK: true, want: 1},
		{name: "plain doctor still passes healthy grades", hubUp: true, tokenOK: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var log string
			if tc.grades {
				when := time.Now().UTC().Format(time.RFC3339)
				log = fmt.Sprintf(`{"time":%q,"client":"codex","event":"PreToolUse","stage":"touch","error":"test failure"}`+"\n", when)
				log += strings.Repeat(fmt.Sprintf(`{"time":%q,"client":"copilot","event":"PostToolUse","stage":"shell status","error":"shell reconciliation passed its deadline"}`+"\n", when), 11)
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
			if tc.only {
				args = append(args, "--client", integrate.Grok)
			}
			ran := 0
			live := func(_ context.Context, _ doctor.LiveOptions, p doctor.LivePlan) doctor.LiveResult {
				ran++
				if p.Client != integrate.Grok || filepath.Dir(p.Binary) != binDir {
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
			if explained := strings.Contains(out.String(), "exit status reflects live results and hub/token health; offline client grades are reported separately"); explained != tc.live {
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
	}
	var out, errOut bytes.Buffer
	if code := liveStage(context.Background(), plans, nil, false, time.Minute, strings.NewReader("Yes\n"), &out, &errOut, live); code != 1 {
		t.Fatalf("a failed client must fail the run: %d", code)
	}
	if strings.Join(ran, ",") != "claude,codex" || limits[0] != time.Minute {
		t.Fatalf("ran %v limits %v", ran, limits)
	}
	if !strings.Contains(out.String(), "claude (/bin/claude, model haiku): ok") || !strings.Contains(out.String(), "codex (/bin/codex, model gpt-6.1-terra): failed") || !strings.Contains(errOut.String(), "its hook runs only if you have done this: Review hooks once in /hooks") {
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
