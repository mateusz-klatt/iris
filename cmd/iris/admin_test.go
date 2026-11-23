package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/mateusz-klatt/iris/internal/store"
)

func TestAdminForgetSendsTheRequestAndPrintsTheReport(t *testing.T) {
	var got map[string]any
	client := tokenClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/admin/forget" || r.Method != "POST" {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"dry_run":true,"counts":{"Agent":1}}`))
	})
	var out bytes.Buffer
	if err := adminCommand(context.Background(), client, []string{"forget", "--agent", "probe", "--dry-run"}, &out); err != nil {
		t.Fatal(err)
	}
	if got["agent"] != "probe" || got["dry_run"] != true || got["repository_id"] != "" {
		t.Fatal(got)
	}
	if !strings.Contains(out.String(), `"Agent": 1`) {
		t.Fatal(out.String())
	}
}

func TestAdminForgetReportsARefusal(t *testing.T) {
	client := tokenClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"code":"forbidden","message":"only the hub's own token may do this"}}`))
	})
	var out bytes.Buffer
	err := adminCommand(context.Background(), client, []string{"forget", "--repository", "rp"}, &out)
	if err == nil || !strings.Contains(err.Error(), "only the hub's own token") || out.Len() != 0 {
		t.Fatal(err, out.String())
	}
}

func TestAdminUsage(t *testing.T) {
	for _, args := range [][]string{nil, {"nope"}, {"forget"}, {"forget", "--agent", "a", "--repository", "r"}, {"forget", "--bogus"}, {"forget", "--agent", "a", "extra"}, {"restore-check", "only-one"}, {"backup", "only-one"}, {"mark-restored"}, {"mark-restored", "a.db", "b.db"}, {"merge"}, {"merge", "--from", "a"}, {"merge", "--into", "b"}, {"merge", "--from", "a", "--into", "a"}, {"merge", "--bogus"}, {"merge", "--from", "a", "--into", "b", "extra"}, {"unalias"}, {"unalias", "--repository", "rp_a"}, {"unalias", "local:x"}, {"unalias", "--bogus"}, {"policy"}, {"policy", "--repository", "rp"}, {"policy", "--claim-auto-scope", "worktree"}, {"policy", "--repository", "rp", "--claim-auto-scope", "seat"}, {"policy", "--repository", "rp", "--claim-auto-scope", "worktree", "extra"}, {"policy", "--bogus"}} {
		if err := runAdmin(args); err == nil || err.Error() != adminUsage {
			t.Errorf("%v: %v", args, err)
		}
	}
}

func TestAdminMergeSendsThePlanAndApplyRequests(t *testing.T) {
	var got []map[string]any
	var paths []string
	client := tokenClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		paths = append(paths, r.URL.Path)
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		got = append(got, body)
		_ = json.NewEncoder(w).Encode(map[string]any{"dry_run": body["dry_run"], "counts": map[string]int{"Message": 3}})
	})
	for _, dry := range []bool{true, false} {
		args := []string{"merge", "--from", "rp_split", "--into", "rp_canonical"}
		if dry {
			args = append(args, "--dry-run")
		}
		if !dry {
			// The folder-project repair has a route of its own (ADR 034).
			args = append(args, "--outside")
		}
		var out bytes.Buffer
		if err := adminCommand(context.Background(), client, args, &out); err != nil {
			t.Fatal(err)
		}
		var report map[string]any
		if err := json.Unmarshal(out.Bytes(), &report); err != nil || report["dry_run"] != dry || report["counts"].(map[string]any)["Message"] != float64(3) {
			t.Fatalf("%v: %s", err, out.String())
		}
	}
	if len(got) != 2 || got[0]["from_repository_id"] != "rp_split" || got[0]["into_repository_id"] != "rp_canonical" || got[0]["dry_run"] != true || got[1]["dry_run"] != false {
		t.Fatal(got)
	}
	if len(paths) != 2 || paths[0] != "/v1/admin/merge" || paths[1] != "/v1/admin/merge/outside" {
		t.Fatal(paths)
	}
}

// unalias sends exact aliases: named on the command line, or one a line from standard input
// after a lone "-", where blank lines are skipped. Input with no alias is a usage error.
func TestAdminUnaliasSendsAliasesFromArgumentsOrStandardInput(t *testing.T) {
	var got []map[string]any
	client := tokenClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/admin/unalias" || r.Method != "POST" {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		got = append(got, body)
		_ = json.NewEncoder(w).Encode(map[string]any{"dry_run": body["dry_run"], "removed": body["aliases"], "absent": []string{}})
	})
	stdin := adminStdin
	t.Cleanup(func() { adminStdin = stdin })
	var out bytes.Buffer
	if err := adminCommand(context.Background(), client, []string{"unalias", "--repository", "rp_a", "--dry-run", "remote:example.com/b", "local:h:/b"}, &out); err != nil {
		t.Fatal(err)
	}
	adminStdin = strings.NewReader("checkout:\"h\":\"/b\":\"\":\"remote:example.com/b\"\n\n  local:h:/c  \n")
	if err := adminCommand(context.Background(), client, []string{"unalias", "--repository", "rp_a", "-"}, &out); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0]["dry_run"] != true || len(got[0]["aliases"].([]any)) != 2 || got[1]["dry_run"] != false {
		t.Fatal(got)
	}
	if aliases := got[1]["aliases"].([]any); len(aliases) != 2 || aliases[0] != `checkout:"h":"/b":"":"remote:example.com/b"` || aliases[1] != "local:h:/c" {
		t.Fatal(aliases)
	}
	adminStdin = strings.NewReader("\n  \n")
	if err := adminCommand(context.Background(), client, []string{"unalias", "--repository", "rp_a", "-"}, &out); err == nil || err.Error() != adminUsage {
		t.Fatal("input with no alias was sent", err)
	}
	adminStdin = iotest.ErrReader(errors.New("stdin failed"))
	if err := adminCommand(context.Background(), client, []string{"unalias", "--repository", "rp_a", "-"}, &out); err == nil || err.Error() != "stdin failed" {
		t.Fatal(err)
	}
}

func TestAdminIsDispatched(t *testing.T) {
	if err := run([]string{"admin"}); err == nil || err.Error() != adminUsage {
		t.Fatal(err)
	}
}

func TestAdminContactSetsAndClears(t *testing.T) {
	var got []map[string]any
	client := tokenClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/admin/contact" {
			t.Errorf("%s", r.URL.Path)
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		got = append(got, body)
		_, _ = w.Write([]byte(`{"repository_id":"rp","contact":""}`))
	})
	var out bytes.Buffer
	for _, args := range [][]string{{"contact", "--repository", "rp", "claude-holzera-iris"}, {"contact", "--repository", "rp", "--clear"}} {
		if err := adminCommand(context.Background(), client, args, &out); err != nil {
			t.Fatal(args, err)
		}
	}
	if got[0]["agent"] != "claude-holzera-iris" || got[1]["agent"] != "" || got[1]["repository_id"] != "rp" {
		t.Fatal(got)
	}
	for _, bad := range [][]string{{"contact"}, {"contact", "--repository", "rp"}, {"contact", "--repository", "rp", "a", "--clear"}, {"contact", "--repository", "rp", "a", "b"}, {"contact", "--clear", "a"}} {
		if err := adminCommand(context.Background(), client, bad, &out); err == nil || err.Error() != adminUsage {
			t.Errorf("%v: %v", bad, err)
		}
	}
}

func TestAdminPolicySendsTheOwnerScope(t *testing.T) {
	var got []map[string]any
	client := tokenClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/admin/policy" || r.Method != "POST" {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		got = append(got, body)
		_ = json.NewEncoder(w).Encode(map[string]any{"repository_id": body["repository_id"], "claim_auto_scope": body["claim_auto_scope"]})
	})
	var out bytes.Buffer
	for _, scope := range []string{"worktree", "repository"} {
		if err := adminCommand(context.Background(), client, []string{"policy", "--repository", "rp_a", "--claim-auto-scope", scope}, &out); err != nil {
			t.Fatal(scope, err)
		}
	}
	if len(got) != 2 || got[0]["repository_id"] != "rp_a" || got[0]["claim_auto_scope"] != "worktree" || got[1]["claim_auto_scope"] != "repository" {
		t.Fatal(got)
	}
	if !strings.Contains(out.String(), `"claim_auto_scope": "worktree"`) || !strings.Contains(out.String(), `"claim_auto_scope": "repository"`) {
		t.Fatal(out.String())
	}
}

// backup runs locally on the hub's database while the hub has it open, and the copy it writes
// is one restore-check accepts.
func TestAdminBackupFeedsRestoreCheck(t *testing.T) {
	src := filepath.Join(t.TempDir(), "hub.db")
	s, err := store.Open(src, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	dest := filepath.Join(t.TempDir(), "backup.db")
	var out bytes.Buffer
	if err := adminCommand(context.Background(), nil, []string{"backup", src, dest}, &out); err != nil || !strings.Contains(out.String(), "integrity ok") {
		t.Fatalf("%v\n%s", err, out.String())
	}
	out.Reset()
	if err := adminCommand(context.Background(), nil, []string{"restore-check", dest, filepath.Join(t.TempDir(), "scratch")}, &out); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
}

// mark-restored runs locally and offline. It marks a copy that a backup did not make, so restore-check
// checks its new stream, and it refuses a database that a hub has open.
func TestAdminMarkRestoredMarksACopyAndRefusesALiveHub(t *testing.T) {
	src := filepath.Join(t.TempDir(), "copy.db")
	s, err := store.Open(src, nil)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := adminCommand(context.Background(), nil, []string{"mark-restored", src}, &out); err == nil || !strings.Contains(err.Error(), "database already owned by another hub") || out.Len() != 0 {
		t.Fatalf("a database a hub has open: %v\n%s", err, out.String())
	}
	_ = s.Close()
	if err := adminCommand(context.Background(), nil, []string{"mark-restored", src}, &out); err != nil || out.String() != "restore marker set in "+src+": the next hub start on it runs on a new stream (ADR 006)\n" {
		t.Fatalf("%v\n%s", err, out.String())
	}
	out.Reset()
	if err := adminCommand(context.Background(), nil, []string{"restore-check", src, filepath.Join(t.TempDir(), "scratch")}, &out); err != nil || !strings.Contains(out.String(), "runs on a new stream") {
		t.Fatalf("%v\n%s", err, out.String())
	}
}

// restore-check runs locally on a copy of a backup; it needs no hub and no token.
func TestAdminRestoreCheckRunsOnACopy(t *testing.T) {
	src := filepath.Join(t.TempDir(), "backup.db")
	s, err := store.Open(src, nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	var out bytes.Buffer
	if err := adminCommand(context.Background(), nil, []string{"restore-check", src, filepath.Join(t.TempDir(), "scratch")}, &out); err != nil || !strings.Contains(out.String(), "ok  ") {
		t.Fatalf("%v\n%s", err, out.String())
	}
}
