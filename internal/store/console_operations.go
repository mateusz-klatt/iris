package store

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
)

// These storage contracts are deliberately absent from Call and the HTTP router.
// The authorization step must check current account/object authority before using
// a saved result. In particular, finding an operation is never permission to send.
type consoleMessageRequest struct {
	RepositoryID string   `json:"repository_id"`
	To           []string `json:"to"`
	Subject      string   `json:"subject"`
	Body         string   `json:"body"`
	ReplyTo      string   `json:"reply_to"`
}

type consoleDeliveryResult struct {
	ID           string `json:"id"`
	Recipient    string `json:"recipient"`
	RepositoryID string `json:"repository_id"`
	Queued       bool   `json:"queued"`
}

type consoleSendResult struct {
	MessageID  string                  `json:"message_id"`
	ThreadID   string                  `json:"thread_id"`
	Deliveries []consoleDeliveryResult `json:"deliveries"`
}

type consoleOperation struct {
	OriginRepositoryID string
	MessageID          string
	RecipientIDs       []string
	Result             consoleSendResult
	CreatedMS          int64
	Tombstone          bool
}

func consoleFingerprint(operation string, request consoleMessageRequest) [32]byte {
	raw, _ := json.Marshal(struct {
		Operation string
		Request   consoleMessageRequest
	}{operation, request}) // only strings and string slices
	return sha256.Sum256(raw)
}

// consoleOperation reads only the account namespace, never the legacy shared
// Operation cache. Original origin/recipient IDs are evidence for later current-
// object authorization; retired IDs do not become valid origins for new sends.
func (t *transaction) consoleOperation(account, key, operation string, request consoleMessageRequest) (*consoleOperation, error) {
	if account == "" || key == "" || operation == "" {
		return nil, fail("bad_request", "account, operation and retry key are required")
	}
	var saved consoleOperation
	var hash []byte
	var message, result sql.NullString
	var recipients string
	err := t.row(`SELECT request_hash,origin_repository_id,message_id,result_json,recipient_ids_json,created_ms,tombstone
 FROM ConsoleOperation WHERE account_id=? AND key=?`, account, key).Scan(&hash, &saved.OriginRepositoryID, &message, &result, &recipients, &saved.CreatedMS, &saved.Tombstone)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	want := consoleFingerprint(operation, request)
	if !bytes.Equal(hash, want[:]) {
		return nil, fail("key_reused", "retry key was used for a different console request")
	}
	saved.MessageID = message.String
	if err = json.Unmarshal([]byte(recipients), &saved.RecipientIDs); err != nil {
		return nil, err
	}
	if saved.Tombstone || !message.Valid || !result.Valid {
		return nil, fail("not_found", "the accepted console result is no longer available")
	}
	if err = json.Unmarshal([]byte(result.String), &saved.Result); err != nil {
		return nil, err
	}
	return &saved, nil
}

// rememberConsoleOperation belongs in the same transaction as the authored
// message/deliveries. Resolve origin and recipients from those accepted records,
// rather than trusting addresses or recording a legacy message as someone's work.
// result is the hub-owned send result from that transaction, never a wire input.
func (t *transaction) rememberConsoleOperation(account, key, operation string, request consoleMessageRequest, result consoleSendResult) error {
	if account == "" || key == "" || operation == "" {
		return fail("bad_request", "account, operation and retry key are required")
	}
	hash := consoleFingerprint(operation, request)
	raw, _ := json.Marshal(result) // hub-owned strings, booleans and slices
	res, err := t.exec(`INSERT INTO ConsoleOperation(account_id,key,request_hash,origin_repository_id,message_id,result_json,recipient_ids_json,created_ms)
 SELECT ?,?,?,m.repository_id,m.id,?,
 (SELECT json_group_array(recipient_agent_id) FROM (SELECT recipient_agent_id FROM Delivery WHERE message_id=m.id ORDER BY recipient_agent_id)),?
 FROM Message m WHERE m.id=? AND m.sender_account_id=? AND EXISTS(SELECT 1 FROM Delivery WHERE message_id=m.id)`, account, key, hash[:], string(raw), t.s.clock.Now().UnixMilli(), result.MessageID, account)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fail("conflict", "console retries require the account's accepted message and deliveries")
	}
	return nil
}
