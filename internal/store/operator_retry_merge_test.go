package store

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestOperatorLostResponseRetryAfterBothHumanMailboxesMerge(t *testing.T) {
	s, _, _ := testStore(t)
	a, ar, _, br := mergeSplit(t, s)
	from, into := br["repository_id"].(string), ar["repository_id"].(string)
	// Both repositories already have operator mailboxes. The request comes from
	// the whole-hub console, so its sender repository is inferred from To.
	historySend(t, s, a, "human", "destination-human", "", "destination", "existing human mailbox")
	request := Request{Key: "lost-response", To: []string{br["agent"].(map[string]any)["name"].(string)}, Body: "send this exactly once"}
	accepted := call(t, s, "ui_messages", request)
	before := count(t, s, "SELECT COUNT(*) FROM Message")
	call(t, s, "merge", mergeRequest(from, into, false))
	snapshot := databaseState(t, s)
	retried := call(t, s, "ui_messages", request)
	firstJSON, _ := json.Marshal(accepted)
	retryJSON, _ := json.Marshal(retried)
	if string(firstJSON) != string(retryJSON) || count(t, s, "SELECT COUNT(*) FROM Message") != before {
		t.Fatalf("lost-response retry sent twice or changed the saved result: %s versus %s", firstJSON, retryJSON)
	}
	if databaseState(t, s) != snapshot {
		t.Fatal("saved retry created a new operator identity or changed history")
	}
}

func TestOperatorRetryKeyCollisionsAfterMerge(t *testing.T) {
	s, _, _ := testStore(t)
	_, ar, _, br := mergeSplit(t, s)
	from, into := br["repository_id"].(string), ar["repository_id"].(string)
	requests := []Request{
		{Key: "same-key", To: []string{ar["agent"].(map[string]any)["name"].(string)}, Body: "destination operation"},
		{Key: "same-key", To: []string{br["agent"].(map[string]any)["name"].(string)}, Body: "source operation"},
	}
	accepted := []map[string]any{}
	for _, request := range requests {
		accepted = append(accepted, call(t, s, "ui_messages", request))
	}
	call(t, s, "merge", mergeRequest(from, into, false))
	before := count(t, s, "SELECT COUNT(*) FROM Message")
	for i, request := range requests {
		result := call(t, s, "ui_messages", request)
		firstJSON, _ := json.Marshal(accepted[i])
		retryJSON, _ := json.Marshal(result)
		if string(firstJSON) != string(retryJSON) {
			t.Fatalf("collision returned another mailbox's operation: %s versus %s", firstJSON, retryJSON)
		}
	}
	changed := requests[1]
	changed.Body = "changed request has no matching prior operation"
	expectError(t, s, "ui_messages", changed, "key_reused")
	if count(t, s, "SELECT COUNT(*) FROM Message") != before {
		t.Fatal("retry collision sent a message")
	}
}

func TestOperatorRetryAfterReplyRepositoryMoves(t *testing.T) {
	s, _, _ := testStore(t)
	a, ar, b, br := mergeSplit(t, s)
	from, into := br["repository_id"].(string), ar["repository_id"].(string)
	historySend(t, s, a, "human", "destination-human", "", "destination", "existing human mailbox")
	question := historySend(t, s, b, "human", "question", "", "source", "reply to this")
	request := Request{Key: "lost-reply", ReplyTo: question["message_id"].(string), To: []string{br["agent"].(map[string]any)["name"].(string)}, Body: "operator reply accepted before repair"}
	accepted := call(t, s, "ui_messages", request)
	call(t, s, "merge", mergeRequest(from, into, false))
	before := databaseState(t, s)
	retry := call(t, s, "ui_messages", request)
	if retry["message_id"] != accepted["message_id"] || databaseState(t, s) != before {
		t.Fatal("reply-derived retry created another message", retry)
	}
}

func TestOperatorRetryFindsActualOriginAcrossMergeChain(t *testing.T) {
	s, _, _ := testStore(t)
	_, ar, _, br := mergeSplit(t, s)
	from, middle := br["repository_id"].(string), ar["repository_id"].(string)
	name := br["agent"].(map[string]any)["name"].(string)
	call(t, s, "ui_messages", Request{RepositoryID: from, Key: "first-origin", To: []string{name}, Body: "creates source human"})
	call(t, s, "merge", mergeRequest(from, middle, false))
	request := Request{RepositoryID: middle, Key: "chain-retry", To: []string{name}, Body: "accepted in the middle repository"}
	accepted := call(t, s, "ui_messages", request)
	third := call(t, s, "sessions", Request{Client: "codex", Host: "third", SessionID: "third", Cwd: "/third", RemoteURL: "https://third.example/iris.git", RootCommit: "root"})
	into := third["repository_id"].(string)
	thirdExecution := Request{ExecutionID: third["execution_id"].(string), Generation: third["generation"].(int64)}
	historySend(t, s, thirdExecution, "human", "third-human", "", "third", "existing destination human")
	call(t, s, "merge", mergeRequest(middle, into, false))
	if count(t, s, "SELECT COUNT(*) FROM Agent WHERE id=?", "human_"+middle) != 0 {
		t.Fatal("fixture accidentally created a canonical middle human")
	}
	before := databaseState(t, s)
	retry := call(t, s, "ui_messages", request)
	if retry["message_id"] != accepted["message_id"] || databaseState(t, s) != before {
		t.Fatal("retired repository lost its actual operator origin", retry)
	}
	fresh := request
	fresh.Key = "never-accepted"
	expectError(t, s, "ui_messages", fresh, "not_found")
	changed := request
	changed.Body = "different payload under the accepted key"
	expectError(t, s, "ui_messages", changed, "key_reused")
	if databaseState(t, s) != before {
		t.Fatal("refused retired-origin write changed records")
	}
}

func TestOperatorRetryRefusesMultipleExactOrigins(t *testing.T) {
	s, _, _ := testStore(t)
	_, ar, _, br := mergeSplit(t, s)
	from, into := br["repository_id"].(string), ar["repository_id"].(string)
	request := Request{Key: "old-ambiguous", To: []string{br["agent"].(map[string]any)["name"].(string)}, Body: "accepted before the bug was repaired"}
	call(t, s, "ui_messages", request)
	other := call(t, s, "ui_messages", Request{Key: "other", To: []string{ar["agent"].(map[string]any)["name"].(string)}, Body: "different old result"})
	otherJSON, _ := json.Marshal(other)
	// A hub affected by the original retry bug may already have accepted the
	// same request under both principals. Neither old result can be guessed.
	if _, err := s.db.Exec(`INSERT INTO Operation(principal_agent_id,key,request_hash,result_json,created_ms)
 SELECT ?,key,request_hash,?,created_ms FROM Operation WHERE principal_agent_id=? AND key=?`, "human_"+into, string(otherJSON), "human_"+from, request.Key); err != nil {
		t.Fatal(err)
	}
	call(t, s, "merge", mergeRequest(from, into, false))
	before := databaseState(t, s)
	expectError(t, s, "ui_messages", request, "key_reused")
	if databaseState(t, s) != before {
		t.Fatal("ambiguous retry selected or created a result")
	}
}

func TestOperatorRetryIgnoresOtherHumanClientMailboxes(t *testing.T) {
	s, _, _ := testStore(t)
	_, ar, _, br := mergeSplit(t, s)
	from, into := br["repository_id"].(string), ar["repository_id"].(string)
	request := Request{Key: "not-a-human-origin", To: []string{br["agent"].(map[string]any)["name"].(string)}, Body: "only the real operator origin may answer"}
	accepted := call(t, s, "ui_messages", request)
	for _, tuple := range []struct{ id, host, client, worktree string }{
		{"fake-host", "agent-host", "human", ""},
		{"fake-worktree", "operator", "human", "/agent"},
		{"fake-client", "operator", "codex", ""},
	} {
		if _, err := s.db.Exec("INSERT INTO Agent VALUES(?,?,?,?,?,?,1)", tuple.id, tuple.id, into, tuple.host, tuple.client, tuple.worktree); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(`INSERT INTO Operation(principal_agent_id,key,request_hash,result_json,created_ms)
 SELECT ?,key,request_hash,result_json,created_ms FROM Operation WHERE principal_agent_id=? AND key=?`, tuple.id, "human_"+from, request.Key); err != nil {
			t.Fatal(err)
		}
	}
	call(t, s, "merge", mergeRequest(from, into, false))
	before := databaseState(t, s)
	retried := call(t, s, "ui_messages", request)
	if retried["message_id"] != accepted["message_id"] || databaseState(t, s) != before {
		t.Fatal("an arbitrary human client changed the operator retry namespace", retried)
	}
}

func TestOperatorRetryReportsMalformedSavedResult(t *testing.T) {
	s, _, _ := testStore(t)
	_, ar := session(t, s, 1)
	request := Request{Key: "saved-result", To: []string{ar["agent"].(map[string]any)["name"].(string)}, Body: "already persisted"}
	call(t, s, "ui_messages", request)
	if _, err := s.db.Exec("UPDATE Operation SET result_json='{' WHERE key=?", request.Key); err != nil {
		t.Fatal(err)
	}
	before := databaseState(t, s)
	_, err := s.Call(t.Context(), "ui_messages", request)
	var domain *Error
	if err == nil || errors.As(err, &domain) || databaseState(t, s) != before {
		t.Fatal("invalid persisted result became a new send or final domain refusal", err)
	}
}
