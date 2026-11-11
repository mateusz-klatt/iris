package hub

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/mateusz-klatt/iris/internal/config"
	"github.com/mateusz-klatt/iris/internal/store"
)

// This exercises the supported recovery operations on synthetic restored state.
// Isolation and keeping recovery passwords private until access review finishes
// remain owner procedures; the hub cannot enforce either outside its own routes.
func TestIsolatedScryptRestoreRecoversAndReviewsOriginalAccess(t *testing.T) {
	h, server, filename := consoleSetup(t)
	repository := register(t, server, 1)["repository_id"].(string)
	operator := consoleAccount(t, h, "Recovery operator", "operator")
	members := []store.ConsoleAccount{
		consoleAccount(t, h, "Restored member", "member"),
		consoleAccount(t, h, "Restored disabled member", "member"),
	}
	for i := range members {
		var err error
		members[i], err = h.store.ReplaceConsoleAccess(t.Context(), members[i].ID, members[i].EditRevision, "member", false, []store.ConsoleGrant{{RepositoryID: repository, Mode: []string{"ro", "rw"}[i]}})
		if err != nil {
			t.Fatal("could not arrange restored access", err)
		}
	}
	const legacyHash = "scrypt$16384$8$1$AAECAwQFBgcICQoLDA0ODw$5UJCybzgeNrUxZTV5mzrOYr8Aj1OlDXLrgTFXHxr1m4"
	var oldDigests [][]byte
	for i, account := range append([]store.ConsoleAccount{operator}, members...) {
		digest := bytes.Repeat([]byte{byte(i + 1)}, 32)
		if _, err := h.store.IssueConsoleSession(t.Context(), account, digest); err != nil {
			t.Fatal("could not arrange restored session", err)
		}
		oldDigests = append(oldDigests, digest)
		// Only fixture construction writes SQL: recovery below uses owner and
		// account routes, never direct edits behind the serving hub.
		consoleDB(t, filename, "UPDATE ConsoleAccount SET password_hash=?,disabled=? WHERE id=?", legacyHash, i == 2, account.ID)
	}
	db, err := sql.Open("sqlite", filename)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	assertSessions := func(want int) {
		t.Helper()
		var got int
		if err := db.QueryRow("SELECT COUNT(*) FROM ConsoleSession").Scan(&got); err != nil || got != want {
			t.Fatalf("restored sessions = %d, want %d: %v", got, want, err)
		}
	}
	keyPath := filepath.Join(t.TempDir(), "fresh-console.key")
	var freshKey []byte
	if err := h.ConfigureConsole(nil, nil, nil, func() error {
		var err error
		freshKey, err = config.ConsoleKey(keyPath)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	consoleDo(t, h, server.URL, "GET", "/v1/ui/login", nil, consoleBrowser{}, 401)
	reset := func(name, password string) {
		t.Helper()
		w := ownerAccountRequest(t, h, server, "/v1/admin/console/accounts/reset", activationPayload(name, password), "test-secret", 200)
		if len(w.Result().Cookies()) != 0 || strings.Contains(w.Body.String(), password) {
			t.Fatal("recovery returned a password or browser authority")
		}
	}
	reset(operator.Username, consoleTestPassword)
	assertSessions(2)
	if _, err := h.store.ConsoleSessionInfo(t.Context(), oldDigests[0]); err == nil {
		t.Fatal("operator recovery retained its restored session")
	}
	w := ownerAccountRequest(t, h, server, "/v1/admin/console/activate", activationPayload(operator.Username, consoleTestPassword), "test-secret", 200)
	if len(w.Result().Cookies()) != 0 || consoleJSON(t, w)["restart_required"] != true || len(freshKey) != 32 || bytes.Equal(freshKey, bytes.Repeat([]byte{9}, 32)) {
		t.Fatal("activation omitted fresh independent key material or issued a session")
	}
	assertSessions(0)
	consoleDo(t, h, server.URL, "GET", "/v1/ui/login", nil, consoleBrowser{}, 401)
	if err := h.ConfigureConsole(freshKey, nil, nil, nil); err != nil {
		t.Fatal("restart did not open recovered named login", err)
	}
	browser := consoleSignIn(t, h, server.URL, operator.Username)
	listed := func(id string) store.ConsoleAccessAccount {
		t.Helper()
		w := consoleDo(t, h, server.URL, "GET", "/v1/ui/accounts", nil, browser, 200)
		var result struct{ Accounts []store.ConsoleAccessAccount }
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		for _, account := range result.Accounts {
			if account.ID == id {
				return account
			}
		}
		t.Fatal("access review omitted a restored account")
		return store.ConsoleAccessAccount{}
	}
	accessBody := func(record store.ConsoleAccessAccount, revision int64) map[string]any {
		return map[string]any{"edit_revision": revision, "role": record.Role, "disabled": record.Disabled, "grants": record.Grants}
	}
	// Preserve these records before any member reset, including disabled grants.
	records := []store.ConsoleAccessAccount{listed(members[0].ID), listed(members[1].ID)}
	for i, record := range records {
		if record.Role != "member" || record.Disabled != (i == 1) || !reflect.DeepEqual(record.Grants, []store.ConsoleGrant{{RepositoryID: repository, Mode: []string{"ro", "rw"}[i]}}) {
			t.Fatal("pre-reset access record lost restored authority")
		}
		reset(record.Username, []string{"private synthetic member recovery", "private synthetic disabled recovery"}[i])
		path := "/v1/ui/accounts/" + record.ID + "/access"
		current := listed(record.ID)
		if current.Role != "operator" || current.Disabled || len(current.Grants) != 0 || current.EditRevision <= record.EditRevision {
			t.Fatal("reset did not expose its temporary enabled operator state")
		}
		consoleDo(t, h, server.URL, "PUT", path, accessBody(record, record.EditRevision), browser, 409)
		invalid := accessBody(record, current.EditRevision)
		invalid["grants"] = []store.ConsoleGrant{{RepositoryID: repository, Mode: "invalid"}}
		consoleDo(t, h, server.URL, "PUT", path, invalid, browser, 400)
		if !reflect.DeepEqual(listed(record.ID), current) {
			t.Fatal("failed save changed access or revision")
		}
		// Reload the revision, then apply the original record, never the
		// temporary operator state left by reset or a stale pre-reset form.
		current = listed(record.ID)
		consoleDo(t, h, server.URL, "PUT", path, accessBody(record, current.EditRevision), browser, 200)
		checked := listed(record.ID)
		if checked.Role != record.Role || checked.Disabled != record.Disabled || !reflect.DeepEqual(checked.Grants, record.Grants) {
			t.Fatal("reviewed access did not read back as recorded before reset")
		}
	}
	for _, digest := range oldDigests {
		if _, err := h.store.ConsoleSessionInfo(t.Context(), digest); err == nil {
			t.Fatal("recovery or activation retained a restored session")
		}
	}
	assertSessions(1)
}
