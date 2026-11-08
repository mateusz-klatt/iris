package store

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

func TestMessageSearchTreatsTermsAsText(t *testing.T) {
	for _, tc := range []struct {
		query, text, distractor string
		matches                 int
	}{
		{"fetch-receipt", "fetch-receipt", "fetch interim receipt", 2},
		{"ADR:005", "ADR:005", "ADR 006", 2},
		{"C++", "C++", "Rust", 2},
		{`"quoted`, `"quoted`, "plain", 2},
		{"NOT", "NOT", "plain", 2},
		{"*", "*", "plain", 0}, // punctuation alone contributes no FTS tokens
		{`say"hello`, `say"hello`, "say then hello", 2},
		{"alpha \t beta", "alpha and beta", "alpha", 2},
		{"alpha OR beta", "alpha OR beta", "alpha beta", 2},
		{" \t\n\u2003 ", "matching", "plain", 3},
		{"", "matching", "plain", 3},
	} {
		t.Run(tc.query, func(t *testing.T) {
			s, _, _ := testStore(t)
			a, ar := session(t, s, 1)
			_, br := session(t, s, 2)
			to := br["agent"].(map[string]any)["name"].(string)
			title := historySend(t, s, a, to, "title", "", tc.text, "body")
			body := historySend(t, s, a, to, "body", "", "title", tc.text)
			other := historySend(t, s, a, to, "other", "", "other", tc.distractor)
			want := []string{}
			if tc.matches >= 2 {
				want = append(want, title["message_id"].(string), body["message_id"].(string))
			}
			if tc.matches == 3 {
				want = append(want, other["message_id"].(string))
			}
			slices.Sort(want)
			for _, op := range []string{"history", "ui_threads"} {
				r := a
				r.Q, r.RepositoryID = tc.query, ar["repository_id"].(string)
				field, idField := "messages", "message_id"
				if op == "ui_threads" {
					field, idField = "threads", "id"
				}
				items := call(t, s, op, r)[field].([]map[string]any)
				got := []string{}
				for _, item := range items {
					got = append(got, item[idField].(string))
				}
				slices.Sort(got)
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("%s search %q returned %v, want %v", op, tc.query, got, want)
				}
			}
		})
	}
}

func TestSQLiteMatchErrorsAreBadRequests(t *testing.T) {
	s, _, _ := testStore(t)
	a, ar := session(t, s, 1)
	_, br := session(t, s, 2)
	historySend(t, s, a, br["agent"].(map[string]any)["name"].(string), "mail", "", "subject", "body")
	// SQLite's MATCH parser stops at an embedded NUL, before the closing quote.
	// This reaches the real FTS engine even after literal quoting.
	a.Q, a.RepositoryID = "\x00", ar["repository_id"].(string)
	var underlying *sqlite.Error
	err := s.db.QueryRow("SELECT COUNT(*) FROM MessageSearch WHERE MessageSearch MATCH ?", literalMatch(a.Q)).Scan(new(int))
	if !errors.As(err, &underlying) || underlying.Code() != sqlite3.SQLITE_ERROR {
		t.Fatalf("expected an FTS parser error, got %v", err)
	}
	before := databaseState(t, s)
	for _, op := range []string{"history", "ui_threads"} {
		expectError(t, s, op, a, "bad_request")
	}
	if after := databaseState(t, s); after != before {
		t.Fatal("failed search changed store state")
	}
	// Other driver/storage errors keep their identity for rollback and retry.
	if err := matchError(errStorageFault); !errors.Is(err, errStorageFault) {
		t.Fatal(err)
	}
	if err := matchError(context.Canceled); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
