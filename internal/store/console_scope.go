package store

import "strconv"

// repositoryVisibleSQL is applied before grouping and pagination. A missing
// filter on a member request means the union of its explicit read grants.
func (t *transaction) repositoryVisibleSQL(column string) (string, []any) {
	if t.console == nil || t.console.Role == "operator" {
		return "1=1", nil
	}
	return `EXISTS(SELECT 1 FROM ConsoleRepositoryAccess cg WHERE cg.account_id=? AND cg.repository_id=` + column + ` AND cg.mode IN ('ro','rw'))`, []any{t.console.ID}
}

// visibleIn combines ADR 004's per-message envelope with the account predicate;
// a reply or a shared thread identifier never widens the repository grant.
func (t *transaction) visibleIn(alias, repo string) (string, []any) {
	filter, filterArgs := visibleIn(alias, repo)
	origin, originArgs := t.repositoryVisibleSQL(alias + ".repository_id")
	recipient, recipientArgs := t.repositoryVisibleSQL("av.repository_id")
	return "(" + filter + ") AND (" + origin + " OR EXISTS(SELECT 1 FROM Delivery dv JOIN Agent av ON av.id=dv.recipient_agent_id WHERE dv.message_id=" + alias + ".id AND " + recipient + "))", append(append(filterArgs, originArgs...), recipientArgs...)
}

// Account searches filter search and participant matches before selecting a
// thread. Internal unscoped observations retain the store's search contract;
// they do not create a browser principal.
func (t *transaction) consoleMatchIn(alias, repo string) (string, []any) {
	if t.console == nil {
		return "1=1", nil
	}
	return t.visibleIn(alias, repo)
}

func (t *transaction) validateConsoleThreadFilters(r Request) error {
	if t.console == nil {
		return nil
	}
	if r.Agent != "" && r.Agent != "human" {
		scope, args := t.repositoryVisibleSQL("a.repository_id")
		var known int
		if err := t.row(`SELECT COUNT(*) FROM Agent a WHERE (a.id=? OR a.name=?) AND `+scope, append([]any{r.Agent, r.Agent}, args...)...).Scan(&known); err != nil {
			return err
		}
		if known == 0 {
			return fail("not_found", "unknown participant")
		}
	}
	if r.Before != "" {
		if _, err := strconv.ParseInt(r.Before, 10, 64); err != nil {
			visible, args := t.visibleIn("mc", r.RepositoryID)
			var known int
			if err := t.row(`SELECT COUNT(*) FROM Message mc WHERE mc.thread_id=? AND `+visible, append([]any{r.Before}, args...)...).Scan(&known); err != nil {
				return err
			}
			if known == 0 {
				return fail("not_found", "unknown thread cursor")
			}
		}
	}
	return nil
}

// Authors are immutable message facts, not a directory of account profiles.
// Only authors of this view's messages contribute to its participants.
func (t *transaction) threadAuthors(thread map[string]any, visible string, args []any) error {
	rows, err := t.rows(`SELECT DISTINCT m.sender_account_id,m.sender_username,m.sender_display_name FROM Message m WHERE m.thread_id=? AND m.sender_account_id IS NOT NULL AND `+visible+` ORDER BY m.sender_account_id,m.sender_username,m.sender_display_name`, append([]any{thread["id"]}, args...)...)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	authors := []map[string]any{}
	for rows.Next() {
		var accountID, username, displayName string
		rows.Scan(&accountID, &username, &displayName)
		author := map[string]any{}
		messageAttribution(accountID, username, displayName, author)
		authors = append(authors, author)
	}
	thread["authors"] = authors
	return rows.Err()
}
