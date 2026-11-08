package store

// consoleEvents exposes a durable account cursor, never the global Event cursor.
// Mutation triggers update it only for changes in that account's readable scope.
// The caller validates the current account and session in this same transaction.
func (t *transaction) consoleEvents() (map[string]any, error) {
	var revision int64
	err := t.row("SELECT revision FROM ConsoleRevision WHERE account_id=?", t.console.ID).Scan(&revision)
	return map[string]any{"cursor": revision, "kind": "changed"}, err
}
