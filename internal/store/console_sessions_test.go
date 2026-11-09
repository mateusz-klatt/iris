package store

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"
)

const consoleArgonTestHash = "$argon2id$v=19$m=65536,t=3,p=1$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
const consoleOtherArgonTestHash = "$argon2id$v=19$m=65536,t=3,p=1$AQEBAQEBAQEBAQEBAQEBAQ$AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE"

func newSessionAccount(t *testing.T, s *Store, username, role string) ConsoleAccount {
	t.Helper()
	a, err := s.CreateConsoleAccount(t.Context(), ConsoleAccountInput{Username: username, PasswordHash: consoleArgonTestHash, Role: role})
	if err != nil {
		t.Fatal("session account setup failed", err)
	}
	return a
}
func issueTestSession(t *testing.T, s *Store, a ConsoleAccount, n byte) Principal {
	t.Helper()
	digest := make([]byte, 32)
	digest[0] = n
	if _, err := s.IssueConsoleSession(t.Context(), a, digest, ""); err != nil {
		t.Fatal("session issuance failed", err)
	}
	return Principal{Kind: ConsolePrincipal, SessionDigest: digest}
}
func sessionCount(t *testing.T, s *Store, a ConsoleAccount) int {
	t.Helper()
	var count int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM ConsoleSession WHERE account_id=?", a.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestPersistentConsoleSessionsKeepAbsoluteExpiryAndSecretsPrivate(t *testing.T) {
	s, clock, file := testStore(t)
	a := newSessionAccount(t, s, "persistent", "member")
	p := issueTestSession(t, s, a, 1)
	initial, err := s.ConsoleSessionInfo(t.Context(), p.SessionDigest)
	if err != nil {
		t.Fatal(err)
	}
	if initial.Account.ID != a.ID || initial.Account.PasswordHash != "" || initial.Account.LoginKey != "" || initial.CreatedMS != clock.Now().UnixMilli() || initial.ExpiresMS-initial.CreatedMS != ConsoleSessionLifetime.Milliseconds() {
		t.Fatal("unsafe or non-absolute session projection")
	}
	raw, _ := json.Marshal(initial)
	var projection map[string]any
	_ = json.Unmarshal(raw, &projection)
	account := projection["account"].(map[string]any)
	for _, key := range []string{"digest", "password_hash", "login_key", "csrf", "token"} {
		if _, ok := account[key]; ok {
			t.Fatal("secret in account inspection")
		}
		if _, ok := projection[key]; ok {
			t.Fatal("secret in session inspection")
		}
	}
	snapshot, err := s.ConsoleSessionAccount(t.Context(), p.SessionDigest)
	if err != nil || snapshot.PasswordHash != a.PasswordHash || snapshot.LastLoginMS == nil || *snapshot.LastLoginMS != initial.CreatedMS {
		t.Fatal("trusted verification snapshot missing", err)
	}
	clock.advance(3 * time.Hour)
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s.lock = nil
	reopened, err := Open(file, clock)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	restored, err := reopened.ConsoleSessionInfo(t.Context(), p.SessionDigest)
	if err != nil || restored.CreatedMS != initial.CreatedMS || restored.ExpiresMS != initial.ExpiresMS {
		t.Fatal("restart changed absolute session", err)
	}
	clock.advance(9*time.Hour - time.Millisecond)
	if _, err = reopened.ConsoleSessionAccount(t.Context(), p.SessionDigest); err != nil {
		t.Fatal("session expired early", err)
	}
	clock.advance(time.Millisecond)
	_, err = reopened.ConsoleSessionAccount(t.Context(), p.SessionDigest)
	consoleError(t, err, "unauthorized")
	before := databaseState(t, reopened)
	if err = reopened.DeleteConsoleSession(t.Context(), p.SessionDigest); err != nil || databaseState(t, reopened) != before {
		t.Fatal("expired logout changed server state", err)
	}
}

func TestConsoleSessionAdmissionBoundsAndPrunesWithoutExtendingOldSessions(t *testing.T) {
	s, clock, _ := testStore(t)
	a := newSessionAccount(t, s, "bounded", "operator")
	b := newSessionAccount(t, s, "other", "member")
	old := issueTestSession(t, s, b, 250)
	first := issueTestSession(t, s, a, 1)
	for i := byte(2); i <= MaxConsoleSessions+1; i++ {
		clock.advance(time.Millisecond)
		issueTestSession(t, s, a, i)
	}
	if sessionCount(t, s, a) != MaxConsoleSessions || sessionCount(t, s, b) != 1 {
		t.Fatal("session bound crossed account boundaries")
	}
	_, err := s.ConsoleSessionInfo(t.Context(), first.SessionDigest)
	consoleError(t, err, "unauthorized")
	clock.advance(ConsoleSessionLifetime)
	issueTestSession(t, s, a, 100)
	if sessionCount(t, s, a) != 1 || sessionCount(t, s, b) != 0 {
		t.Fatal("expired sessions not pruned")
	}
	_, err = s.ConsoleSessionInfo(t.Context(), old.SessionDigest)
	consoleError(t, err, "unauthorized")
	// Equal creation times still retain exactly the configured bound.
	for i := byte(101); i < 101+MaxConsoleSessions; i++ {
		issueTestSession(t, s, a, i)
	}
	if sessionCount(t, s, a) != MaxConsoleSessions {
		t.Fatal("same-tick logins escaped session bound")
	}
}

func TestConsoleSessionRefusalsAndIdempotentLogout(t *testing.T) {
	s, clock, _ := testStore(t)
	a := newSessionAccount(t, s, "logout", "member")
	p := issueTestSession(t, s, a, 1)
	changes := s.Changes()
	if err := s.DeleteConsoleSession(t.Context(), p.SessionDigest); err != nil {
		t.Fatal(err)
	}
	select {
	case <-changes:
	default:
		t.Fatal("logout did not wake established sockets")
	}
	_, err := s.ConsoleSessionInfo(t.Context(), p.SessionDigest)
	consoleError(t, err, "unauthorized")
	state := databaseState(t, s)
	for _, digest := range [][]byte{p.SessionDigest, nil, make([]byte, 31), make([]byte, 32)} {
		if err = s.DeleteConsoleSession(t.Context(), digest); err != nil || databaseState(t, s) != state {
			t.Fatal("stale logout changed state", err)
		}
	}
	for _, digest := range [][]byte{nil, make([]byte, 31), p.SessionDigest} {
		_, err = s.ConsoleSessionInfo(t.Context(), digest)
		consoleError(t, err, "unauthorized")
	}
	p = issueTestSession(t, s, a, 2)
	clock.advance(-time.Hour)
	_, err = s.ConsoleSessionInfo(t.Context(), p.SessionDigest)
	consoleError(t, err, "unauthorized")
	clock.advance(time.Hour)
	if _, err = s.db.Exec("UPDATE ConsoleAccount SET disabled=1 WHERE id=?", a.ID); err != nil {
		t.Fatal(err)
	}
	_, err = s.ConsoleSessionInfo(t.Context(), p.SessionDigest)
	consoleError(t, err, "unauthorized")
	_, err = s.IssueConsoleSession(t.Context(), a, make([]byte, 32), "")
	consoleError(t, err, "unauthorized")
	var actions int
	if err = s.db.QueryRow("SELECT COUNT(*) FROM ConsoleAction WHERE action='logout' AND actor_account_id=?", a.ID).Scan(&actions); err != nil || actions != 1 {
		t.Fatal("logout audit was duplicated", err)
	}
}

func TestConsolePasswordUpgradeAndExplicitChangesRevokeEverySession(t *testing.T) {
	s, _, _ := testStore(t)
	legacy := createTestConsoleAccount(t, s, "legacy", "member")
	old := consoleTestPrincipal(t, s, legacy)
	if _, err := s.IssueConsoleSession(t.Context(), legacy, make([]byte, 32), ""); err == nil {
		t.Fatal("legacy login issued without upgrade")
	}
	digest := make([]byte, 32)
	digest[0] = 10
	upgraded, err := s.IssueConsoleSession(t.Context(), legacy, digest, consoleArgonTestHash)
	if err != nil {
		t.Fatal(err)
	}
	if upgraded.PasswordHash != consoleArgonTestHash || upgraded.AuthVersion != legacy.AuthVersion+1 || upgraded.EditRevision != legacy.EditRevision+1 || sessionCount(t, s, legacy) != 1 {
		t.Fatal("upgrade failed to replace only after verification")
	}
	_, err = s.ConsoleSessionInfo(t.Context(), old.SessionDigest)
	consoleError(t, err, "unauthorized")
	p := Principal{Kind: ConsolePrincipal, SessionDigest: digest}
	other := issueTestSession(t, s, upgraded, 11)
	edited, err := s.EditConsoleProfile(t.Context(), p, upgraded.EditRevision, "Cosmetic", "en", "")
	if err != nil {
		t.Fatal(err)
	}
	if edited.AuthVersion != upgraded.AuthVersion {
		t.Fatal("profile changed authority")
	}
	if _, err = s.ConsoleSessionInfo(t.Context(), other.SessionDigest); err != nil {
		t.Fatal("profile revoked sessions", err)
	}
	changes := s.Changes()
	if err = s.ChangeConsolePassword(t.Context(), p, upgraded, consoleOtherArgonTestHash); err != nil {
		t.Fatal(err)
	}
	select {
	case <-changes:
	default:
		t.Fatal("password change did not wake sockets")
	}
	for _, session := range []Principal{p, other} {
		_, err = s.ConsoleSessionInfo(t.Context(), session.SessionDigest)
		consoleError(t, err, "unauthorized")
	}
	after, err := s.GetConsoleAccount(t.Context(), legacy.ID)
	if err != nil || after.AuthVersion != upgraded.AuthVersion+1 || after.PasswordHash != consoleOtherArgonTestHash || sessionCount(t, s, legacy) != 0 {
		t.Fatal("password change did not revoke all sessions", err)
	}
	_, err = s.IssueConsoleSession(t.Context(), upgraded, make([]byte, 32), "")
	consoleError(t, err, "unauthorized")
	if _, err = s.IssueConsoleSession(t.Context(), after, make([]byte, 32), ""); err != nil {
		t.Fatal("fresh authentication refused", err)
	}
}

func TestConsoleCredentialCommitRejectsInvalidAndStaleSnapshots(t *testing.T) {
	s, _, _ := testStore(t)
	a := newSessionAccount(t, s, "current", "member")
	p := issueTestSession(t, s, a, 1)
	other := newSessionAccount(t, s, "someone else", "member")
	for _, hash := range []string{"", consoleTestHash, "invalid"} {
		consoleError(t, s.ChangeConsolePassword(t.Context(), p, a, hash), "bad_request")
		_, err := s.RecoverConsoleOperator(t.Context(), a, hash)
		consoleError(t, err, "bad_request")
		_, err = s.ProvisionConsoleOperator(t.Context(), ConsoleAccountInput{Username: "new", PasswordHash: hash})
		consoleError(t, err, "bad_request")
	}
	consoleError(t, s.ChangeConsolePassword(t.Context(), Principal{}, a, consoleOtherArgonTestHash), "unauthorized")
	consoleError(t, s.ChangeConsolePassword(t.Context(), p, other, consoleOtherArgonTestHash), "forbidden")
	stale := a
	stale.PasswordHash = consoleOtherArgonTestHash
	consoleError(t, s.ChangeConsolePassword(t.Context(), p, stale, consoleOtherArgonTestHash), "unauthorized")
	for _, before := range []ConsoleAccount{stale, {ID: "missing", PasswordHash: consoleArgonTestHash, AuthVersion: 1}} {
		_, err := s.IssueConsoleSession(t.Context(), before, make([]byte, 32), "")
		consoleError(t, err, "unauthorized")
	}
	for _, input := range []struct {
		before  ConsoleAccount
		digest  []byte
		upgrade string
	}{{a, nil, ""}, {a, make([]byte, 32), consoleOtherArgonTestHash}, {ConsoleAccount{ID: a.ID, PasswordHash: "malformed"}, make([]byte, 32), ""}} {
		_, err := s.IssueConsoleSession(t.Context(), input.before, input.digest, input.upgrade)
		consoleError(t, err, "bad_request")
	}
	legacy := createTestConsoleAccount(t, s, "invalid upgrade", "member")
	_, err := s.IssueConsoleSession(t.Context(), legacy, make([]byte, 32), consoleTestHash)
	consoleError(t, err, "bad_request")
}

func TestOwnerProvisionAndRecoveryRecordAuthorityAndRecheckRevision(t *testing.T) {
	s, _, _ := testStore(t)
	a, err := s.ProvisionConsoleOperator(t.Context(), ConsoleAccountInput{Username: "Owner named", PasswordHash: consoleArgonTestHash, Role: "none", Disabled: true})
	if err != nil || !a.Enabled() || a.Role != "operator" {
		t.Fatal("owner provision failed", err)
	}
	p := issueTestSession(t, s, a, 1)
	before := a
	a, err = s.UpdateConsoleProfile(t.Context(), a.ID, a.EditRevision, "updated", "pl", "")
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.RecoverConsoleOperator(t.Context(), before, consoleOtherArgonTestHash)
	consoleError(t, err, "conflict")
	disabled, err := s.ReplaceConsoleAccess(t.Context(), a.ID, a.EditRevision, "none", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := s.RecoverConsoleOperator(t.Context(), disabled, consoleOtherArgonTestHash)
	if err != nil || !recovered.Enabled() || recovered.Role != "operator" || recovered.PasswordHash != consoleOtherArgonTestHash || recovered.AuthVersion != disabled.AuthVersion+1 || recovered.EditRevision != disabled.EditRevision+1 {
		t.Fatal("recovery failed", err)
	}
	_, err = s.ConsoleSessionInfo(t.Context(), p.SessionDigest)
	consoleError(t, err, "unauthorized")
	_, err = s.RecoverConsoleOperator(t.Context(), disabled, consoleArgonTestHash)
	consoleError(t, err, "unauthorized")
	if rowCount(t, s, "SELECT COUNT(*) FROM ConsoleAction WHERE actor_kind='owner' AND actor_account_id IS NULL AND action IN ('owner_provision','owner_recovery')") != 2 {
		t.Fatal("owner audit invented an account actor")
	}
	_, err = s.ProvisionConsoleOperator(t.Context(), ConsoleAccountInput{Username: "Owner named", PasswordHash: consoleArgonTestHash})
	consoleError(t, err, "conflict")
}

func TestConsoleFrameAdmissionIsSerializedWithRevocation(t *testing.T) {
	s, _, _ := testStore(t)
	a := newSessionAccount(t, s, "frame", "operator")
	p := issueTestSession(t, s, a, 1)
	admitted, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		done <- s.AdmitConsoleFrame(context.Background(), p, func(frame map[string]any) error {
			if len(frame) != 2 || frame["kind"] != "changed" {
				return errors.New("unsafe frame")
			}
			close(admitted)
			<-release
			return nil
		})
	}()
	<-admitted
	resetDone := make(chan error, 1)
	go func() {
		_, err := s.RecoverConsoleOperator(context.Background(), a, consoleOtherArgonTestHash)
		resetDone <- err
	}()
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := <-resetDone; err != nil {
		t.Fatal(err)
	}
	called := false
	err := s.AdmitConsoleFrame(t.Context(), p, func(map[string]any) error { called = true; return nil })
	consoleError(t, err, "unauthorized")
	if called {
		t.Fatal("post-revocation frame was admitted")
	}
	a, err = s.GetConsoleAccount(t.Context(), a.ID)
	if err != nil {
		t.Fatal(err)
	}
	p = issueTestSession(t, s, a, 2)
	transportError := errors.New("queue unavailable")
	err = s.AdmitConsoleFrame(t.Context(), p, func(map[string]any) error { return transportError })
	if !errors.Is(err, transportError) {
		t.Fatal("frame admission swallowed backpressure")
	}
}

func TestConcurrentConsoleLoginAndRecoveryNeverRestoreOldCredentials(t *testing.T) {
	for i := 0; i < 12; i++ {
		s, _, _ := testStore(t)
		a := newSessionAccount(t, s, "concurrent", "operator")
		digest := make([]byte, 32)
		start := make(chan struct{})
		var wg sync.WaitGroup
		var loginErr, resetErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, loginErr = s.IssueConsoleSession(context.Background(), a, digest, "")
		}()
		go func() {
			defer wg.Done()
			<-start
			_, resetErr = s.RecoverConsoleOperator(context.Background(), a, consoleOtherArgonTestHash)
		}()
		close(start)
		wg.Wait()
		if resetErr != nil {
			t.Fatal("reset lost to stale login", resetErr)
		}
		if loginErr != nil {
			consoleError(t, loginErr, "unauthorized")
		}
		_, err := s.ConsoleSessionInfo(t.Context(), digest)
		consoleError(t, err, "unauthorized")
		current, err := s.GetConsoleAccount(t.Context(), a.ID)
		if err != nil || current.PasswordHash != consoleOtherArgonTestHash || current.AuthVersion != a.AuthVersion+1 || sessionCount(t, s, a) != 0 {
			t.Fatal("concurrent login restored old authority", err)
		}
	}
}

func TestConcurrentGrantDowngradeSerializesSendReplyAndCachedRetry(t *testing.T) {
	for _, mode := range []string{"ro", "none"} {
		for _, operation := range []string{"send", "reply", "cached"} {
			t.Run(mode+"/"+operation, func(t *testing.T) {
				for i := 0; i < 6; i++ {
					s, _, _ := testStore(t)
					machine, sr := session(t, s, 1)
					repo := sr["repository_id"].(string)
					recipient := sr["agent"].(map[string]any)["id"].(string)
					a := newSessionAccount(t, s, "writer", "member")
					a, err := s.ReplaceConsoleAccess(t.Context(), a.ID, a.EditRevision, "member", false, []ConsoleGrant{{repo, "rw"}})
					if err != nil {
						t.Fatal(err)
					}
					p := issueTestSession(t, s, a, 1)
					req := Request{RepositoryID: repo, Key: "prepared", To: []string{recipient}, Body: "prepared body"}
					if operation == "reply" {
						parent := call(t, s, "messages", sendTo(machine, "parent", "parent body", "human"))
						req.ReplyTo = parent["message_id"].(string)
					}
					if operation == "cached" {
						consoleCall(t, s, p, "ui_messages", req)
					}
					before := rowCount(t, s, "SELECT COUNT(*) FROM Message")
					grants := []ConsoleGrant{}
					if mode == "ro" {
						grants = append(grants, ConsoleGrant{repo, "ro"})
					}
					var writeErr, downgradeErr error
					var current ConsoleAccount
					start := make(chan struct{})
					var wg sync.WaitGroup
					wg.Add(2)
					go func() {
						defer wg.Done()
						<-start
						_, writeErr = s.CallConsole(context.Background(), p, "ui_messages", req)
					}()
					go func() {
						defer wg.Done()
						<-start
						current, downgradeErr = s.ReplaceConsoleAccess(context.Background(), a.ID, a.EditRevision, "member", false, grants)
					}()
					close(start)
					wg.Wait()
					if downgradeErr != nil {
						t.Fatal("downgrade lost to prepared request", downgradeErr)
					}
					if writeErr != nil {
						consoleError(t, writeErr, "unauthorized")
					}
					after := rowCount(t, s, "SELECT COUNT(*) FROM Message")
					want := before
					if writeErr == nil && operation != "cached" {
						want++
					}
					if after != want {
						t.Fatal("serialized write duplicated or hid message")
					}
					for _, request := range []Request{req, {RepositoryID: repo, Key: "post-downgrade-send", To: []string{recipient}, Body: "must not send"}, {RepositoryID: repo, Key: "post-downgrade-reply", To: []string{recipient}, ReplyTo: req.ReplyTo, Body: "must not reply"}} {
						_, err = s.CallConsole(t.Context(), p, "ui_messages", request)
						consoleError(t, err, "unauthorized")
					}
					fresh := issueTestSession(t, s, current, 2)
					overview := consoleCall(t, s, fresh, "ui_overview", Request{})
					wantRepos := 0
					if mode == "ro" {
						wantRepos = 1
					}
					if len(overview["repositories"].([]map[string]any)) != wantRepos {
						t.Fatal("reauthentication restored removed scope")
					}
					for _, request := range []Request{req, {RepositoryID: repo, Key: "fresh-send", To: []string{recipient}, Body: "denied"}, {RepositoryID: repo, Key: "fresh-reply", To: []string{recipient}, ReplyTo: req.ReplyTo, Body: "denied"}} {
						_, err = s.CallConsole(t.Context(), fresh, "ui_messages", request)
						consoleError(t, err, "forbidden")
					}
					if rowCount(t, s, "SELECT COUNT(*) FROM Message") != after {
						t.Fatal("post-downgrade request used earlier authority")
					}
				}
			})
		}
	}
}

func TestConsoleLegacyUpgradeInsertionFailureLeavesHashAndSessionsUnchanged(t *testing.T) {
	s, _, _ := testStore(t)
	legacy := createTestConsoleAccount(t, s, "legacy rollback", "member")
	old := consoleTestPrincipal(t, s, legacy)
	other := newSessionAccount(t, s, "digest owner", "member")
	collision := issueTestSession(t, s, other, 90)
	before := databaseState(t, s)
	_, err := s.IssueConsoleSession(t.Context(), legacy, collision.SessionDigest, consoleArgonTestHash)
	if err == nil || databaseState(t, s) != before {
		t.Fatal("failed session insertion changed legacy credentials")
	}
	if _, err = s.ConsoleSessionInfo(t.Context(), old.SessionDigest); err != nil {
		t.Fatal("failed upgrade revoked prior session", err)
	}
}

func TestConsoleOwnerAuditMigrationPreservesHistoryAndEnforcesActorKinds(t *testing.T) {
	s, clock, file := testStore(t)
	a := newSessionAccount(t, s, "historical actor", "operator")
	issueTestSession(t, s, a, 1)
	// A version18 database has account actors only. Reconstruct that table instead
	// of leaving new-schema columns behind in an old-version migration fixture.
	_, err := s.db.Exec(`CREATE TABLE LegacyConsoleAction (
 id INTEGER PRIMARY KEY AUTOINCREMENT,actor_account_id TEXT NOT NULL REFERENCES ConsoleAccount(id),
 target_account_id TEXT REFERENCES ConsoleAccount(id),action TEXT NOT NULL,outcome TEXT NOT NULL,created_ms INTEGER NOT NULL);
 INSERT INTO LegacyConsoleAction SELECT id,actor_account_id,target_account_id,action,outcome,created_ms FROM ConsoleAction;
 DROP TABLE ConsoleAction;ALTER TABLE LegacyConsoleAction RENAME TO ConsoleAction;
 DROP TABLE ConsoleAssignmentImport;PRAGMA user_version=18;`)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s.lock = nil
	reopened, err := Open(file, clock)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	var actor, kind, action string
	var id int64
	if err = reopened.db.QueryRow("SELECT id,actor_account_id,actor_kind,action FROM ConsoleAction").Scan(&id, &actor, &kind, &action); err != nil || id != 1 || actor != a.ID || kind != "account" || action != "login" {
		t.Fatal("migration changed historical attribution", err)
	}
	if _, err = reopened.RecoverConsoleOperator(t.Context(), a, consoleOtherArgonTestHash); err != nil {
		t.Fatal(err)
	}
	var next int64
	if err = reopened.db.QueryRow("SELECT id FROM ConsoleAction WHERE actor_kind='owner'").Scan(&next); err != nil || next <= id {
		t.Fatal("migration regressed action identity", err)
	}
	for _, input := range []struct {
		actor any
		kind  string
	}{{nil, "account"}, {a.ID, "owner"}, {nil, "unknown"}} {
		_, err = reopened.db.Exec("INSERT INTO ConsoleAction(actor_account_id,actor_kind,action,outcome,created_ms) VALUES(?,?,'invalid','accepted',0)", input.actor, input.kind)
		if err == nil {
			t.Fatal("audit accepted contradictory actor kind")
		}
	}
}
