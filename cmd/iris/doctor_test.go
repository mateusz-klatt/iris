package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/doctor"
)

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
	if got.Hub.Source != "command line" || got.Hub.Up || got.Iris.Version == "" || strings.Contains(out.String(), "127.0.0.1") {
		t.Fatalf("%s", out.String())
	}
	out.Reset()
	if code := runDoctorIO(context.Background(), []string{"--bundle", "--live"}, strings.NewReader(""), &out, &errOut, never); code != 2 {
		t.Fatalf("--bundle with --live: %d", code)
	}
}
