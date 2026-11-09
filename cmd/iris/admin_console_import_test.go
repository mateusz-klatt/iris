package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mateusz-klatt/iris/internal/accountimport"
	"github.com/mateusz-klatt/iris/internal/hubclient"
)

func importManifestFile(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "manifest.json")
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal("could not create synthetic manifest")
	}
	return path
}

const cliImportManifest = `{"version":1,"source_id":"synthetic-cli","table":"accounts","timestamp_encoding":"unix_seconds","timestamp_zone":"UTC","renames":{},"locales":{}}`
const cliAssignmentManifest = `{"version":1,"source_id":"synthetic-cli","rows":[{"id":"1","account_row_id":"1","project_id":"project-a","role":"viewer"}],"projects":{"project-a":"rp_synthetic"}}`

func cliImportSource(t *testing.T) (string, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "source.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal("could not open synthetic source")
	}
	defer func() { _ = db.Close() }()
	_, err = db.Exec(`CREATE TABLE accounts(id INTEGER,username TEXT,password_hash TEXT,role TEXT,disabled INTEGER,display_name TEXT,preferred_ui_locale TEXT,preferred_correspondence_locale TEXT,created_ts INTEGER,last_login_ts INTEGER)`)
	if err != nil {
		t.Fatal("could not create synthetic source table")
	}
	record := "scrypt$16384$8$1$" + base64.RawURLEncoding.EncodeToString(make([]byte, 16)) + "$" + base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	_, err = db.Exec("INSERT INTO accounts VALUES(1,'Synthetic',?,'member',0,'','','',1700000000,NULL)", record)
	if err != nil {
		t.Fatal("could not insert synthetic source")
	}
	return path, record
}

func TestAdminConsoleImportValidatesFlagsAndFilesBeforeHub(t *testing.T) {
	for _, args := range [][]string{
		{"import"}, {"import", "--source", "unused"}, {"import", "--manifest", "unused"},
		{"import", "--source", "unused", "--manifest", "unused", "extra"},
		{"import", "--unknown"}, {"import-grants", "--manifest", "unused", "--source", "unused"},
	} {
		if err := adminConsoleCommand(context.Background(), nil, args, nil, io.Discard); err == nil || err.Error() != adminConsoleUsage {
			t.Fatal("invalid import flags accepted")
		}
	}
	for _, args := range [][]string{
		{"import", "--source", "unused", "--manifest", filepath.Join(t.TempDir(), "absent")},
		{"import", "--source", "unused", "--manifest", importManifestFile(t, "{")},
		{"import", "--source", filepath.Join(t.TempDir(), "absent"), "--manifest", importManifestFile(t, cliImportManifest)},
		{"import-grants", "--manifest", importManifestFile(t, "{")},
		{"import-grants", "--manifest", importManifestFile(t, cliAssignmentManifest+" {}")},
		{"import-grants", "--manifest", importManifestFile(t, `{"unknown":"field"}`)},
		{"import-grants", "--manifest", importManifestFile(t, strings.Repeat(" ", 32769)+cliAssignmentManifest)},
	} {
		if err := adminConsoleCommand(context.Background(), nil, args, nil, io.Discard); err == nil || strings.Contains(err.Error(), "hub client") {
			t.Fatal("invalid input reached hub")
		}
	}
}

func TestAdminConsoleImportSendsBoundedSnapshotAndExplicitDryRun(t *testing.T) {
	source, record := cliImportSource(t)
	manifest := importManifestFile(t, cliImportManifest)
	var requests int
	client := tokenClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		var request struct {
			Batch  accountimport.Batch `json:"batch"`
			DryRun bool                `json:"dry_run"`
		}
		if json.NewDecoder(r.Body).Decode(&request) != nil || r.Method != "POST" || r.URL.Path != "/v1/admin/console/import" || r.URL.RawQuery != "" {
			t.Error("incorrect import request")
		}
		if request.DryRun != (requests == 1) || len(request.Batch.Rows) != 1 || request.Batch.Rows[0].PasswordHash != record || request.Batch.Rows[0].CreatedMS != 1700000000000 || request.Batch.Rows[0].LastLoginMS != nil {
			t.Error("snapshot or dry-run changed")
		}
		_, _ = w.Write([]byte(`{"created":1,"rows":[{"source_row_id":"1","action":"create","role":"member"}],"password_hash":"untrusted-response-secret"}`))
	})
	for _, apply := range []bool{false, true} {
		args := []string{"console", "import", "--source", source, "--manifest", manifest}
		if apply {
			args = append(args, "--apply")
		}
		var out bytes.Buffer
		if err := adminCommand(context.Background(), client, args, &out); err != nil {
			t.Fatal("import request failed")
		}
		if !strings.Contains(out.String(), `"created": 1`) || strings.Contains(out.String(), record) || strings.Contains(out.String(), "untrusted-response-secret") {
			t.Fatal("report missing result or leaked credential material")
		}
	}
	if requests != 2 {
		t.Fatal("missing import request")
	}
}

func TestAdminConsoleAssignmentTransferAndSanitizedFailures(t *testing.T) {
	manifest := importManifestFile(t, cliAssignmentManifest)
	var requests int
	client := tokenClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		var body map[string]json.RawMessage
		if json.NewDecoder(r.Body).Decode(&body) != nil || r.URL.Path != "/v1/admin/console/import-grants" || string(body["dry_run"]) != "true" || !bytes.Contains(body["transfer"], []byte("project-a")) {
			t.Error("assignment transfer changed")
		}
		_, _ = w.Write([]byte(`{"created":1}`))
	})
	args := []string{"import-grants", "--manifest", manifest}
	if err := adminConsoleCommand(context.Background(), client, args, nil, io.Discard); err != nil {
		t.Fatal("assignment transfer failed")
	}
	if err := adminConsoleCommand(context.Background(), client, args, nil, consoleBrokenWriter{}); err == nil {
		t.Fatal("report output failure hidden")
	}
	if requests != 2 {
		t.Fatal("missing assignment transfer")
	}
	badClient := tokenClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"code":"bad_request","message":"untrusted-request-secret"}`))
	})
	if err := adminConsoleCommand(context.Background(), badClient, args, nil, io.Discard); err == nil || strings.Contains(err.Error(), "untrusted-request-secret") {
		t.Fatal("server failure exposed payload")
	}
}

func TestImportFailureReportsOnlyRecognizedSafeRefusals(t *testing.T) {
	for _, test := range []struct {
		code, row, reason, want string
	}{
		{"conflict", "2", "login_collision", "console import refused: conflict; source row 2: login_collision"},
		{"conflict", "2", "untrusted-secret", "console import refused: conflict"},
		{"bad_request", "untrusted-secret", "source_changed", "console import refused: bad_request"},
		{"untrusted-secret", "2", "source_changed", "console import request failed; no result confirmed; repeat dry-run before retrying"},
	} {
		err := consoleImportFailure(&hubclient.Error{Code: test.code, Message: "untrusted-secret", Details: map[string]any{"source_row_id": test.row, "reason": test.reason}})
		if err.Error() != test.want {
			t.Fatal("refusal lost safe reason or leaked response data")
		}
	}
	if err := consoleImportFailure(io.EOF); !strings.Contains(err.Error(), "no result confirmed") {
		t.Fatal("transport failure incorrectly claims refusal")
	}
}
