package doctor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/integrate"
)

func TestCodexHistoryPreservesLiveReadiness(t *testing.T) {
	t.Setenv("IRIS_FAKE_STDOUT", "{}")
	t.Setenv("IRIS_FAKE_EXIT", "0")
	for _, installed := range []bool{false, true} {
		t.Run(fmt.Sprintf("installed=%v", installed), func(t *testing.T) {
			layout := doctorLayout(t)
			iris := layout.Iris()
			layout.LookPath = func(name string) (string, error) {
				if name == "iris" || name == "codex" {
					return iris, nil
				}
				return "", os.ErrNotExist
			}
			want := "codex: configuration is absent"
			wantStatus := StatusNotConfigured
			codex, _ := integrate.ByName(integrate.Codex)
			if installed {
				if err := codex.Install(layout, nil); err != nil {
					t.Fatal(err)
				}
				want = "codex: Review hooks once in /hooks"
				wantStatus = StatusNeedsHuman
			}
			initial := checkClient(context.Background(), Options{Layout: layout, VersionTimeout: versionTimeout}, codex)
			if initial.Status != wantStatus || initial.Detail != strings.TrimPrefix(want, "codex: ") {
				t.Fatalf("fixture must reach its unready state before history: %+v", initial)
			}
			now := time.Now().UTC()
			deadline := fmt.Sprintf(`{"time":%q,"client":"codex","event":"post_tool","stage":"shell status","error":"shell reconciliation passed its deadline"}`+"\n", now.Format(time.RFC3339))
			token := filepath.Join(layout.Home, "hub.token")
			if err := os.WriteFile(filepath.Join(layout.Home, "hook-failures.log"), []byte(strings.Repeat(deadline, slowLimit+1)), 0o600); err != nil {
				t.Fatal(err)
			}
			res, err := Run(context.Background(), Options{Layout: layout, HubURL: "http://127.0.0.1:1", TokenFile: token, Now: func() time.Time { return now }})
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(res.Format(), "codex\tslow\t") {
				t.Fatalf("historical slow grade missing:\n%s", res.Format())
			}
			for _, only := range []string{"", integrate.Codex} {
				plans, skipped := LivePlans(layout, res.Report.Clients, only)
				if installed && only != "" {
					if len(plans) != 1 || plans[0].Client != integrate.Codex || plans[0].Requires != "Review hooks once in /hooks" || len(skipped) != 0 {
						t.Fatalf("explicit Codex must run despite unconfirmed review: plans %+v, skipped %v", plans, skipped)
					}
					continue
				}
				if len(plans) != 0 || !strings.Contains(strings.Join(skipped, "\n"), want) {
					t.Fatalf("only %q: unready Codex must be skipped: plans %+v, skipped %v", only, plans, skipped)
				}
			}
		})
	}
}

// A current canary error is a grade, not evidence that the client's human
// step is complete. It must not authorize an otherwise unready billable run.
func TestCodexCanaryFailurePreservesUnconfirmedReview(t *testing.T) {
	for _, tc := range []struct {
		name, output, exit, noReceipt string
		brokenHooks                   bool
	}{
		{name: "failed command", output: "{}", exit: "1"},
		{name: "invalid output", output: "not JSON", exit: "0"},
		{name: "missing receipt", output: "{}", exit: "0", noReceipt: "1"},
		{name: "broken hooks configuration", output: "{}", exit: "0", brokenHooks: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("IRIS_FAKE_STDOUT", tc.output)
			t.Setenv("IRIS_FAKE_EXIT", tc.exit)
			t.Setenv("IRIS_FAKE_NO_RECEIPT", tc.noReceipt)
			layout := doctorLayout(t)
			iris := layout.Iris()
			layout.LookPath = func(name string) (string, error) {
				if name == "iris" || name == "codex" {
					return iris, nil
				}
				return "", os.ErrNotExist
			}
			codex, _ := integrate.ByName(integrate.Codex)
			if err := codex.Install(layout, nil); err != nil {
				t.Fatal(err)
			}
			if tc.brokenHooks {
				files, _ := codex.Files(layout)
				if err := os.WriteFile(files[0], []byte("{broken"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if step, _ := codex.(integrate.Reviewer).Review(layout); step != "Review hooks once in /hooks" {
				t.Fatalf("fixture must require a human step: %q", step)
			}
			for _, history := range []bool{false, true} {
				now := time.Now().UTC()
				var log string
				if history {
					deadline := fmt.Sprintf(`{"time":%q,"client":"codex","event":"post_tool","stage":"shell status","error":"shell reconciliation passed its deadline"}`+"\n", now.Format(time.RFC3339))
					log = strings.Repeat(deadline, slowLimit+1)
				}
				if err := os.WriteFile(filepath.Join(layout.Home, "hook-failures.log"), []byte(log), 0o600); err != nil {
					t.Fatal(err)
				}
				res, err := Run(context.Background(), Options{Layout: layout, Self: iris, HubURL: "http://127.0.0.1:1", TokenFile: filepath.Join(layout.Home, "hub.token"), Now: func() time.Time { return now }})
				if err != nil {
					t.Fatal(err)
				}
				wantGrade := StatusSilent
				if history {
					wantGrade = StatusSlow
				}
				if !strings.Contains(res.Format(), "codex\t"+wantGrade+"\t") || res.ExitCode() != 1 {
					t.Fatalf("canary/history grade must still fail plain doctor:\n%s", res.Format())
				}
				if plans, skipped := LivePlans(layout, res.Report.Clients, ""); len(plans) != 0 || !strings.Contains(strings.Join(skipped, "\n"), "codex: Review hooks once in /hooks") {
					t.Fatalf("history %v: a failed canary must not bypass review: plans %+v, skipped %v", history, plans, skipped)
				}
				if plans, skipped := LivePlans(layout, res.Report.Clients, integrate.Codex); len(plans) != 1 || plans[0].Requires != "Review hooks once in /hooks" || len(skipped) != 0 {
					t.Fatalf("history %v: explicit run must retain the unconfirmed step: plans %+v, skipped %v", history, plans, skipped)
				}
			}
		})
	}
}

// Once Codex has recorded a review of every Iris hook, the row is ok and says
// what was not verified; before that it asks for the review.
func TestCodexRowFollowsTheRecordedHookReview(t *testing.T) {
	t.Setenv("IRIS_FAKE_STDOUT", "{}")
	t.Setenv("IRIS_FAKE_EXIT", "0")
	layout := doctorLayout(t)
	iris, _ := layout.LookPath("iris")
	layout.LookPath = func(name string) (string, error) {
		if name == "iris" || name == "codex" {
			return iris, nil
		}
		return "", os.ErrNotExist
	}
	codex, _ := integrate.ByName(integrate.Codex)
	if err := codex.Install(layout, nil); err != nil {
		t.Fatal(err)
	}
	row := func() ClientReport {
		res, err := Run(context.Background(), Options{Layout: layout, HubURL: "http://127.0.0.1:1"})
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range res.Report.Clients {
			if r.Client == integrate.Codex {
				return r
			}
		}
		t.Fatal("no codex row")
		return ClientReport{}
	}
	if r := row(); r.Status != StatusNeedsHuman || r.Detail != "Review hooks once in /hooks" {
		t.Fatalf("%+v", r)
	}
	files, _ := codex.Files(layout)
	raw, _ := os.ReadFile(files[1])
	for _, e := range []string{"session_start", "user_prompt_submit", "pre_tool_use", "post_tool_use", "stop", "session_end"} {
		// %q escapes a path's backslashes and quotes as a TOML basic string does.
		raw = append(raw, fmt.Sprintf("\n[hooks.state.%q]\ntrusted_hash = \"sha256:x\"\n", files[0]+":"+e+":0:0")...)
	}
	if err := os.WriteFile(files[1], raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if r := row(); r.Status != StatusOK || !strings.HasPrefix(r.Detail, "hook review recorded; hash not verified") {
		t.Fatalf("%+v", r)
	}
	// With review recorded, a current canary failure may still be checked live.
	t.Setenv("IRIS_FAKE_EXIT", "1")
	r := row()
	if r.Status != StatusSilent {
		t.Fatalf("the current failure must remain visible: %+v", r)
	}
	if plans, skipped := LivePlans(layout, []ClientReport{r}, ""); len(plans) != 1 || plans[0].Requires != "" || len(skipped) != 0 {
		t.Fatalf("a reviewed client may still be checked live: plans %+v, skipped %v", plans, skipped)
	}
}
