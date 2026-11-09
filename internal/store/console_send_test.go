package store

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

func namedSend(s *Store, account ConsoleAccount, r Request) (map[string]any, error) {
	var out map[string]any
	err := s.transact(context.Background(), func(tx *transaction) error {
		tx.console = &account
		var err error
		out, err = tx.consoleSend(r)
		return err
	})
	return out, err
}

func mustNamedSend(t *testing.T, s *Store, account ConsoleAccount, r Request) map[string]any {
	t.Helper()
	out, err := namedSend(s, account, r)
	if err != nil {
		t.Fatal("named send:", err)
	}
	return out
}

func grantNamed(t *testing.T, s *Store, a ConsoleAccount, repo, mode string) {
	t.Helper()
	if _, err := s.db.Exec(`INSERT INTO ConsoleRepositoryAccess VALUES(?,?,?) ON CONFLICT(account_id,repository_id) DO UPDATE SET mode=excluded.mode`, a.ID, repo, mode); err != nil {
		t.Fatal(err)
	}
}

func TestNamedSendRequiresEveryWritableRepositoryAndImmutableAgentAddresses(t *testing.T) {
	s, _, aout, _, bout := crossRepo(t)
	aRepo, bRepo := aout["repository_id"].(string), bout["repository_id"].(string)
	aID := aout["agent"].(map[string]any)["id"].(string)
	bID := bout["agent"].(map[string]any)["id"].(string)
	a := operationAccount(t, s, "Member")
	r := Request{RepositoryID: aRepo, Key: "reviewed", To: []string{aID, bID}, Subject: "subject", Body: "body"}
	for _, state := range []struct{ origin, target string }{{"", ""}, {"ro", "rw"}, {"rw", ""}, {"rw", "ro"}} {
		if _, err := s.db.Exec("DELETE FROM ConsoleRepositoryAccess WHERE account_id=?", a.ID); err != nil {
			t.Fatal(err)
		}
		if state.origin != "" {
			grantNamed(t, s, a, aRepo, state.origin)
		}
		if state.target != "" {
			grantNamed(t, s, a, bRepo, state.target)
		}
		_, err := namedSend(s, a, r)
		consoleError(t, err, "not_found")
		if rowCount(t, s, "SELECT COUNT(*) FROM Message") != 0 || rowCount(t, s, "SELECT COUNT(*) FROM Delivery") != 0 {
			t.Fatal("partly unauthorized batch delivered mail")
		}
	}
	grantNamed(t, s, a, aRepo, "rw")
	grantNamed(t, s, a, bRepo, "rw")
	for _, address := range []string{agentName(aout), "human", "human:" + aRepo, "repo:" + bRepo, "ag_guessed"} {
		bad := r
		bad.To = []string{aID, address}
		_, err := namedSend(s, a, bad)
		consoleError(t, err, "not_found")
	}
	missingOrigin := r
	missingOrigin.RepositoryID = ""
	_, err := namedSend(s, a, missingOrigin)
	consoleError(t, err, "bad_request")
	accepted := mustNamedSend(t, s, a, r)
	if accepted["sender_account_id"] != a.ID || accepted["sender_username"] != a.Username || accepted["sender_display_name"] != a.DisplayName || accepted["reply_address"] != "human:"+aRepo || len(accepted["deliveries"].([]map[string]any)) != 2 {
		t.Fatal("missing named author or accepted recipients")
	}
	if rowCount(t, s, "SELECT COUNT(*) FROM Operation") != 0 || rowCount(t, s, "SELECT COUNT(*) FROM ConsoleOperation") != 1 {
		t.Fatal("named send used shared retry namespace")
	}
	// Repeated recipients remain one delivery, matching machine batch semantics.
	r.Key, r.To = "duplicate", []string{bID, bID}
	if len(mustNamedSend(t, s, a, r)["deliveries"].([]map[string]any)) != 1 {
		t.Fatal("duplicate delivery")
	}
}

func TestNamedNewSendsRequireWritableOriginIndependentlyOfRecipients(t *testing.T) {
	for _, operation := range []string{"send", "reply"} {
		for _, originMode := range []string{"none", "ro"} {
			t.Run(operation+"/"+originMode, func(t *testing.T) {
				s, ar, aout, _, bout := crossRepo(t)
				aRepo, bRepo := aout["repository_id"].(string), bout["repository_id"].(string)
				bID := bout["agent"].(map[string]any)["id"].(string)
				a := operationAccount(t, s, "Member")
				grantNamed(t, s, a, bRepo, "rw")
				if originMode == "ro" {
					grantNamed(t, s, a, aRepo, "ro")
				}
				r := Request{RepositoryID: aRepo, Key: "fresh-" + operation, To: []string{bID}, Body: "reviewed message"}
				if operation == "reply" {
					// A parent sent from A to B is visible in the selected origin
					// and through the member's B grant. Parent visibility must not
					// conceal a missing origin-write check either.
					parent := call(t, s, "messages", sendTo(ar, "parent", "visible parent", bID))
					r.ReplyTo = parent["message_id"].(string)
				}
				principal := consoleTestPrincipal(t, s, a)
				before := databaseState(t, s)
				_, err := s.CallConsole(t.Context(), principal, "ui_messages", r)
				var domain *Error
				if !errors.As(err, &domain) || domain.Code != "not_found" {
					t.Errorf("%s without rw on its origin must be refused: %v", operation, err)
				}
				if databaseState(t, s) != before {
					t.Error("refused origin wrote mail, retry records or other store state")
				}
				if t.Failed() {
					return
				}
				// The same request succeeds with rw on A, proving the origin
				// grant was the only missing authority and refusal saved no retry.
				grantNamed(t, s, a, aRepo, "rw")
				accepted := consoleCall(t, s, principal, "ui_messages", r)
				if len(accepted["deliveries"].([]map[string]any)) != 1 {
					t.Fatal("authorized cross-repository request did not deliver once")
				}
			})
		}
	}
}

func TestNamedReplyRequiresTheParentInItsSelectedOriginAndNeverAcknowledges(t *testing.T) {
	s, ar, aout, br, bout := crossRepo(t)
	aRepo, bRepo := aout["repository_id"].(string), bout["repository_id"].(string)
	aID := aout["agent"].(map[string]any)["id"].(string)
	bID := bout["agent"].(map[string]any)["id"].(string)
	a := operationAccount(t, s, "Replying")
	grantNamed(t, s, a, aRepo, "rw")
	grantNamed(t, s, a, bRepo, "rw")
	parent := call(t, s, "messages", sendTo(br, "parent", "visible only in B", "human"))
	r := Request{RepositoryID: aRepo, Key: "reply", To: []string{bID}, ReplyTo: parent["message_id"].(string), Body: "reply"}
	_, err := namedSend(s, a, r)
	consoleError(t, err, "not_found")
	r.RepositoryID = bRepo
	accepted := mustNamedSend(t, s, a, r)
	if accepted["thread_id"] != parent["thread_id"] || rowCount(t, s, "SELECT COUNT(*) FROM Ack") != 0 {
		t.Fatal("reply changed thread or acknowledged shared mail")
	}
	// Thread membership is insufficient: a new B-only reply stays hidden in A.
	cross := call(t, s, "messages", sendTo(ar, "cross", "cross", bID))
	hidden := sendTo(br, "hidden", "hidden B tail", "human")
	hidden.ReplyTo = cross["message_id"].(string)
	hiddenID := call(t, s, "messages", hidden)["message_id"].(string)
	r.Key, r.RepositoryID, r.To, r.ReplyTo = "hidden-parent", aRepo, []string{aID}, hiddenID
	_, err = namedSend(s, a, r)
	consoleError(t, err, "not_found")
	// An accepted reply result remains replayable after its parent is removed.
	if _, err := s.db.Exec("DELETE FROM Delivery WHERE message_id=?", parent["message_id"]); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("DELETE FROM Message WHERE id=?", parent["message_id"]); err != nil {
		t.Fatal(err)
	}
	r = Request{RepositoryID: bRepo, Key: "reply", To: []string{bID}, ReplyTo: parent["message_id"].(string), Body: "reply"}
	if !reflect.DeepEqual(mustNamedSend(t, s, a, r), accepted) {
		t.Fatal("parent deletion changed exact retry")
	}
	r.Key = "new-reply"
	_, err = namedSend(s, a, r)
	consoleError(t, err, "not_found")
}

func TestNamedRetriesAreAccountScopedAndRecheckCurrentGrants(t *testing.T) {
	s, _, aout, _, bout := crossRepo(t)
	aRepo, bRepo := aout["repository_id"].(string), bout["repository_id"].(string)
	bID := bout["agent"].(map[string]any)["id"].(string)
	a, b := operationAccount(t, s, "First"), operationAccount(t, s, "Second")
	for _, account := range []ConsoleAccount{a, b} {
		grantNamed(t, s, account, aRepo, "rw")
		grantNamed(t, s, account, bRepo, "rw")
	}
	r := Request{RepositoryID: aRepo, Key: "same", To: []string{bID}, Body: "accepted"}
	first := mustNamedSend(t, s, a, r)
	second := mustNamedSend(t, s, b, r)
	if first["message_id"] == second["message_id"] {
		t.Fatal("accounts shared retry identity")
	}
	for _, changed := range []Request{
		{RepositoryID: aRepo, Key: "same", To: []string{bID}, Body: "changed"},
		{RepositoryID: aRepo, Key: "same", To: []string{bID, bID}, Body: "accepted"},
		{RepositoryID: bRepo, Key: "same", To: []string{bID}, Body: "accepted"},
		{RepositoryID: aRepo, Key: "same", To: []string{bID}, Body: "accepted", ReplyTo: first["message_id"].(string)},
	} {
		_, err := namedSend(s, a, changed)
		consoleError(t, err, "key_reused")
	}
	for _, repo := range []string{aRepo, bRepo} {
		grantNamed(t, s, a, repo, "ro")
		_, err := namedSend(s, a, r)
		consoleError(t, err, "not_found")
		grantNamed(t, s, a, repo, "rw")
		if !reflect.DeepEqual(first, mustNamedSend(t, s, a, r)) {
			t.Fatal("regrant changed accepted result")
		}
		if _, err := s.db.Exec("DELETE FROM ConsoleRepositoryAccess WHERE account_id=? AND repository_id=?", a.ID, repo); err != nil {
			t.Fatal(err)
		}
		_, err = namedSend(s, a, r)
		consoleError(t, err, "not_found")
		grantNamed(t, s, a, repo, "rw")
	}
	if rowCount(t, s, "SELECT COUNT(*) FROM Message") != 2 || rowCount(t, s, "SELECT COUNT(*) FROM Delivery") != 2 {
		t.Fatal("retries duplicated mail")
	}
	// At-send snapshots survive profile edits and a session/account object refresh.
	updated, err := s.UpdateConsoleProfile(context.Background(), a.ID, 1, "Changed", "pl", "en")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, mustNamedSend(t, s, updated, r)) {
		t.Fatal("profile edit rewrote accepted authorship")
	}
}

func TestNamedSendBoundsAndOperatorOriginInference(t *testing.T) {
	s, _, _ := testStore(t)
	_, ar := session(t, s, 1)
	repo := ar["repository_id"].(string)
	operator := createTestConsoleAccount(t, s, "Operator", "operator")
	r := Request{Key: "inferred", To: []string{agentName(ar)}, Body: "accepted"}
	out := mustNamedSend(t, s, operator, r)
	if out["reply_address"] != "human:"+repo {
		t.Fatal("operator origin inference failed")
	}
	parent := out["message_id"].(string)
	r.Key, r.ReplyTo = "inferred-reply", parent
	mustNamedSend(t, s, operator, r)
	for _, tc := range []struct {
		r    Request
		code string
	}{
		{Request{Key: "none"}, "bad_request"},
		{Request{Key: "empty-to", RepositoryID: repo}, "bad_request"},
		{Request{Key: "many", RepositoryID: repo, To: make([]string, 101)}, "bad_request"},
		{Request{Key: "large-body", To: r.To, Body: strings.Repeat("x", 16385)}, "too_large"},
		{Request{Key: "large-subject", To: r.To, Subject: strings.Repeat("x", 1025)}, "too_large"},
		{Request{Key: "no-origin", To: []string{"missing"}}, "not_found"},
		{Request{Key: "unknown-recipient", RepositoryID: repo, To: []string{"missing"}}, "not_found"},
		{Request{Key: "unknown-reply", To: r.To, ReplyTo: "missing"}, "not_found"},
		{Request{Key: "empty-origin", To: []string{"repo:missing"}}, "not_found"},
		{Request{Key: "gone-origin", RepositoryID: "retired", To: r.To}, "not_found"},
		{Request{RepositoryID: repo, To: r.To}, "bad_request"},
	} {
		_, err := namedSend(s, operator, tc.r)
		consoleError(t, err, tc.code)
	}
	human := mustNamedSend(t, s, operator, Request{Key: "human", RepositoryID: repo, To: []string{"human"}, Body: "shared"})
	if human["deliveries"].([]map[string]any)[0]["recipient"] != "human" {
		t.Fatal("operator shared routing changed")
	}
	// A demoted account cannot replay a former operator send to a human mailbox.
	member := operator
	member.Role = "member"
	grantNamed(t, s, member, repo, "rw")
	_, err := namedSend(s, member, Request{Key: "human", RepositoryID: repo, To: []string{"human"}, Body: "shared"})
	consoleError(t, err, "not_found")
}

func TestNamedSendsUseAccountQuotasAndDoNotChargeExactRetries(t *testing.T) {
	s, _, ar, _, br := crossRepo(t)
	aRepo, bRepo := ar["repository_id"].(string), br["repository_id"].(string)
	aID := ar["agent"].(map[string]any)["id"].(string)
	bID := br["agent"].(map[string]any)["id"].(string)
	a, b := operationAccount(t, s, "Quota A"), operationAccount(t, s, "Quota B")
	for _, account := range []ConsoleAccount{a, b} {
		grantNamed(t, s, account, aRepo, "rw")
		grantNamed(t, s, account, bRepo, "rw")
	}
	r := Request{RepositoryID: aRepo, Key: "first", To: []string{aID}, Body: "quota"}
	first := mustNamedSend(t, s, a, r)
	for i := 0; i < crossLimit; i++ {
		if !reflect.DeepEqual(mustNamedSend(t, s, a, r), first) {
			t.Fatal("exact replay changed")
		}
	}
	s.cross["console-send:"+a.ID] = crossWindow{start: s.clock.Now(), count: crossLimit}
	r.Key = "limited"
	_, err := namedSend(s, a, r)
	consoleError(t, err, "rate_limited")
	mustNamedSend(t, s, b, r)
	s.cross["console-send:"+a.ID] = crossWindow{start: s.clock.Now(), count: 0}
	s.cross["console-cross:"+a.ID] = crossWindow{start: s.clock.Now(), count: crossLimit}
	r.To = []string{bID}
	_, err = namedSend(s, a, r)
	consoleError(t, err, "rate_limited")
	if rowCount(t, s, "SELECT COUNT(*) FROM Message") != 2 {
		t.Fatal("quota refusal delivered")
	}
}

func TestNamedSendAndReplayStorageFailuresRemainAtomic(t *testing.T) {
	for _, replay := range []bool{false, true} {
		t.Run(fmt.Sprint(replay), func(t *testing.T) {
			s, _, file := testStore(t)
			_, ar := session(t, s, 1)
			repo := ar["repository_id"].(string)
			a := operationAccount(t, s, "Fault")
			grantNamed(t, s, a, repo, "rw")
			r := Request{RepositoryID: repo, Key: "fault", To: []string{ar["agent"].(map[string]any)["id"].(string)}, Body: "atomic"}
			if replay {
				mustNamedSend(t, s, a, r)
			}
			fault := installFaultDriver(t, s, file)
			invoke := func() error { _, err := namedSend(s, a, r); return err }
			fault.discover = true
			if err := invoke(); !errors.Is(err, errStorageFault) {
				t.Fatal("discovery", err)
			}
			trace := append([]string(nil), fault.trace...)
			fault.discover = false
			before := databaseState(t, s)
			for i := range trace {
				fault.count, fault.at, fault.trace = 0, i+1, nil
				err := invoke()
				fault.at = 0
				if !errors.Is(err, errStorageFault) {
					t.Fatalf("boundary %d: %v", i+1, err)
				}
				if databaseState(t, s) != before {
					t.Fatalf("boundary %d changed state", i+1)
				}
			}
			if err := invoke(); err != nil {
				t.Fatal("retry", err)
			}
			if rowCount(t, s, "SELECT COUNT(*) FROM Message") != 1 || rowCount(t, s, "SELECT COUNT(*) FROM Delivery") != 1 {
				t.Fatal("retry duplicated accepted mail")
			}
		})
	}
}

func TestNamedSendRetrySurvivesRestart(t *testing.T) {
	s, clock, file := testStore(t)
	_, ar := session(t, s, 1)
	a := operationAccount(t, s, "Persistent")
	repo := ar["repository_id"].(string)
	grantNamed(t, s, a, repo, "rw")
	r := Request{RepositoryID: repo, Key: "restart", To: []string{ar["agent"].(map[string]any)["id"].(string)}, Body: "durable"}
	first := mustNamedSend(t, s, a, r)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s.lock = nil
	clock.advance(time.Second)
	reopened, err := Open(file, clock)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	if !reflect.DeepEqual(first, mustNamedSend(t, reopened, a, r)) {
		t.Fatal("restart changed exact send")
	}
	if rowCount(t, reopened, "SELECT COUNT(*) FROM Delivery") != 1 {
		t.Fatal("restart duplicated delivery")
	}
}
