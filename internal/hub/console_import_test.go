package hub

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/mateusz-klatt/iris/internal/accountimport"
	"github.com/mateusz-klatt/iris/internal/store"
)

func importHTTPBatch(t *testing.T) accountimport.Batch {
	t.Helper()
	path := filepath.Join(t.TempDir(), "synthetic.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal("could not open synthetic source")
	}
	defer func() { _ = db.Close() }()
	_, err = db.Exec(`CREATE TABLE accounts(id INTEGER,username TEXT,password_hash TEXT,role TEXT,disabled INTEGER,display_name TEXT,preferred_ui_locale TEXT,preferred_correspondence_locale TEXT,created_ts INTEGER,last_login_ts INTEGER)`)
	if err != nil {
		t.Fatal("could not create synthetic source")
	}
	record := "scrypt$16384$8$1$" + base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{5}, 16)) + "$" + base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{8}, 32))
	_, err = db.Exec("INSERT INTO accounts VALUES(1,?,?,?,?,?,?,?,?,NULL)", "Synthetic", record, "member", 0, "Synthetic display", "en", "pl", 1700000000)
	if err != nil {
		t.Fatal("could not insert synthetic source")
	}
	batch, err := accountimport.Read(context.Background(), path, accountimport.Manifest{Version: 1, SourceID: "synthetic", Table: "accounts", TimestampEncoding: "unix_seconds", TimestampZone: "UTC"})
	if err != nil {
		t.Fatal("could not read synthetic source")
	}
	return batch
}

func importHTTPBody(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal("could not encode synthetic import request")
	}
	return string(raw)
}

func importHTTPReport(t *testing.T, w *httptest.ResponseRecorder) store.ConsoleImportReport {
	t.Helper()
	var report store.ConsoleImportReport
	if err := json.Unmarshal(w.Body.Bytes(), &report); err != nil {
		t.Fatal("import report is not JSON")
	}
	return report
}

func TestOwnerImportWorksBeforeConsoleCutoverAndReportsOnlySafeFields(t *testing.T) {
	h, server := setup(t)
	batch := importHTTPBatch(t)
	var logs bytes.Buffer
	h.Log = log.New(&logs, "", 0)
	h.AccessLog = true
	const path = "/v1/admin/console/import"
	for _, dryRun := range []bool{true, false, false} {
		body := importHTTPBody(t, map[string]any{"batch": batch, "dry_run": dryRun})
		w := ownerAccountRequest(t, h, server, path, body, "test-secret", http.StatusOK)
		report := importHTTPReport(t, w)
		if report.DryRun != dryRun || len(report.Rows) != 1 || report.Rows[0].SourceRowID != "1" || report.Rows[0].Role != "member" || report.Quarantined != 0 || report.Created+report.Skipped != 1 {
			t.Fatal("import report lost the bounded row result")
		}
		if w.Header().Get("Cache-Control") != "no-store" || len(w.Result().Cookies()) != 0 {
			t.Fatal("import returned a cacheable response or browser session")
		}
		if dryRun {
			if report.Rows[0].AccountID != "" {
				t.Fatal("dry-run assigned an account identity")
			}
			if _, err := h.store.LookupConsoleAccount(context.Background(), "Synthetic"); err == nil {
				t.Fatal("dry-run persisted a source account")
			}
		}
		for _, forbidden := range []string{batch.Rows[0].PasswordHash, batch.Rows[0].Username, batch.Rows[0].DisplayName, "source_fingerprint", "password_hash"} {
			if strings.Contains(w.Body.String(), forbidden) || strings.Contains(logs.String(), forbidden) {
				t.Fatal("import returned or logged source row data")
			}
		}
	}
	account, err := h.store.LookupConsoleAccount(context.Background(), "Synthetic")
	if err != nil || account.PasswordHash != batch.Rows[0].PasswordHash || account.LastLoginMS != nil || account.CreatedMS != 1700000000000 {
		t.Fatal("HTTP apply did not retain the source credential and nullable history")
	}
}

func TestOwnerImportRequiresOwnerWithoutBrowserOrMultipleCredentials(t *testing.T) {
	h, server := setup(t)
	batch := importHTTPBatch(t)
	body := importHTTPBody(t, map[string]any{"batch": batch, "dry_run": false})
	host, err := h.store.AddToken(context.Background(), "import-host")
	if err != nil {
		t.Fatal("could not arrange host token")
	}
	for _, path := range []string{"/v1/admin/console/import", "/v1/admin/console/import-grants"} {
		ownerAccountRequest(t, h, server, path, body, "", http.StatusUnauthorized)
		ownerAccountRequest(t, h, server, path, body, host, http.StatusForbidden)
		for _, cookie := range []string{"iris_console_session=synthetic", "iris_session=synthetic", "iris_console_login=synthetic"} {
			r := httptest.NewRequest(http.MethodPost, server.URL+path, strings.NewReader(body))
			r.Header.Set("Authorization", "Bearer test-secret")
			r.Header.Set("Cookie", cookie)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != http.StatusForbidden {
				t.Fatal("mixed browser credentials reached importer")
			}
		}
		r := httptest.NewRequest(http.MethodPost, server.URL+path, strings.NewReader(body))
		r.Header.Add("Authorization", "Bearer test-secret")
		r.Header.Add("Authorization", "Bearer "+host)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusForbidden {
			t.Fatal("multiple machine credentials reached importer")
		}
	}
	if _, err = h.store.LookupConsoleAccount(context.Background(), "Synthetic"); err == nil {
		t.Fatal("refused credential path changed accounts")
	}
}

func TestOwnerImportRejectsUnreviewedPayloadsAndDuplicateFields(t *testing.T) {
	h, server := setup(t)
	batch := importHTTPBatch(t)
	const path = "/v1/admin/console/import"
	valid := importHTTPBody(t, map[string]any{"batch": batch, "dry_run": true})
	for _, body := range []string{
		`{`, `null`, `[]`, `{}`, `{"dry_run":true}`, `{"batch":{}}`,
		`{"dry_run":true,"batch":{},"transfer":{}}`, `{"dry_run":true,"transfer":{}}`,
		`{"dry_run":true,"batch":{},"unexpected":true}`, valid + `{}`,
		`{"dry_run":true,"dry_run":false,"batch":{}}`,
		`{"dry_run":true,"Dry_Run":false,"batch":{}}`, `{"dry_run":true,"BATCH":{}}`,
		`{"batch":{"manifest":{"renames":{"1":"A","1":"B"}}},"dry_run":true}`,
		`{"batch":[null,],"dry_run":true}`, `{"batch":[],"dry_run":true}`,
		`{"batch":{"manifest":1},"dry_run":true}`, `{"batch":{"manifest":{"version":1},},"dry_run":true}`,
		`{"dry_run":true,"batch":{1:true}}`, `{"batch":{"manifest":{"source_id":"\ud800"}},"dry_run":true}`,
	} {
		ownerAccountRequest(t, h, server, path, body, "test-secret", http.StatusBadRequest)
	}
	ownerAccountRequest(t, h, server, "/v1/admin/console/import-grants", valid, "test-secret", http.StatusBadRequest)
	batch.Rows[0].PasswordHash = "malformed-synthetic-record"
	body := importHTTPBody(t, map[string]any{"batch": batch, "dry_run": false})
	w := ownerAccountRequest(t, h, server, path, body, "test-secret", http.StatusBadRequest)
	if strings.Contains(w.Body.String(), batch.Rows[0].PasswordHash) {
		t.Fatal("malformed source credential reached error response")
	}
}

func TestOwnerImportConflictAndStorageDiagnosticsRemainSanitized(t *testing.T) {
	h, server, filename := consoleSetup(t)
	batch := importHTTPBatch(t)
	var logs bytes.Buffer
	h.Log = log.New(&logs, "", 0)
	h.AccessLog = true
	const path = "/v1/admin/console/import"
	body := importHTTPBody(t, map[string]any{"batch": batch, "dry_run": false})
	consoleDB(t, filename, `CREATE TRIGGER import_storage_refusal BEFORE INSERT ON ConsoleAccount BEGIN SELECT RAISE(ABORT,'synthetic-sensitive-diagnostic'); END`)
	w := ownerAccountRequest(t, h, server, path, body, "test-secret", http.StatusBadRequest)
	if strings.Contains(w.Body.String(), "synthetic-sensitive-diagnostic") || strings.Contains(logs.String(), "synthetic-sensitive-diagnostic") || strings.Contains(w.Body.String(), batch.Rows[0].PasswordHash) || strings.Contains(logs.String(), batch.Rows[0].PasswordHash) {
		t.Fatal("storage error exposed sensitive diagnostic data")
	}
	consoleDB(t, filename, "DROP TRIGGER import_storage_refusal")
	ownerAccountRequest(t, h, server, path, body, "test-secret", http.StatusOK)
	batch.Rows[0].DisplayName = "changed source profile"
	w = ownerAccountRequest(t, h, server, path, importHTTPBody(t, map[string]any{"batch": batch, "dry_run": false}), "test-secret", http.StatusConflict)
	if !strings.Contains(w.Body.String(), `"source_row_id":"1"`) || strings.Contains(w.Body.String(), batch.Rows[0].DisplayName) || strings.Contains(logs.String(), batch.Rows[0].DisplayName) {
		t.Fatal("changed rerun did not return a safe row refusal")
	}
}

func TestOwnerImportSeparateReviewedViewerTransfer(t *testing.T) {
	h, server := setup(t)
	batch := importHTTPBatch(t)
	ownerAccountRequest(t, h, server, "/v1/admin/console/import", importHTTPBody(t, map[string]any{"batch": batch, "dry_run": false}), "test-secret", http.StatusOK)
	account, err := h.store.LookupConsoleAccount(context.Background(), "Synthetic")
	if err != nil {
		t.Fatal("account import missing")
	}
	repo := register(t, server, 1)["repository_id"].(string)
	transfer := store.ConsoleAssignmentTransfer{Version: 1, SourceID: "synthetic", Rows: []store.ConsoleAssignmentRow{{ID: "1", AccountRowID: "1", ProjectID: "1", Role: "viewer"}}, Projects: map[string]string{"1": repo}}
	const path = "/v1/admin/console/import-grants"
	for _, dryRun := range []bool{true, false, false} {
		w := ownerAccountRequest(t, h, server, path, importHTTPBody(t, map[string]any{"transfer": transfer, "dry_run": dryRun}), "test-secret", http.StatusOK)
		report := importHTTPReport(t, w)
		if report.DryRun != dryRun || len(report.Rows) != 1 || report.Rows[0].Role != "ro" || report.Created+report.Skipped != 1 {
			t.Fatal("assignment transfer did not report viewer mapping")
		}
		mode, err := h.store.ConsoleRepositoryMode(context.Background(), account.ID, repo)
		if err != nil || (dryRun && mode != "") || (!dryRun && mode != "ro") {
			t.Fatal("reviewed assignment transfer did not honor dry-run and ro policy")
		}
	}
	transfer.Rows[0].Role = "operator"
	ownerAccountRequest(t, h, server, path, importHTTPBody(t, map[string]any{"transfer": transfer, "dry_run": false}), "test-secret", http.StatusConflict)
}

type importReadFailure struct{}

func (importReadFailure) Read([]byte) (int, error) { return 0, errors.New("synthetic read failure") }

func TestOwnerImportBodyReadFailuresStayGeneric(t *testing.T) {
	h, _ := setup(t)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/v1/admin/console/import", nil)
	r.Header.Set("Authorization", "Bearer test-secret")
	r = r.WithContext(context.WithValue(r.Context(), principalKey{}, principal{name: ownerName}))
	r.Body = io.NopCloser(importReadFailure{})
	h.consoleImport(w, r, false)
	if w.Code != http.StatusBadRequest || strings.Contains(w.Body.String(), "synthetic read failure") {
		t.Fatal("import reader failure returned unsafe success")
	}
}

func TestOwnerImportAcceptsBoundedBatchBeyondManifestFileLimit(t *testing.T) {
	h, server := setup(t)
	batch := importHTTPBatch(t)
	row := batch.Rows[0]
	batch.Rows = nil
	for i := 1; i <= accountimport.MaxRows; i++ {
		row.ID = strconv.Itoa(i)
		row.Username = strings.Repeat("界", 62) + row.ID
		row.DisplayName = strings.Repeat("🚀", 128)
		row.Role = strings.Repeat("q", 64)
		batch.Rows = append(batch.Rows, row)
	}
	body := importHTTPBody(t, map[string]any{"batch": batch, "dry_run": true})
	if len(body) <= accountimport.MaxManifestBytes || len(body) >= 64<<10 {
		t.Fatal("synthetic batch did not exercise the separate HTTP payload limit")
	}
	w := ownerAccountRequest(t, h, server, "/v1/admin/console/import", body, "test-secret", http.StatusOK)
	report := importHTTPReport(t, w)
	if report.Created != accountimport.MaxRows || report.Quarantined != accountimport.MaxRows {
		t.Fatal("large bounded batch lost source rows")
	}
}
