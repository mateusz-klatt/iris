package doctor

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// gitRepoFolding is a fresh repository with core.ignorecase set as given.
func gitRepoFolding(t *testing.T, folds string) string {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	dir := t.TempDir()
	for _, args := range [][]string{{"init", dir}, {"-C", dir, "config", "core.ignorecase", folds}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	return dir
}

// ADR 003 promises that doctor warns about a repository left case-sensitive that a host which
// folds case works in: there, a claim on a.go would not stop an edit of A.GO (gap review C4).
func TestDoctorWarnsOfASensitiveRepositoryOnAFoldingCheckout(t *testing.T) {
	var asked []string
	hub := func(policy string, status int) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var in map[string]any
			_ = json.NewDecoder(r.Body).Decode(&in)
			asked = append(asked, r.URL.Path+" "+in["host"].(string))
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"message":"","conflicts":[],"case_policy":"` + policy + `"}`))
		}))
	}
	ctx := context.Background()
	folding := gitRepoFolding(t, "true")
	for _, tc := range []struct {
		name, dir, policy string
		status            int
		warn              bool
	}{
		{"a sensitive repository on a folding checkout", folding, "sensitive", http.StatusOK, true},
		{"a folded repository", folding, "folded", http.StatusOK, false},
		{"a hub that cannot say", folding, "sensitive", http.StatusInternalServerError, false},
		{"a checkout that does not fold", gitRepoFolding(t, "false"), "sensitive", http.StatusOK, false},
		{"no repository at all", t.TempDir(), "sensitive", http.StatusOK, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			asked = nil
			srv := hub(tc.policy, tc.status)
			defer srv.Close()
			got := caseLine(ctx, srv.URL, "t", "this-host", tc.dir, srv.Client())
			if (got != "") != tc.warn || (tc.warn && !strings.Contains(got, "A.GO")) {
				t.Fatalf("%q, want a warning: %v", got, tc.warn)
			}
			// Only a folding checkout needs the hub's answer.
			if folds := tc.dir == folding; folds != (len(asked) == 1) || (folds && asked[0] != "/v1/guard/check this-host") {
				t.Fatalf("asked %v", asked)
			}
		})
	}
	if got := caseLine(ctx, "http://127.0.0.1:9", "t", "h", folding, http.DefaultClient); got != "" {
		t.Fatalf("an unreachable hub warned: %q", got)
	}
	if out := (Result{Case: "the warning"}).Format(); !strings.Contains(out, "case policy: the warning\n") {
		t.Fatal(out)
	}
}
