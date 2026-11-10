package doctor

import (
	"strings"
	"testing"
)

// Every shell scope line is printed under its own label, and none is printed when there are
// none.
func TestDoctorPrintsShellScopeLines(t *testing.T) {
	out := Result{Scope: []string{"/w: reduced until then", "limits: what it cannot do"}}.Format()
	if !strings.Contains(out, "shell scope: /w: reduced until then\nshell scope: limits: what it cannot do\n") {
		t.Fatalf("not printed:\n%s", out)
	}
	if strings.Contains(Result{}.Format(), "shell scope") {
		t.Fatal("an empty scope is printed")
	}
}
