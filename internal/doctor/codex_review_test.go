package doctor

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/mateusz-klatt/iris/internal/integrate"
)

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
}
