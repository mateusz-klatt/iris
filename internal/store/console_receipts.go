package store

import "database/sql"

// consoleReceipts observes shared delivery state without ACKing it. An author
// has exactly the same current repository scope as every other account reader.
func (t *transaction) consoleReceipts(r Request) (map[string]any, error) {
	visible, visibleArgs := t.visibleIn("m", r.RepositoryID)
	originScope, originArgs := t.repositoryVisibleSQL("m.repository_id")
	var repo, client, account, username, display string
	err := t.row(`SELECT CASE WHEN `+originScope+` THEN m.repository_id ELSE '' END,x.client,
 COALESCE(m.sender_account_id,''),COALESCE(m.sender_username,''),COALESCE(m.sender_display_name,'')
 FROM Message m JOIN Execution x ON x.id=m.sender_execution_id WHERE m.id=? AND `+visible,
		append(append(originArgs, r.Key), visibleArgs...)...).Scan(&repo, &client, &account, &username, &display)
	if err != nil {
		return nil, missing(err, "unknown message")
	}
	recipientScope, recipientArgs := t.repositoryVisibleSQL("a.repository_id")
	rows, err := t.rows(`SELECT d.id,CASE WHEN a.client='human' THEN 'human' ELSE a.name END,a.repository_id,d.state,COALESCE(d.handled_ms,d.acked_ms)
 FROM DeliveryState d JOIN Agent a ON a.id=d.recipient_agent_id WHERE d.message_id=? AND `+recipientScope+` ORDER BY a.name,d.id`, append([]any{r.Key}, recipientArgs...)...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	recipients := []map[string]any{}
	for rows.Next() {
		var delivery, name, repository, state string
		var at sql.NullInt64
		rows.Scan(&delivery, &name, &repository, &state, &at)
		recipients = append(recipients, map[string]any{"id": delivery, "recipient": name, "repository_id": repository, "state": state, "at": timestamp(at)})
	}
	out := map[string]any{"message_id": r.Key, "recipients": recipients}
	messageAttribution(account, username, display, out)
	if repo != "" {
		out["repository_id"] = repo
		if client == "human" {
			out["reply_address"] = "human:" + repo
		}
	}
	return out, rows.Err()
}
