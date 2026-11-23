package store

import (
	"database/sql"
	"errors"
	"strings"
	"testing"
)

func TestWorktreeDisplayPathsKeepSpellingAndFoldOnlyWindows(t *testing.T) {
	cases := []struct {
		raw, want string
		sameAs    string
		differ    string
	}{
		{raw: "", want: ""},
		{raw: `C:\repo\File`, want: "C:/repo/File", sameAs: `c:/repo/file`, differ: "/repo/File"},
		{raw: `c:\`, want: "C:/", sameAs: "C:/", differ: "C:/repo"},
		{raw: "C:", want: "C:/"},
		{raw: `C:\foo\.\bar\..`, want: "C:/foo"},
		{raw: `C:\foo\..\..\outside`, want: "C:/outside"},
		{raw: `C:\repo.\file.txt.`, want: "C:/repo/file.txt", sameAs: `c:/repo/file.txt`},
		{raw: `C:\repo \a `, want: "C:/repo/a"},
		{raw: `C:\...`, want: "C:/..."},
		{raw: `C:\foo\\bar`, want: "C:/foo/bar"},
		{raw: `\\Server\Share\Repo`, want: "//Server/Share/Repo", sameAs: `//server/share/repo`, differ: "C:/Server/Share/Repo"},
		{raw: `\\server\share\`, want: "//server/share", sameAs: `//server/share`},
		{raw: `\\server.\share.\repo.`, want: "//server/share/repo"},
		{raw: `\\server\share\repo\..`, want: "//server/share"},
		{raw: `\\server\share\..\..\x`, want: "//server/share/x"},
		{raw: `\\server`, want: "//server"},
		{raw: `\\`, want: "//", sameAs: "//"},
		{raw: "//Server/Share/Repo", want: "//Server/Share/Repo"},
		{raw: `C:\Straße`, want: "C:/Straße", sameAs: "C:/STRASSE"},
		{raw: "C:/caf\u00e9", want: "C:/caf\u00e9", sameAs: "C:/cafe\u0301"},
		{raw: "/Repo/File", want: "/Repo/File", differ: "/repo/file"},
		{raw: "/a/../b/", want: "/b", sameAs: "/b"},
		{raw: "/a/./b", want: "/a/b"},
		{raw: "/a/../../b", want: "/b"},
		{raw: "/", want: "/"},
		{raw: ` /a\b`, want: ` /a\b`, differ: "/a/b"},
		{raw: `/a\b`, want: `/a\b`, differ: "/a/b"},
		{raw: "a/../b", want: "b"},
		{raw: "a/..", want: ""},
		{raw: ".", want: ""},
		{raw: "../b", want: "../b"},
		{raw: "../../b", want: "../../b"},
		{raw: "1:/foo", want: "1:/foo", differ: "C:/foo"},
		{raw: "C", want: "C"},
		{raw: "Cx", want: "Cx"},
	}
	for _, tc := range cases {
		got := canonicalDisplayPath(tc.raw)
		if got != tc.want {
			t.Errorf("canonical %q = %q, want %q", tc.raw, got, tc.want)
		}
		if !worktreePathsEqual(tc.raw, tc.want) {
			t.Errorf("%q does not match its stored spelling %q", tc.raw, tc.want)
		}
		if tc.sameAs != "" && !worktreePathsEqual(tc.raw, tc.sameAs) {
			t.Errorf("%q and %q should be the same worktree path", tc.raw, tc.sameAs)
		}
		if tc.differ != "" && worktreePathsEqual(tc.raw, tc.differ) {
			t.Errorf("%q and %q should be different worktree paths", tc.raw, tc.differ)
		}
	}
	if !worktreePathsEqual("", "") || worktreePathsEqual("", "/a") {
		t.Fatal("empty paths compared incorrectly")
	}
}

func TestWorktreeIdentityStaysWithTheDirectory(t *testing.T) {
	s, _, _ := testStore(t)
	var first, second, otherHost, moved, missing string
	err := s.transact(t.Context(), func(tx *transaction) error {
		var err error
		first, err = tx.ensureWorktreeKey("host", "dev:1")
		if err != nil {
			return err
		}
		second, err = tx.ensureWorktreeKey("host", "dev:1")
		if err != nil {
			return err
		}
		otherHost, err = tx.ensureWorktreeKey("other", "dev:1")
		if err != nil {
			return err
		}
		moved, err = tx.ensureWorktreeKey("host", "dev:2")
		if err != nil {
			return err
		}
		missing, err = tx.ensureWorktreeKey("host", "")
		if err != nil {
			return err
		}
		if missing != "" {
			return errors.New("missing id minted a key")
		}
		if missing, err = tx.ensureWorktreeKey("", "dev:1"); err != nil {
			return err
		}
		if missing != "" {
			return errors.New("missing host minted a key")
		}
		// A bind or symlink is the same directory id. The second spelling folds to the
		// first alias, and both keep the one key. The branch is not part of the key.
		firstPath := Request{Host: "host", Worktree: "/wt", WorktreeID: "dev:1", Cwd: "/wt", RemoteURL: "https://example.com/app.git", RootCommit: "root"}
		if err = tx.canonicalWorktree(&firstPath); err != nil {
			return err
		}
		alias := Request{Host: "host", Worktree: "/mnt/wt", WorktreeID: "dev:1", Cwd: "/mnt/wt/sub", RemoteURL: "https://example.com/app.git", RootCommit: "root"}
		if err = tx.canonicalWorktree(&alias); err != nil {
			return err
		}
		if alias.Worktree != "/wt" || alias.Cwd != "/wt/sub" {
			return errors.New("bind mount was not the first path")
		}
		again, err := tx.ensureWorktreeKey("host", "dev:1")
		if err != nil {
			return err
		}
		if again != first {
			return errors.New("display path changed the key")
		}
		// Replacing the alias when the remote changes keeps the same key (ADR 037).
		replaced := Request{Host: "host", Worktree: "/elsewhere", WorktreeID: "dev:1", Cwd: "/elsewhere", RemoteURL: "https://example.com/other.git", RootCommit: "other"}
		if err = tx.canonicalWorktree(&replaced); err != nil {
			return err
		}
		if replaced.Worktree != "/elsewhere" {
			return errors.New("a different repository was folded into the old path")
		}
		kept, err := tx.ensureWorktreeKey("host", "dev:1")
		if err != nil {
			return err
		}
		if kept != first {
			return errors.New("alias replacement rekeyed the directory")
		}
		var remote string
		if err = tx.row("SELECT remote_url FROM WorktreeAlias WHERE host_id=? AND worktree_id=?", "host", "dev:1").Scan(&remote); err != nil {
			return err
		}
		if remote != "https://example.com/other.git" {
			return errors.New("alias was not replaced")
		}
		// Case spellings on one volume share a key only because the id matches.
		// The paths themselves do not fold.
		mac, err := tx.ensureWorktreeKey("mac", "dev:case")
		if err != nil {
			return err
		}
		macAgain, err := tx.ensureWorktreeKey("mac", "dev:case")
		if err != nil {
			return err
		}
		if mac == "" || macAgain != mac || worktreePathsEqual("/Repo", "/repo") {
			return errors.New("posix case aliases folded, or did not share the directory id's key")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if first == "" || !strings.HasPrefix(first, "wt_") || strings.Contains(first, "dev:1") || strings.Contains(first, "host") {
		t.Fatalf("key %q is not an opaque worktree key", first)
	}
	if second != first || otherHost == first || otherHost == "" || moved == first || moved == otherHost {
		t.Fatalf("keys first=%q second=%q host=%q moved=%q", first, second, otherHost, moved)
	}
	var n int
	if err = s.db.QueryRow("SELECT COUNT(*) FROM WorktreeIdentity WHERE host_id=? AND worktree_id=?", "host", "dev:1").Scan(&n); err != nil || n != 1 {
		t.Fatalf("identity rows %d: %v", n, err)
	}
	if err = s.db.QueryRow("SELECT COUNT(*) FROM WorktreeIdentity WHERE worktree_key=?", "").Scan(&n); err != nil || n != 0 {
		t.Fatal("null identity was stored", n, err)
	}
}

func TestAStorageFailureDoesNotPublishAWorktreeKey(t *testing.T) {
	s, _, file := testStore(t)
	var kept string
	if err := s.transact(t.Context(), func(tx *transaction) error {
		var err error
		kept, err = tx.ensureWorktreeKey("host", "dev:kept")
		return err
	}); err != nil || kept == "" {
		t.Fatal(kept, err)
	}
	fault := installFaultDriver(t, s, file)
	invoke := func() error {
		return s.transact(t.Context(), func(tx *transaction) error {
			if _, err := tx.ensureWorktreeKey("host", "dev:new"); err != nil {
				return err
			}
			_, err := tx.ensureWorktreeKey("host", "dev:kept")
			return err
		})
	}
	fault.discover = true
	if err := invoke(); !errors.Is(err, errStorageFault) {
		t.Fatal("discovery", err)
	}
	trace := append([]string(nil), fault.trace...)
	fault.discover = false
	if len(trace) == 0 || !strings.Contains(strings.Join(trace, "\n"), "WorktreeIdentity") {
		t.Fatalf("trace did not reach worktree identity: %v", trace)
	}
	for i, label := range trace {
		fault.at, fault.count, fault.trace = i+1, 0, nil
		err := invoke()
		fault.at = 0
		if !errors.Is(err, errStorageFault) {
			t.Fatalf("fault %d %s: %v", i+1, label, err)
		}
		var fresh int
		var still string
		if err = s.db.QueryRow("SELECT COUNT(*) FROM WorktreeIdentity WHERE worktree_id=?", "dev:new").Scan(&fresh); err != nil || fresh != 0 {
			t.Fatalf("fault %d published a key: %d %v", i+1, fresh, err)
		}
		if err = s.db.QueryRow("SELECT worktree_key FROM WorktreeIdentity WHERE worktree_id=?", "dev:kept").Scan(&still); err != nil || still != kept {
			t.Fatalf("fault %d changed the existing key %q: %v", i+1, still, err)
		}
	}
	fault.at, fault.count = 0, 0
	var minted string
	if err := s.transact(t.Context(), func(tx *transaction) error {
		var err error
		minted, err = tx.ensureWorktreeKey("host", "dev:new")
		if err != nil {
			return err
		}
		again, err := tx.ensureWorktreeKey("host", "dev:new")
		if err != nil || again != minted {
			return errors.New("retry did not keep the new key")
		}
		return nil
	}); err != nil || minted == "" || minted == kept {
		t.Fatal(minted, err)
	}
}

func TestMigration029LeavesLegacyRowsRepositoryWide(t *testing.T) {
	s, clock, file := testStore(t)
	a, registered := session(t, s, 1)
	repo := registered["repository_id"].(string)
	call(t, s, "touch", touchRequest(a, "legacy", "src/api.go"))
	operator := createTestConsoleAccount(t, s, "Scope reader", "operator")
	var scope string
	if err := s.db.QueryRow("SELECT auto_claim_scope FROM Repository WHERE id=?", repo).Scan(&scope); err != nil || scope != "repository" {
		t.Fatal(scope, err)
	}
	if _, err := s.db.Exec("UPDATE Repository SET auto_claim_scope=? WHERE id=?", "seat", repo); err == nil {
		t.Fatal("a scope other than repository or worktree was stored")
	}
	before := consoleRevision(t, s, operator)
	if _, err := s.db.Exec("UPDATE Repository SET auto_claim_scope='worktree' WHERE id=?", repo); err != nil {
		t.Fatal(err)
	}
	if consoleRevision(t, s, operator) <= before {
		t.Fatal("changing claim.auto_scope did not move the console")
	}
	var fence int64
	var path string
	var claimKey, claimPath, claimBranch sql.NullString
	if err := s.db.QueryRow("SELECT fence,path,worktree_key,worktree_path,branch FROM Claim WHERE execution_id=?", a.ExecutionID).Scan(&fence, &path, &claimKey, &claimPath, &claimBranch); err != nil {
		t.Fatal(err)
	}
	// No WorktreeID was established, so the key stays null and the claim is repository-wide.
	// The display path is the registered checkout. The session named no branch.
	if path != "src/api.go" || claimKey.Valid || claimPath.String != "/repo/w1" || claimBranch.Valid {
		t.Fatalf("automatic claim without a worktree id: %s key=%#v path=%#v branch=%#v", path, claimKey, claimPath, claimBranch)
	}
	var executionKey sql.NullString
	if err := s.db.QueryRow("SELECT worktree_key FROM Execution WHERE id=?", a.ExecutionID).Scan(&executionKey); err != nil || executionKey.Valid {
		t.Fatal("execution key", executionKey, err)
	}
	if _, err := s.db.Exec("UPDATE Claim SET worktree_key='wt_kept',worktree_path='/wt',branch='feature' WHERE fence=?", fence); err != nil {
		t.Fatal(err)
	}
	before = consoleRevision(t, s, operator)
	if _, err := s.db.Exec("UPDATE Claim SET expires_ms=expires_ms+1 WHERE fence=?", fence); err != nil {
		t.Fatal(err)
	}
	if consoleRevision(t, s, operator) != before {
		t.Fatal("renewing an expiry moved the console")
	}
	for _, q := range []string{
		"UPDATE Claim SET worktree_key='wt_other' WHERE fence=?",
		"UPDATE Claim SET worktree_path='/other' WHERE fence=?",
		"UPDATE Claim SET branch='other' WHERE fence=?",
	} {
		before = consoleRevision(t, s, operator)
		if _, err := s.db.Exec(q, fence); err != nil {
			t.Fatal(err)
		}
		if consoleRevision(t, s, operator) <= before {
			t.Fatalf("%s did not move the console", q)
		}
	}
	before = consoleRevision(t, s, operator)
	if _, err := s.db.Exec("UPDATE Claim SET worktree_key='wt_other' WHERE fence=?", fence); err != nil {
		t.Fatal(err)
	}
	if consoleRevision(t, s, operator) != before {
		t.Fatal("writing the same worktree key moved the console")
	}
	if _, err := s.db.Exec("UPDATE Repository SET auto_claim_scope='repository' WHERE id=?", repo); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow("SELECT worktree_key FROM Claim WHERE fence=?", fence).Scan(&claimKey); err != nil || claimKey.String != "wt_other" {
		t.Fatal("a scope change rewrote the claim", claimKey, err)
	}

	if _, err := s.db.Exec(removeWorktreeScopeSchema + "PRAGMA user_version=28"); err != nil {
		t.Fatal(err)
	}
	// An older hub inserts with the columns it knows. Forward again must not invent a key.
	if _, err := s.db.Exec("INSERT INTO Waiter(execution_id,repository_id,path_key,kind,expires_ms) VALUES(?,?,'src/api.go','exact',1)", a.ExecutionID, repo); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s.lock = nil
	reopened, err := Open(file, clock)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	var version int
	if err = reopened.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != latestVersion {
		t.Fatal(version, err)
	}
	if err = reopened.db.QueryRow("SELECT auto_claim_scope FROM Repository WHERE id=?", repo).Scan(&scope); err != nil || scope != "repository" {
		t.Fatal("reopened scope", scope, err)
	}
	if err = reopened.db.QueryRow("SELECT path,worktree_key,worktree_path,branch FROM Claim WHERE fence=?", fence).Scan(&path, &claimKey, &claimPath, &claimBranch); err != nil || path != "src/api.go" || claimKey.Valid || claimPath.Valid || claimBranch.Valid {
		t.Fatalf("forward again changed the legacy claim %s %#v %#v %#v %v", path, claimKey, claimPath, claimBranch, err)
	}
	var waiterKey sql.NullString
	if err = reopened.db.QueryRow("SELECT worktree_key FROM Waiter WHERE execution_id=?", a.ExecutionID).Scan(&waiterKey); err != nil || waiterKey.Valid {
		t.Fatal("older waiter gained a key", waiterKey, err)
	}
}

func TestMigration029RollsBackAtEveryStep(t *testing.T) {
	s, clock, file := testStore(t)
	a, registered := session(t, s, 1)
	repo := registered["repository_id"].(string)
	call(t, s, "touch", touchRequest(a, "legacy", "src/api.go"))
	var fence int64
	if err := s.db.QueryRow("SELECT fence FROM Claim WHERE execution_id=?", a.ExecutionID).Scan(&fence); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(removeWorktreeScopeSchema + "PRAGMA user_version=28"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s.lock = nil

	f := &storageFault{discover: true}
	name := registerFaultDriver(f)
	if failed, err := openDatabase(file, clock, name); !errors.Is(err, errStorageFault) {
		if failed != nil {
			_ = failed.Close()
		}
		t.Fatal("discovery", err)
	}
	trace := append([]string(nil), f.trace...)
	f.discover = false
	if !strings.Contains(strings.Join(trace, "\n"), "auto_claim_scope") {
		t.Fatalf("migration 029 missing from fault trace: %v", trace)
	}
	for i, label := range trace {
		f.count, f.at, f.trace = 0, i+1, nil
		failed, err := openDatabase(file, clock, name)
		f.at = 0
		if !errors.Is(err, errStorageFault) {
			if failed != nil {
				_ = failed.Close()
			}
			t.Fatalf("fault %d %s: %v", i+1, label, err)
		}
		raw, err := sql.Open("sqlite", file)
		if err != nil {
			t.Fatal(err)
		}
		var version int
		var path string
		var scopeColumn int
		if err = raw.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 28 {
			_ = raw.Close()
			t.Fatalf("fault %d %s changed version to %d: %v", i+1, label, version, err)
		}
		if err = raw.QueryRow("SELECT COUNT(*) FROM pragma_table_info('Repository') WHERE name='auto_claim_scope'").Scan(&scopeColumn); err != nil || scopeColumn != 0 {
			_ = raw.Close()
			t.Fatalf("fault %d %s left the scope column: %d %v", i+1, label, scopeColumn, err)
		}
		if err = raw.QueryRow("SELECT path FROM Claim WHERE fence=?", fence).Scan(&path); err != nil || path != "src/api.go" {
			_ = raw.Close()
			t.Fatalf("fault %d %s lost the claim: %s %v", i+1, label, path, err)
		}
		_ = raw.Close()
	}
	retried, err := openDatabase(file, clock, name)
	if err != nil {
		t.Fatal("migration 029 retry failed", err)
	}
	defer func() { _ = retried.Close() }()
	var version int
	var path, scope string
	var claimKey sql.NullString
	if err = retried.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != latestVersion {
		t.Fatal(version, err)
	}
	if err = retried.db.QueryRow("SELECT auto_claim_scope FROM Repository WHERE id=?", repo).Scan(&scope); err != nil || scope != "repository" {
		t.Fatal(scope, err)
	}
	if err = retried.db.QueryRow("SELECT path,worktree_key FROM Claim WHERE fence=?", fence).Scan(&path, &claimKey); err != nil || path != "src/api.go" || claimKey.Valid {
		t.Fatal(path, claimKey, err)
	}
}
