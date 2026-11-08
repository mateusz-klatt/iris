package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

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
	for _, args := range [][]string{nil, {"nope"}, {"forget"}, {"forget", "--agent", "a", "--repository", "r"}, {"forget", "--bogus"}, {"forget", "--agent", "a", "extra"}, {"restore-check", "only-one"}, {"backup", "only-one"}} {
		if err := runAdmin(args); err == nil || err.Error() != adminUsage {
			t.Errorf("%v: %v", args, err)
		}
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
