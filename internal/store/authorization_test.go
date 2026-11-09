package store

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// Synthetic sessions isolate the authorization matrix from credential verification.
// Session issuance and login have separate lifecycle tests.
func consoleTestPrincipal(t *testing.T, s *Store, a ConsoleAccount) Principal {
	t.Helper()
	digest := make([]byte, 32)
	_, _ = rand.Read(digest)
	now := s.clock.Now().UnixMilli()
	if _, err := s.db.Exec("INSERT INTO ConsoleSession VALUES(?,?,?,?,?)", digest, a.ID, a.AuthVersion, now, now+int64(12*time.Hour/time.Millisecond)); err != nil {
		t.Fatal("synthetic console session failed")
	}
	return Principal{Kind: ConsolePrincipal, SessionDigest: digest}
}

func consoleCall(t *testing.T, s *Store, p Principal, op string, r Request) map[string]any {
	t.Helper()
	out, err := s.CallConsole(context.Background(), p, op, r)
	if err != nil {
		t.Fatalf("%s refused: %v", op, err)
	}
	return out
}

func TestConsoleOperationMatrixAndCredentialBoundaries(t *testing.T) {
	s, _, _ := testStore(t)
	_, sr := session(t, s, 1)
	repo := sr["repository_id"].(string)
	for _, mode := range []string{"", "ro", "rw"} {
		a := createTestConsoleAccount(t, s, "member-"+mode, "member")
		if mode != "" {
			if _, err := s.db.Exec("INSERT INTO ConsoleRepositoryAccess VALUES(?,?,?)", a.ID, repo, mode); err != nil {
				t.Fatal(err)
			}
		}
		p := consoleTestPrincipal(t, s, a)
		for _, op := range []string{"ack", "claim", "touch", "release", "ui_read", "ui_revoke", "ui_doctor_get", "ui_doctor_put", "ui_accounts", "ui_access", "contact", "forget", "merge", "messages", "agents", "sessions", "status", "events", "unknown"} {
			_, err := s.CallConsole(context.Background(), p, op, Request{RepositoryID: repo})
			consoleError(t, err, "forbidden")
		}
		out := consoleCall(t, s, p, "ui_overview", Request{})
		want := 1
		if mode == "" {
			want = 0
		}
		if len(out["repositories"].([]map[string]any)) != want {
			t.Fatal("omitted repository bypassed grants")
		}
		_, err := s.CallConsole(context.Background(), p, "ui_overview", Request{RepositoryID: "guessed"})
		consoleError(t, err, "not_found")
		if mode == "" {
			_, err = s.CallConsole(context.Background(), p, "ui_overview", Request{RepositoryID: repo})
			consoleError(t, err, "not_found")
		}
		updated, err := s.EditConsoleProfile(context.Background(), p, 1, "Own name", "en", "pl")
		if err != nil || updated.ID != a.ID || updated.Role != "member" || updated.AuthVersion != a.AuthVersion {
			t.Fatal("self profile edit changed authority", err)
		}
	}
	operator := createTestConsoleAccount(t, s, "operator", "operator")
	p := consoleTestPrincipal(t, s, operator)
	for _, op := range []string{"ack", "claim", "messages", "ui_doctor_put", "merge", "forget", "contact"} {
		_, err := s.CallConsole(context.Background(), p, op, Request{})
		consoleError(t, err, "forbidden")
	}
	for _, kind := range []PrincipalKind{OwnerPrincipal, HostPrincipal} {
		machine := Principal{Kind: kind}
		_, err := s.CallConsole(context.Background(), machine, "ui_messages", Request{})
		consoleError(t, err, "forbidden")
		_, err = s.CallAuthorized(context.Background(), machine, "ui_messages", Request{})
		consoleError(t, err, "forbidden")
		_, err = s.CallAuthorized(context.Background(), Principal{Kind: kind, SessionDigest: p.SessionDigest}, "ui_overview", Request{})
		consoleError(t, err, "forbidden")
		for _, op := range []string{"ui_profile", "ui_access", "ui_accounts"} {
			_, err = s.CallAuthorized(context.Background(), machine, op, Request{})
			consoleError(t, err, "forbidden")
		}
		if _, err = s.CallAuthorized(context.Background(), machine, "status", Request{}); err != nil {
			t.Fatal("machine status refused", err)
		}
	}
	for _, op := range []string{"merge", "forget", "contact"} {
		_, err := s.CallAuthorized(context.Background(), Principal{Kind: HostPrincipal}, op, Request{})
		consoleError(t, err, "forbidden")
	}
	_, err := s.CallAuthorized(context.Background(), Principal{Kind: OwnerPrincipal}, "contact", Request{RepositoryID: repo})
	if err != nil {
		t.Fatal("owner contact refused", err)
	}
}

func TestConsoleSessionAuthorityIsRechecked(t *testing.T) {
	for _, change := range []string{"missing", "expired", "future", "version", "disabled", "unknown-role", "none"} {
		t.Run(change, func(t *testing.T) {
			s, clock, _ := testStore(t)
			a := createTestConsoleAccount(t, s, "Member", "member")
			p := consoleTestPrincipal(t, s, a)
			consoleCall(t, s, p, "ui_overview", Request{})
			var err error
			switch change {
			case "missing":
				_, err = s.db.Exec("DELETE FROM ConsoleSession")
			case "expired":
				clock.advance(12 * time.Hour)
			case "future":
				_, err = s.db.Exec("UPDATE ConsoleSession SET created_ms=created_ms+1")
			case "version":
				_, err = s.db.Exec("UPDATE ConsoleAccount SET auth_version=auth_version+1")
			case "disabled":
				_, err = s.db.Exec("UPDATE ConsoleAccount SET disabled=1")
			case "unknown-role":
				if _, err = s.db.Exec("PRAGMA ignore_check_constraints=ON"); err == nil {
					_, err = s.db.Exec("UPDATE ConsoleAccount SET role='superuser'")
				}
			case "none":
				_, err = s.db.Exec("UPDATE ConsoleAccount SET role='none',disabled=1")
			}
			if err != nil {
				t.Fatal(err)
			}
			_, err = s.CallConsole(context.Background(), p, "ui_overview", Request{})
			consoleError(t, err, "unauthorized")
		})
	}
	s, _, _ := testStore(t)
	_, err := s.Call(context.Background(), "ui_receipts", Request{})
	consoleError(t, err, "forbidden")
	for _, p := range []Principal{{}, {Kind: "invented"}, {Kind: ConsolePrincipal}, {Kind: ConsolePrincipal, SessionDigest: make([]byte, 31)}, {Kind: ConsolePrincipal, SessionDigest: make([]byte, 32)}} {
		_, err := s.CallAuthorized(context.Background(), p, "ui_overview", Request{})
		consoleError(t, err, "unauthorized")
	}
}

func TestConsoleAccessLifecycleAndActionAttribution(t *testing.T) {
	s, _, _ := testStore(t)
	_, sr := session(t, s, 1)
	repo := sr["repository_id"].(string)
	op := createTestConsoleAccount(t, s, "Operator", "operator")
	member := createTestConsoleAccount(t, s, "Member", "member")
	p, mp := consoleTestPrincipal(t, s, op), consoleTestPrincipal(t, s, member)
	_, err := s.ChangeConsoleAccess(context.Background(), mp, op.ID, 1, "member", false, nil)
	consoleError(t, err, "forbidden")
	if _, err = s.ListConsoleAccounts(context.Background(), mp); err == nil {
		t.Fatal("member listed profiles")
	}
	for _, edit := range []struct {
		role     string
		disabled bool
	}{{"member", false}, {"operator", true}} {
		_, err = s.ChangeConsoleAccess(context.Background(), p, op.ID, 1, edit.role, edit.disabled, nil)
		consoleError(t, err, "conflict")
	}
	_, err = s.ChangeConsoleAccess(context.Background(), p, "missing", 1, "member", false, nil)
	consoleError(t, err, "not_found")
	_, err = s.ChangeConsoleAccess(context.Background(), p, member.ID, 1, "invented", false, nil)
	consoleError(t, err, "bad_request")
	updated, err := s.ChangeConsoleAccess(context.Background(), p, member.ID, 1, "member", false, []ConsoleGrant{{repo, "rw"}})
	if err != nil || updated.AuthVersion != 2 || updated.EditRevision != 2 {
		t.Fatal("access edit lost versions", err)
	}
	_, err = s.CallConsole(context.Background(), mp, "ui_overview", Request{})
	consoleError(t, err, "unauthorized")
	_, err = s.ChangeConsoleAccess(context.Background(), p, member.ID, 1, "member", false, nil)
	consoleError(t, err, "conflict")
	mp = consoleTestPrincipal(t, s, updated)
	_, err = s.EditConsoleProfile(context.Background(), mp, 1, "Stale", "en", "en")
	consoleError(t, err, "conflict")
	_, err = s.EditConsoleProfile(context.Background(), mp, 2, strings.Repeat("x", 129), "en", "en")
	consoleError(t, err, "bad_request")
	_, err = s.EditConsoleProfile(context.Background(), Principal{}, 2, "Name", "en", "en")
	consoleError(t, err, "unauthorized")
	if _, err = s.EditConsoleProfile(context.Background(), mp, 2, "New name", "en", "en"); err != nil {
		t.Fatal(err)
	}
	accounts, err := s.ListConsoleAccounts(context.Background(), p)
	if err != nil || len(accounts) != 2 {
		t.Fatal("operator list failed", err)
	}
	for _, a := range accounts {
		if a.PasswordHash != "" || a.LoginKey != "" {
			t.Fatal("credential escaped account projection")
		}
	}
	data, _ := json.Marshal(accounts)
	if strings.Contains(string(data), "password") {
		t.Fatal("credential escaped JSON")
	}
	if rowCount(t, s, "SELECT COUNT(*) FROM ConsoleAction WHERE actor_account_id='"+op.ID+"' AND target_account_id='"+member.ID+"' AND action='access'") != 1 || rowCount(t, s, "SELECT COUNT(*) FROM ConsoleAction WHERE actor_account_id='"+member.ID+"' AND action='profile'") != 1 {
		t.Fatal("action records lost authenticated person")
	}
	second := createTestConsoleAccount(t, s, "Second", "operator")
	if _, err = s.ChangeConsoleAccess(context.Background(), p, op.ID, 1, "member", false, nil); err != nil {
		t.Fatal("demotion with another operator failed", err)
	}
	_, err = s.ListConsoleAccounts(context.Background(), p)
	consoleError(t, err, "unauthorized")
	sp := consoleTestPrincipal(t, s, second)
	current, err := s.GetConsoleAccount(context.Background(), op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ChangeConsoleAccess(context.Background(), sp, op.ID, current.EditRevision, "none", false, nil); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentDemotionsKeepAnEnabledOperator(t *testing.T) {
	s, _, _ := testStore(t)
	a := createTestConsoleAccount(t, s, "A", "operator")
	b := createTestConsoleAccount(t, s, "B", "operator")
	pa, pb := consoleTestPrincipal(t, s, a), consoleTestPrincipal(t, s, b)
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, change := range []struct {
		p      Principal
		target string
	}{{pa, a.ID}, {pb, b.ID}} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.ChangeConsoleAccess(context.Background(), change.p, change.target, 1, "member", false, nil)
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	success, conflict := 0, 0
	for err := range results {
		if err == nil {
			success++
		} else {
			consoleError(t, err, "conflict")
			conflict++
		}
	}
	if success != 1 || conflict != 1 || rowCount(t, s, "SELECT COUNT(*) FROM ConsoleAccount WHERE role='operator' AND disabled=0") != 1 {
		t.Fatal("concurrent edits removed last operator")
	}
}

func TestConsoleAuthorizationStorageFailuresRollback(t *testing.T) {
	for _, operation := range []string{"read", "profile", "access", "access-demotion", "accounts", "ack", "revoke"} {
		t.Run(operation, func(t *testing.T) {
			s, _, file := testStore(t)
			r, sr := session(t, s, 1)
			repo := sr["repository_id"].(string)
			op := createTestConsoleAccount(t, s, "Operator", "operator")
			member := createTestConsoleAccount(t, s, "Member", "member")
			other := createTestConsoleAccount(t, s, "Other operator", "operator")
			p := consoleTestPrincipal(t, s, op)
			claim := call(t, s, "claim", claimRequest(r, "claim", "held", "exact"))
			r.To, r.Key, r.Body = []string{"human"}, "mail", "content"
			mail := call(t, s, "messages", r)
			invoke := func() error {
				var err error
				switch operation {
				case "read":
					_, err = s.CallConsole(context.Background(), p, "ui_threads", Request{RepositoryID: repo})
				case "profile":
					_, err = s.EditConsoleProfile(context.Background(), p, 1, "New", "en", "en")
				case "access":
					_, err = s.ChangeConsoleAccess(context.Background(), p, member.ID, 1, "member", false, []ConsoleGrant{{repo, "rw"}})
				case "access-demotion":
					_, err = s.ChangeConsoleAccess(context.Background(), p, other.ID, 1, "member", false, nil)
				case "accounts":
					_, err = s.ListConsoleAccounts(context.Background(), p)
				case "ack":
					_, err = s.CallConsole(context.Background(), p, "ui_read", Request{RepositoryID: repo, Key: mail["thread_id"].(string)})
				case "revoke":
					_, err = s.CallConsole(context.Background(), p, "ui_revoke", Request{Fence: claim["fence"].(int64)})
				}
				return err
			}
			fault := installFaultDriver(t, s, file)
			fault.discover = true
			if err := invoke(); !errors.Is(err, errStorageFault) {
				t.Fatal("discovery failed", err)
			}
			trace := append([]string(nil), fault.trace...)
			fault.discover = false
			before := databaseState(t, s)
			for i := range trace {
				fault.count, fault.at, fault.trace = 0, i+1, nil
				err := invoke()
				fault.at = 0
				if !errors.Is(err, errStorageFault) {
					t.Fatalf("boundary %d lost storage failure: %v", i+1, err)
				}
				if databaseState(t, s) != before {
					t.Fatalf("boundary %d committed unauthorized partial state", i+1)
				}
			}
			if err := invoke(); err != nil {
				t.Fatal("retry failed", err)
			}
		})
	}
}
