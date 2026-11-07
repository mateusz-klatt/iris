package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
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
	for _, args := range [][]string{nil, {"nope"}, {"forget"}, {"forget", "--agent", "a", "--repository", "r"}, {"forget", "--bogus"}, {"forget", "--agent", "a", "extra"}} {
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
