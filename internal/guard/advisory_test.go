package guard

import (
	"strings"
	"testing"

	"github.com/mateusz-klatt/iris/internal/hubclient"
)

func TestAdvisoryTextNamesTheOverlapAndUnknownLocation(t *testing.T) {
	if AdvisoryText(nil) != "" {
		t.Fatal(AdvisoryText(nil))
	}
	one := AdvisoryText([]hubclient.Advisory{{Path: "a.go", Holder: "codex-host-wt", Worktree: "/repo/wt", Branch: "feature"}})
	if !strings.Contains(one, "Iris: a.go is also auto-claimed by codex-host-wt in worktree /repo/wt on branch feature.") || !strings.Contains(one, "this edit is allowed") {
		t.Fatal(one)
	}
	two := AdvisoryText([]hubclient.Advisory{{Path: "a.go", Holder: "one"}, {Path: "b.go", Holder: "two", Worktree: "/wt", Branch: "main"}})
	if !strings.Contains(two, "worktree unknown") || !strings.Contains(two, "branch unknown") || !strings.Contains(two, "Iris: b.go") || strings.Count(two, "this edit is allowed") != 1 {
		t.Fatal(two)
	}
}
