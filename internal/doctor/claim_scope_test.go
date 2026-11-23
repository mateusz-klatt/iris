package doctor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// iris doctor says which claim.auto_scope is in force, and what worktree cannot do.
// A hub that omits the field is unknown, rather than either value (ADR 037).
func TestDoctorReportsTheClaimAutoScope(t *testing.T) {
	ctx := context.Background()
	repo := gitRepoFolding(t, "false")
	hub := func(status int, body string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/v1/guard/check" || r.Method != http.MethodPost {
				t.Errorf("asked %s %s", r.Method, r.URL.Path)
			}
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		}))
	}
	for _, tc := range []struct {
		name, body, want string
		status           int
	}{
		{"repository", `{"message":"","conflicts":[],"claim_auto_scope":"repository"}`, "repository. Automatic claims apply", http.StatusOK},
		{"worktree", `{"claim_auto_scope":"worktree"}`, "worktree. Cross-worktree automatic overlaps are advisory", http.StatusOK},
		{"a value doctor does not know", `{"claim_auto_scope":"seat"}`, "unknown.", http.StatusOK},
		{"a hub from before the field", `{"message":"","conflicts":[],"case_policy":"folded"}`, "unknown.", http.StatusOK},
		{"a hub that fails", `{"claim_auto_scope":"worktree"}`, "unknown.", http.StatusInternalServerError},
		{"a body that is not the answer", `{`, "unknown.", http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := hub(tc.status, tc.body)
			defer srv.Close()
			got := claimScopeLine(ctx, srv.URL, "t", "this-host", repo, srv.Client())
			if !strings.HasPrefix(got, tc.want) {
				t.Fatalf("%q", got)
			}
			if tc.want == "worktree. Cross-worktree automatic overlaps are advisory" {
				for _, phrase := range []string{"Explicit claims stay repository-wide", "not enforcement", "two hosts", "rebase or merge", "unrelated agent's"} {
					if !strings.Contains(got, phrase) {
						t.Fatalf("the worktree line omits %q:\n%s", phrase, got)
					}
				}
			}
			if strings.HasPrefix(got, "unknown.") && (strings.Contains(got, "assume repository or worktree.") == false) {
				t.Fatalf("unknown assumed a value: %q", got)
			}
		})
	}
	if got := claimScopeLine(ctx, "http://127.0.0.1:9", "t", "h", repo, http.DefaultClient); !strings.HasPrefix(got, "unknown.") {
		t.Fatalf("an unreachable hub was %q", got)
	}
	if got := claimScopeLine(ctx, "http://127.0.0.1:9", "t", "h", t.TempDir(), http.DefaultClient); got != "" {
		t.Fatalf("a directory outside git reported a scope: %q", got)
	}
	srv := hub(http.StatusOK, `{"claim_auto_scope":"repository"}`)
	defer srv.Close()
	if got := claimScopeLine(ctx, srv.URL, "t", "h", "", srv.Client()); !strings.HasPrefix(got, "repository.") {
		t.Fatalf("the working directory was %q", got)
	}
	shown := (Result{ClaimScope: claimScopeText("worktree")}).Format()
	if !strings.Contains(shown, "claim auto scope: worktree. Cross-worktree automatic overlaps are advisory") || !strings.Contains(shown, "not enforcement") {
		t.Fatal(shown)
	}
	if strings.Contains((Result{}).Format(), "claim auto scope") {
		t.Fatal("an empty result reported a scope")
	}
}
