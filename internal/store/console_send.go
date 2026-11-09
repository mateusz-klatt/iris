package store

// consoleWriteRepository checks current records, including on a retry after a
// repair. An old request's repository ID is never authority for a new message.
func (t *transaction) consoleWriteRepository(repo string) error {
	var allowed bool
	err := t.row(`SELECT EXISTS(SELECT 1 FROM Repository r WHERE r.id=? AND (?='operator' OR EXISTS
 (SELECT 1 FROM ConsoleRepositoryAccess g WHERE g.account_id=? AND g.repository_id=r.id AND g.mode='rw')))`, repo, t.console.Role, t.console.ID).Scan(&allowed)
	if err != nil {
		return err
	}
	if !allowed {
		return fail("not_found", "unknown writable repository")
	}
	return nil
}

// messageAttribution contains only the immutable public author snapshots. Old
// shared mail deliberately has no person attributed to it.
func messageAttribution(account, username, display string, out map[string]any) {
	if account != "" {
		out["sender_account_id"] = account
		out["sender_username"] = username
		out["sender_display_name"] = display
	}
}

func (t *transaction) consoleSend(r Request) (map[string]any, error) {
	request := consoleMessageRequest{RepositoryID: r.RepositoryID, To: r.To, Subject: r.Subject, Body: r.Body, ReplyTo: r.ReplyTo}
	operation := "send"
	if r.ReplyTo != "" {
		operation = "reply"
	}
	saved, err := t.consoleOperation(t.console.ID, r.Key, operation, request)
	if err != nil {
		return nil, err
	}
	if saved != nil {
		return t.consoleReplay(saved)
	}
	if len(r.Body) > 16384 || len(r.Subject) > 1024 {
		return nil, fail("too_large", "body is limited to 16 KiB and subject to 1024 bytes")
	}
	if len(r.To) == 0 || len(r.To) > 100 {
		return nil, fail("bad_request", "provide 1 to 100 recipients")
	}
	repo := r.RepositoryID
	if t.console.Role == "member" && repo == "" {
		return nil, fail("bad_request", "member sends require an explicit repository_id")
	}
	if t.console.Role == "operator" {
		repo, err = t.operatorRepository(r)
		if err != nil {
			return nil, err
		}
	}
	if err = t.consoleWriteRepository(repo); err != nil {
		return nil, err
	}
	// Resolve the entire batch before writing any mailbox, message or delivery.
	recipients := []recipient{}
	seen := map[string]bool{}
	for _, address := range r.To {
		var a recipient
		if t.console.Role == "member" {
			err = t.row("SELECT id,name,repository_id FROM Agent WHERE id=? AND client<>'human'", address).Scan(&a.id, &a.name, &a.repo)
			err = missing(err, "unknown recipient")
		} else {
			a, err = t.resolve(execution{Repo: repo}, address)
		}
		if err != nil {
			return nil, err
		}
		if err = t.consoleWriteRepository(a.repo); err != nil {
			return nil, err
		}
		if !seen[a.id] {
			recipients = append(recipients, a)
			seen[a.id] = true
		}
	}
	message := id("ms_")
	thread := message
	if r.ReplyTo != "" {
		visible, args := visibleIn("m", repo)
		if err = t.row("SELECT m.thread_id FROM Message m WHERE m.id=? AND "+visible, append([]any{r.ReplyTo}, args...)...).Scan(&thread); err != nil {
			return nil, missing(err, "unknown reply in the sending repository")
		}
	}
	cross := 0
	for _, a := range recipients {
		if a.repo != repo {
			cross++
		}
	}
	// Account identity survives logout/relogin. Retries consume no send quota.
	// Both quotas are conservative on a subsequent storage failure, as machine
	// cross-repository quotas already are, and are intentionally reset on restart.
	if !t.s.allowCross("console-send:"+t.console.ID, 1, t.s.clock.Now()) {
		return nil, fail("rate_limited", "console sends are limited to 120 a minute per account; retry later")
	}
	if cross > 0 && !t.s.allowCross("console-cross:"+t.console.ID, cross, t.s.clock.Now()) {
		return nil, crossRefused()
	}
	e, err := t.operatorSender(repo)
	if err != nil {
		return nil, err
	}
	if _, err = t.exec(`INSERT INTO Message(id,sender_execution_id,subject,body,created_ms,repository_id,thread_id,reply_to,sender_account_id,sender_username,sender_display_name)
 VALUES(?,?,?,?,?,?,?,NULLIF(?,''),?,?,?)`, message, e.ID, r.Subject, r.Body, t.s.clock.Now().UnixMilli(), repo, thread, r.ReplyTo, t.console.ID, t.console.Username, t.console.DisplayName); err != nil {
		return nil, err
	}
	result := consoleSendResult{MessageID: message, ThreadID: thread, Deliveries: []consoleDeliveryResult{}}
	for _, a := range recipients {
		d := consoleDeliveryResult{ID: id("dl_"), Recipient: a.name, RepositoryID: a.repo}
		if _, err = t.exec("INSERT INTO Delivery(id,message_id,recipient_agent_id,attention) VALUES(?,?,?,1)", d.ID, message, a.id); err != nil {
			return nil, err
		}
		if err = t.event(a.id, "", "inbox_changed", d.ID); err != nil {
			return nil, err
		}
		if a.name != "human" {
			live, err := t.reachable(a.id)
			if err != nil {
				return nil, err
			}
			d.Queued = !live
		}
		result.Deliveries = append(result.Deliveries, d)
	}
	if err = t.rememberConsoleOperation(t.console.ID, r.Key, operation, request, result); err != nil {
		return nil, err
	}
	return consoleSendProjection(result, repo, t.console.ID, t.console.Username, t.console.DisplayName), nil
}

func consoleSendProjection(result consoleSendResult, repo, account, username, display string) map[string]any {
	deliveries := []map[string]any{}
	for _, d := range result.Deliveries {
		deliveries = append(deliveries, map[string]any{"id": d.ID, "recipient": d.Recipient, "repository_id": d.RepositoryID, "queued": d.Queued})
	}
	out := map[string]any{"message_id": result.MessageID, "thread_id": result.ThreadID, "deliveries": deliveries, "reply_address": "human:" + repo}
	messageAttribution(account, username, display, out)
	return out
}

func (t *transaction) consoleReplay(saved *consoleOperation) (map[string]any, error) {
	var repo, account, username, display string
	if err := t.row(`SELECT repository_id,COALESCE(sender_account_id,''),COALESCE(sender_username,''),COALESCE(sender_display_name,'') FROM Message WHERE id=? AND sender_account_id=?`, saved.MessageID, t.console.ID).Scan(&repo, &account, &username, &display); err != nil {
		return nil, missing(err, "unknown accepted message")
	}
	if err := t.consoleWriteRepository(repo); err != nil {
		return nil, err
	}
	// Re-read current destinations without re-resolving request addresses. Repair
	// may move a mailbox; contact/name changes may never redirect an exact retry.
	for _, recipient := range saved.RecipientIDs {
		var currentRepo, client string
		if err := t.row("SELECT repository_id,client FROM Agent WHERE id=?", recipient).Scan(&currentRepo, &client); err != nil {
			return nil, missing(err, "unknown accepted recipient")
		}
		if t.console.Role == "member" && client == "human" {
			return nil, fail("not_found", "unknown accepted recipient")
		}
		if err := t.consoleWriteRepository(currentRepo); err != nil {
			return nil, err
		}
	}
	for i := range saved.Result.Deliveries {
		d := &saved.Result.Deliveries[i]
		var client string
		if err := t.row(`SELECT CASE WHEN a.client='human' THEN 'human' ELSE a.name END,a.repository_id,a.client FROM Delivery d JOIN Agent a ON a.id=d.recipient_agent_id WHERE d.id=? AND d.message_id=?`, d.ID, saved.MessageID).Scan(&d.Recipient, &d.RepositoryID, &client); err != nil {
			return nil, missing(err, "unknown accepted delivery")
		}
		if t.console.Role == "member" && client == "human" {
			return nil, fail("not_found", "unknown accepted delivery")
		}
		if err := t.consoleWriteRepository(d.RepositoryID); err != nil {
			return nil, err
		}
	}
	return consoleSendProjection(saved.Result, repo, account, username, display), nil
}

// The nullable columns are coalesced at query boundaries, keeping legacy mail
// unattributed while the helper also serves paged body projections.
func attributedBody(r Request, idField, sender, body, repo, account, username, display string) (map[string]any, error) {
	out, err := bodyPart(r, idField, sender, body)
	if err != nil {
		return nil, err
	}
	messageAttribution(account, username, display, out)
	if sender == "human" {
		out["reply_address"] = "human:" + repo
	}
	return out, nil
}
