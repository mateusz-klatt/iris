package doctor

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mateusz-klatt/iris/internal/integrate"
)

func doctorRow(t *testing.T, layout integrate.Layout, client string) ClientReport {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	res, err := Run(context.Background(), Options{Layout: layout, HubURL: srv.URL, TokenFile: filepath.Join(layout.Home, "hub.token"), HTTP: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range res.Report.Clients {
		if row.Client == client {
			return row
		}
	}
	t.Fatalf("no row for %s", client)
	return ClientReport{}
}

// The canary runs only sessionStart. With preToolUse gone, nothing guards an edit, and the
// row says so instead of ok (gap review C3).
func TestAPartialIntegrationNeedsAHuman(t *testing.T) {
	t.Setenv("IRIS_FAKE_STDOUT", "{}")
	t.Setenv("IRIS_FAKE_EXIT", "0")
	layout := doctorLayout(t)
	cursor, _ := integrate.ByName(integrate.Cursor)
	if err := cursor.Install(layout, nil); err != nil {
		t.Fatal(err)
	}
	// The test layout has no cursor binary, which doctor says; a complete integration has no drift.
	if row := doctorRow(t, layout, integrate.Cursor); strings.Contains(row.Detail, "differs") {
		t.Fatalf("a complete integration: %+v", row)
	}
	files, _ := cursor.Files(layout)
	raw, _ := os.ReadFile(files[0])
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	delete(doc["hooks"].(map[string]any), "preToolUse")
	edited, _ := json.Marshal(doc)
	if err := os.WriteFile(files[0], edited, 0o600); err != nil {
		t.Fatal(err)
	}
	row := doctorRow(t, layout, integrate.Cursor)
	want := "configuration differs from what iris integrate installs (" + filepath.Join("~", ".cursor", "hooks.json") + "): run iris integrate cursor"
	if row.Status != StatusNeedsHuman || row.Detail != want {
		t.Fatalf("%+v", row)
	}
}

// A file the canary does not read can still be broken. Doctor cannot tell what it holds, so
// the client is not protected as far as it knows.
func TestABrokenFileBesideTheCanaryIsSilent(t *testing.T) {
	t.Setenv("IRIS_FAKE_STDOUT", "{}")
	t.Setenv("IRIS_FAKE_EXIT", "0")
	layout := doctorLayout(t)
	cursor, _ := integrate.ByName(integrate.Cursor)
	if err := cursor.Install(layout, nil); err != nil {
		t.Fatal(err)
	}
	files, _ := cursor.Files(layout)
	if err := os.WriteFile(files[1], []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if row := doctorRow(t, layout, integrate.Cursor); row.Status != StatusSilent || row.Detail == "" {
		t.Fatalf("%+v", row)
	}
}

// A configuration outside the home directory, as with CODEX_HOME, is named in full.
func TestDriftOutsideHomeKeepsItsPath(t *testing.T) {
	t.Setenv("IRIS_FAKE_STDOUT", "{}")
	t.Setenv("IRIS_FAKE_EXIT", "0")
	layout := doctorLayout(t)
	elsewhere := t.TempDir()
	layout.Getenv = func(name string) string {
		if name == "CODEX_HOME" {
			return elsewhere
		}
		return ""
	}
	codex, _ := integrate.ByName(integrate.Codex)
	if err := codex.Install(layout, nil); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(elsewhere, "config.toml")
	if err := os.WriteFile(config, []byte("# the mcp block is gone\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	row := doctorRow(t, layout, integrate.Codex)
	if row.Status != StatusNeedsHuman || !strings.Contains(row.Detail, "("+config+")") {
		t.Fatalf("%+v", row)
	}
}

// A stand-in that answers {} and exits 0, such as a guard wrapper that never runs Iris, used to
// pass. Only iris hook answers doctor's nonce, so without that receipt the row is silent.
func TestAHookThatDoesNotReachIrisIsSilent(t *testing.T) {
	t.Setenv("IRIS_FAKE_STDOUT", "{}")
	t.Setenv("IRIS_FAKE_EXIT", "0")
	layout := doctorLayout(t)
	codex, _ := integrate.ByName(integrate.Codex)
	if err := codex.Install(layout, nil); err != nil {
		t.Fatal(err)
	}
	if row := doctorRow(t, layout, integrate.Codex); row.Status == StatusSilent {
		t.Fatalf("with a receipt: %+v", row)
	}
	t.Setenv("IRIS_FAKE_NO_RECEIPT", "1")
	row := doctorRow(t, layout, integrate.Codex)
	if row.Status != StatusSilent || !strings.Contains(row.Detail, "not from Iris") {
		t.Fatalf("%+v", row)
	}
}
