package accountimport

import (
	"database/sql"
	"strings"
	"testing"
)

func TestSourceQueryRefusesIdentifierInjectionBeforeAnySQL(t *testing.T) {
	file := sourceFile(t, sourceSchema, 1)
	state := &sourceFault{}
	db, err := faultOpen(t, state)("sqlite", sourceURI(file))
	if err != nil {
		t.Fatal("could not open synthetic source")
	}
	defer func() { _ = db.Close() }()
	tx, err := db.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal("could not start synthetic snapshot")
	}
	defer func() { _ = tx.Rollback() }()
	before := state.count
	for _, table := range []string{"", "1accounts", "main.accounts", `accounts"`, "accounts; DROP TABLE accounts", "accounts --", "café", "accounts\n", "accounts\x00", strings.Repeat("a", 65)} {
		query, err := sourceAccountQuery(table)
		if err == nil || query != "" || err.Error() != "invalid source table identifier" {
			t.Fatal("unsafe source identifier became SQL or disclosed input")
		}
		manifest := testManifest()
		manifest.Table = table
		batch, err := snapshot(t.Context(), tx, manifest)
		if err == nil || len(batch.Rows) != 0 || state.count != before {
			t.Fatal("invalid source identifier reached SQLite")
		}
	}
	for _, table := range []string{"accounts", "_accounts_2026", "Accounts", strings.Repeat("a", 64)} {
		query, err := sourceAccountQuery(table)
		if err != nil || !strings.HasSuffix(query, `FROM "`+table+`" LIMIT ?`) {
			t.Fatal("allowlisted identifier or bound row limit was refused")
		}
	}
}
