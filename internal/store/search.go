package store

import (
	"errors"
	"strings"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// literalMatch requires every whitespace-separated term, with punctuation and
// quotes treated as text rather than FTS5 query operators. SQLite still tokenizes
// that text: this is full-text search, not an exact substring match.
func literalMatch(query string) string {
	terms := strings.Fields(query)
	for i, term := range terms {
		terms[i] = `"` + strings.ReplaceAll(term, `"`, `""`) + `"`
	}
	return strings.Join(terms, " ")
}

// FTS5 reports invalid MATCH input as SQLITE_ERROR, even when it occurs while
// stepping rows. Keep driver and I/O failures distinct from invalid searches.
func matchError(err error) error {
	var sqliteErr *sqlite.Error
	if errors.As(err, &sqliteErr) && sqliteErr.Code() == sqlite3.SQLITE_ERROR {
		return fail("bad_request", "invalid message search")
	}
	return err
}
