package store

import (
	"crypto/sha256"
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

func mergeRequest(from, into string, dry bool) Request {
	var r Request
	b, _ := json.Marshal(map[string]any{"from_repository_id": from, "into_repository_id": into, "dry_run": dry})
	_ = json.Unmarshal(b, &r)
	return r
}

// mergeSplit creates the old hub's split: the same remote and root have durable
// mailboxes in separate repositories. SQL supplies the historical ambiguity that
// corrected registration must not create automatically.
func mergeSplit(t *testing.T, s *Store) (Request, map[string]any, Request, map[string]any) {
	t.Helper()
	a, ar := session(t, s, 1)
	r := Request{Client: "codex", Host: "laptop", SessionID: "split", Cwd: "/work/iris", Worktree: "/work/iris", RootCommit: "root", RemoteURL: "https://split.example/iris.git"}
	br := call(t, s, "sessions", r)
	b := Request{ExecutionID: br["execution_id"].(string), Generation: br["generation"].(int64)}
	if _, err := s.db.Exec("INSERT INTO RepositoryAlias VALUES(?,?)", "remote:example.com/team/project", br["repository_id"]); err != nil {
		t.Fatal(err)
	}
	return a, ar, b, br
}

func TestMergeDryRunAndLosslessRepair(t *testing.T) {
	s, _, _ := testStore(t)
	a, ar, b, br := mergeSplit(t, s)
	from, into := br["repository_id"].(string), ar["repository_id"].(string)
	name := func(reg map[string]any) string { return reg["agent"].(map[string]any)["name"].(string) }
	first := historySend(t, s, b, name(ar), "split-mail", "", "before repair", "preserve this body")
	historySend(t, s, a, name(br), "return-mail", first["message_id"].(string), "reply", "preserve the reply")
	d := call(t, s, "inbox", a)["deliveries"].([]map[string]any)[0]
	ack := a
	ack.Key, ack.Attempt, ack.Handled = d["id"].(string), d["attempt"].(int64), true
	call(t, s, "ack", ack)
	call(t, s, "claim", claimRequest(a, "dest-held", "dest.go", "exact"))
	call(t, s, "claim", claimRequest(b, "source-held", "source.go", "exact"))
	snapshot := b
	snapshot.Kind, snapshot.Status = "before", map[string]string{"source.go": " M"}
	call(t, s, "reconcile", snapshot)
	events := b
	events.ConsumerID = "saved"
	call(t, s, "events", events)
	call(t, s, "contact", Request{RepositoryID: from, Agent: name(br)})
	before := databaseState(t, s)
	plan := call(t, s, "merge", mergeRequest(from, into, true))
	if databaseState(t, s) != before {
		t.Fatal("dry run changed the split database")
	}
	counts := plan["counts"].(map[string]int)
	if plan["dry_run"] != true || counts["Agent"] != 1 || counts["Execution"] != 1 || counts["Claim"] != 1 || counts["Message"] != 1 || plan["coalesced_aliases"] != 2 {
		t.Fatal(plan)
	}
	if plan["from_repository"].(map[string]any)["id"] != from || plan["into_repository"].(map[string]any)["root_commit"] != "root" {
		t.Fatal("plan does not describe the chosen identities", plan)
	}
	untouched := map[string]int{}
	for _, table := range []string{"Message", "Delivery", "Ack", "Operation", "ShellSnapshot", "Cursor", "Claim", "Execution", "Agent"} {
		untouched[table] = count(t, s, "SELECT COUNT(*) FROM "+table)
	}
	var priorState map[string]json.RawMessage
	_ = json.Unmarshal([]byte(before), &priorState)
	result := call(t, s, "merge", mergeRequest(from, into, false))
	result["dry_run"] = true
	if !reflect.DeepEqual(result, plan) {
		t.Fatalf("apply diverged from plan: %v versus %v", result, plan)
	}
	for table, want := range untouched {
		if got := count(t, s, "SELECT COUNT(*) FROM "+table); got != want {
			t.Fatalf("%s: %d rows, want %d", table, got, want)
		}
	}
	var afterState map[string]json.RawMessage
	_ = json.Unmarshal([]byte(databaseState(t, s)), &afterState)
	for _, table := range []string{"Delivery", "Ack", "Operation", "ShellSnapshot", "Cursor"} {
		if string(priorState[table]) != string(afterState[table]) {
			t.Fatalf("merge rewrote %s history", table)
		}
	}
	if count(t, s, "SELECT COUNT(*) FROM Repository WHERE id=?", from) != 0 || count(t, s, "SELECT COUNT(*) FROM Repository WHERE id=? AND contact_agent_id=?", into, br["agent"].(map[string]any)["id"]) != 1 {
		t.Fatal("source references or contact were not moved")
	}
	if got := call(t, s, "validate", b); got["repository_id"] != into || got["execution_id"] != b.ExecutionID || got["generation"] != b.Generation {
		t.Fatal("Execution identity changed", got)
	}
	if got := call(t, s, "agents", Request{RepositoryID: into})["agents"].([]map[string]any); len(got) != 2 {
		t.Fatal("hosts do not share peers", got)
	}
	expectError(t, s, "claim", claimRequest(a, "cross-host", "source.go", "exact"), "conflict")
	for _, who := range []Request{a, b} {
		who.Key = first["message_id"].(string)
		if got := call(t, s, "read_message_body", who); got["body"] != "preserve this body" {
			t.Fatal("mail changed", got)
		}
		if got := call(t, s, "history", who)["messages"].([]map[string]any); len(got) != 2 {
			t.Fatal("history lost", got)
		}
	}
	if _, err := s.db.Exec("INSERT INTO MessageSearch(MessageSearch) VALUES('integrity-check')"); err != nil {
		t.Fatal("search history changed", err)
	}
}

func TestMergeDryRunDoesNotExpireSessions(t *testing.T) {
	s, clock, _ := testStore(t)
	_, ar, b, br := mergeSplit(t, s)
	call(t, s, "claim", claimRequest(b, "old", "expired.go", "exact"))
	clock.advance(time.Hour)
	before := databaseState(t, s)
	call(t, s, "merge", mergeRequest(br["repository_id"].(string), ar["repository_id"].(string), true))
	if databaseState(t, s) != before {
		t.Fatal("dry run expired sessions or claims")
	}
}

func TestMergeRefusesLiveClaimCollisions(t *testing.T) {
	s, _, _ := testStore(t)
	a, ar, b, br := mergeSplit(t, s)
	call(t, s, "claim", claimRequest(a, "held", "src", "prefix"))
	call(t, s, "claim", claimRequest(b, "held", "SRC/file.go", "exact"))
	before := databaseState(t, s)
	for _, dry := range []bool{true, false} {
		expectError(t, s, "merge", mergeRequest(br["repository_id"].(string), ar["repository_id"].(string), dry), "conflict")
		if databaseState(t, s) != before {
			t.Fatal("claim refusal changed records")
		}
	}
	call(t, s, "release", Request{ExecutionID: b.ExecutionID, Generation: b.Generation, All: true})
	call(t, s, "merge", mergeRequest(br["repository_id"].(string), ar["repository_id"].(string), false))
}

func TestMergeNeedsKnownDistinctRepositories(t *testing.T) {
	s, _, _ := testStore(t)
	_, ar := session(t, s, 1)
	repo := ar["repository_id"].(string)
	for _, r := range []Request{mergeRequest("", repo, true), mergeRequest(repo, "", true), mergeRequest(repo, repo, true)} {
		expectError(t, s, "merge", r, "bad_request")
	}
	for _, r := range []Request{mergeRequest("missing", repo, true), mergeRequest(repo, "missing", true)} {
		expectError(t, s, "merge", r, "not_found")
	}
}

func TestMergePreservesNamesAndAllocatesFreeSlots(t *testing.T) {
	s, _, _ := testStore(t)
	_, ar, _, br := mergeSplit(t, s)
	from, into := br["repository_id"].(string), ar["repository_id"].(string)
	second := call(t, s, "sessions", Request{Client: "codex", Host: "laptop", SessionID: "split-second", Cwd: "/work/iris", Worktree: "/work/iris", RootCommit: "root", RemoteURL: "https://split.example/iris.git"})
	// A split may contain the same mailbox tuple in both repositories. Both
	// durable mailboxes and their distinct globally unique names must survive.
	if _, err := s.db.Exec("UPDATE Agent SET host='host',worktree='/repo/w1' WHERE repository_id=?", from); err != nil {
		t.Fatal(err)
	}
	plan := call(t, s, "merge", mergeRequest(from, into, true))
	changes := plan["slot_changes"].([]map[string]any)
	if len(changes) != 1 || changes[0]["agent_id"] != br["agent"].(map[string]any)["id"] || changes[0]["from_slot"] != 1 || changes[0]["into_slot"] != 3 {
		t.Fatal("did not reserve the already-free source slot", plan)
	}
	call(t, s, "merge", mergeRequest(from, into, false))
	for _, reg := range []map[string]any{ar, br, second} {
		a := reg["agent"].(map[string]any)
		if count(t, s, "SELECT COUNT(*) FROM Agent WHERE id=? AND name=? AND repository_id=?", a["id"], a["name"], into) != 1 {
			t.Fatal("mailbox was renamed or combined", a)
		}
	}
	if count(t, s, "SELECT COUNT(*) FROM Agent WHERE repository_id=? AND slot IN (1,2,3)", into) != 3 {
		t.Fatal("slot assignment lost a mailbox")
	}
}

func TestMergeRefusesDifferentPoliciesAndContacts(t *testing.T) {
	s, _, _ := testStore(t)
	_, ar, _, br := mergeSplit(t, s)
	from, into := br["repository_id"].(string), ar["repository_id"].(string)
	if _, err := s.db.Exec("UPDATE Repository SET case_policy='sensitive' WHERE id=?", from); err != nil {
		t.Fatal(err)
	}
	expectError(t, s, "merge", mergeRequest(from, into, false), "conflict")
	if _, err := s.db.Exec("UPDATE Repository SET case_policy='folded',conflict_action='warn' WHERE id=?", from); err != nil {
		t.Fatal(err)
	}
	expectError(t, s, "merge", mergeRequest(from, into, false), "conflict")
	if _, err := s.db.Exec("UPDATE Repository SET conflict_action='deny' WHERE id=?", from); err != nil {
		t.Fatal(err)
	}
	call(t, s, "contact", Request{RepositoryID: into, Agent: ar["agent"].(map[string]any)["name"].(string)})
	call(t, s, "contact", Request{RepositoryID: from, Agent: br["agent"].(map[string]any)["name"].(string)})
	before := databaseState(t, s)
	expectError(t, s, "merge", mergeRequest(from, into, false), "conflict")
	if databaseState(t, s) != before {
		t.Fatal("contact refusal changed records")
	}
	call(t, s, "contact", Request{RepositoryID: from})
	plan := call(t, s, "merge", mergeRequest(from, into, false))
	if plan["contact_agent_id"] != ar["agent"].(map[string]any)["id"] {
		t.Fatal("destination contact was replaced", plan)
	}
}

func TestMergeKeepsPreUpgradeRetryResults(t *testing.T) {
	s, _, _ := testStore(t)
	_, ar, b, br := mergeSplit(t, s)
	request := b
	request.Key, request.To, request.Body = "legacy", []string{br["agent"].(map[string]any)["name"].(string)}, "self addressed before repair"
	sent := call(t, s, "messages", request)
	// Before this feature Request had no merge fields. Preserve the exact retry
	// hash that such a client operation left in the production database.
	raw, _ := json.Marshal(struct {
		Route   string
		Request Request
	}{"message", request})
	var old map[string]json.RawMessage
	_ = json.Unmarshal(raw, &old)
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(old["Request"], &fields)
	if _, exists := fields["from_repository_id"]; exists {
		t.Fatal("empty merge fields changed legacy request hashes")
	}
	hash := sha256.Sum256(raw)
	if _, err := s.db.Exec("UPDATE Operation SET request_hash=? WHERE principal_agent_id=? AND key=?", hash[:], br["agent"].(map[string]any)["id"], request.Key); err != nil {
		t.Fatal(err)
	}
	before := count(t, s, "SELECT COUNT(*) FROM Message")
	call(t, s, "merge", mergeRequest(br["repository_id"].(string), ar["repository_id"].(string), false))
	retried := call(t, s, "messages", request)
	firstJSON, _ := json.Marshal(sent)
	retryJSON, _ := json.Marshal(retried)
	if string(firstJSON) != string(retryJSON) || count(t, s, "SELECT COUNT(*) FROM Message") != before {
		t.Fatalf("retry changed delivery result or sent twice: %s versus %s", firstJSON, retryJSON)
	}
}

func TestMergeClaimLivenessWithoutExpiryWrites(t *testing.T) {
	for _, state := range []string{"recovering", "expired-claim", "expired-lease", "expired-recovery", "old-epoch", "ended", "related"} {
		t.Run(state, func(t *testing.T) {
			s, clock, _ := testStore(t)
			a, ar, b, br := mergeSplit(t, s)
			call(t, s, "claim", claimRequest(a, "held", "file.go", "exact"))
			call(t, s, "claim", claimRequest(b, "held", "file.go", "exact"))
			var err error
			switch state {
			case "recovering":
				_, err = s.db.Exec("UPDATE Execution SET boot_epoch=0,recovery_deadline=? WHERE id=?", clock.Now().Add(time.Minute).UnixMilli(), b.ExecutionID)
			case "expired-claim":
				_, err = s.db.Exec("UPDATE Claim SET expires_ms=? WHERE execution_id=?", clock.Now().UnixMilli()-1, b.ExecutionID)
			case "expired-lease":
				_, err = s.db.Exec("UPDATE Execution SET deadline_tick_ms=? WHERE id=?", clock.Tick(), b.ExecutionID)
			case "expired-recovery":
				_, err = s.db.Exec("UPDATE Execution SET recovery_deadline=? WHERE id=?", clock.Now().UnixMilli()-1, b.ExecutionID)
			case "old-epoch":
				_, err = s.db.Exec("UPDATE Execution SET boot_epoch=0 WHERE id=?", b.ExecutionID)
			case "ended":
				_, err = s.db.Exec("UPDATE Execution SET state='ended' WHERE id=?", b.ExecutionID)
			case "related":
				_, err = s.db.Exec("UPDATE Execution SET parent_id=? WHERE id=?", a.ExecutionID, b.ExecutionID)
			}
			if err != nil {
				t.Fatal(err)
			}
			from, into := br["repository_id"].(string), ar["repository_id"].(string)
			before := databaseState(t, s)
			if state == "recovering" {
				expectError(t, s, "merge", mergeRequest(from, into, true), "conflict")
			} else {
				call(t, s, "merge", mergeRequest(from, into, true))
			}
			if databaseState(t, s) != before {
				t.Fatal("liveness checks changed records")
			}
			if state != "recovering" {
				call(t, s, "merge", mergeRequest(from, into, false))
				if count(t, s, "SELECT COUNT(*) FROM Claim WHERE repository_id=? AND revoked_ms IS NULL", into) != 2 {
					t.Fatal("merge revoked or deleted historical claims")
				}
			}
		})
	}
}
