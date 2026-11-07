package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// The table in docs/adr/003-path-identity.md. Every pair is one file on APFS.
var samePath = []struct {
	name, a, b string
	equalFold  bool
}{
	{"ascii case", "a.go", "A.GO", true},
	{"composition", "café", "café", false},
	{"sharp s", "straße", "STRASSE", false},
	{"ligature", "ﬁle", "FILE", false},
	{"dotted capital", "İx", "i̇x", false},
	{"final sigma", "σ", "ς", true},
	{"kelvin sign", "K", "k", true},
}

const fixtureRemote = "https://example.com/team/project"

func worktreeRequest(n int) Request {
	dir := fmt.Sprintf("/repo/w%d", n)
	return Request{Client: "codex", Host: "host", Cwd: dir, Worktree: dir, RemoteURL: fixtureRemote, RootCommit: "root", SessionID: fmt.Sprint(n), Source: "hook"}
}

func fixtureSession(t *testing.T, s *Store, n int) (Request, map[string]any) {
	t.Helper()
	out := call(t, s, "sessions", worktreeRequest(n))
	return Request{ExecutionID: out["execution_id"].(string), Generation: out["generation"].(int64)}, out
}

func TestPathKeyMatchesWhatAPFSTreatsAsOneFile(t *testing.T) {
	for _, row := range samePath {
		if pathKey(row.a, "folded") != pathKey(row.b, "folded") {
			t.Errorf("%s: %q and %q have different keys", row.name, row.a, row.b)
		}
		if strings.EqualFold(row.a, row.b) != row.equalFold {
			t.Errorf("%s: the ADR's EqualFold column is wrong", row.name)
		}
		if pathKey(row.a, "sensitive") != row.a {
			t.Errorf("%s: an opted-out repository must compare exactly", row.name)
		}
	}
	if pathKey("a.go", "folded") == pathKey("b.go", "folded") {
		t.Fatal("different files share a key")
	}
}

func TestEveryRowConflictsThroughTheHook(t *testing.T) {
	for _, row := range samePath {
		t.Run(row.name, func(t *testing.T) {
			s, _, _ := testStore(t)
			a, _ := fixtureSession(t, s, 1)
			b, _ := fixtureSession(t, s, 2)
			held := "src/" + row.a
			asked := "src/" + row.b
			if out := call(t, s, "touch", touchRequest(a, "first", held)); out["action"] != "allow" {
				t.Fatal(out)
			}
			out := call(t, s, "touch", touchRequest(b, "second", asked))
			message := out["message"].(string)
			if out["action"] != "deny" || !strings.Contains(message, asked) || !strings.Contains(message, " as "+held) {
				t.Fatalf("the refusal must show both spellings: %v", out)
			}
		})
	}
}

func TestEveryRowConflictsThroughTheGuard(t *testing.T) {
	for _, row := range samePath {
		t.Run(row.name, func(t *testing.T) {
			s, _, _ := testStore(t)
			a, _ := fixtureSession(t, s, 1)
			held := "src/" + row.a
			asked := "src/" + row.b
			call(t, s, "claim", claimRequest(a, "held", held, "exact"))
			r := Request{Host: "host", Worktree: "/repo/w2", RemoteURL: fixtureRemote, RootCommit: "root", Paths: []string{asked}}
			out := call(t, s, "check", r)
			message := out["message"].(string)
			if !strings.Contains(message, asked) || !strings.Contains(message, " as "+held) {
				t.Fatalf("the guard must show both spellings: %v", out)
			}
			conflict := out["conflicts"].([]map[string]any)[0]
			if conflict["path"] != asked || conflict["claimed_path"] != held {
				t.Fatal(conflict)
			}
		})
	}
}

func TestEveryRowConflictsForAnExplicitClaim(t *testing.T) {
	for _, row := range samePath {
		t.Run(row.name, func(t *testing.T) {
			s, _, _ := testStore(t)
			a, _ := fixtureSession(t, s, 1)
			b, _ := fixtureSession(t, s, 2)
			call(t, s, "claim", claimRequest(a, "held", row.a, "exact"))
			_, err := s.Call(context.Background(), "claim", claimRequest(b, "asked", row.b, "exact"))
			var refused *Error
			if !errors.As(err, &refused) || refused.Code != "conflict" {
				t.Fatal(err)
			}
			conflict := refused.Details.(map[string]any)["conflicts"].([]map[string]any)[0]
			if conflict["path"] != row.b || conflict["claimed_path"] != row.a {
				t.Fatal(conflict)
			}
		})
	}
}

func TestPrefixClaimsCompareByKey(t *testing.T) {
	s, _, _ := testStore(t)
	a, _ := fixtureSession(t, s, 1)
	b, _ := fixtureSession(t, s, 2)
	call(t, s, "claim", claimRequest(a, "dir", "café/Src", "prefix"))
	out := call(t, s, "touch", touchRequest(b, "under", "CAFÉ/src/deep/File.go"))
	if out["action"] != "deny" {
		t.Fatal("a file under a prefix in another spelling was allowed", out)
	}
	if !strings.Contains(out["message"].(string), "as café/Src") {
		t.Fatal(out)
	}
	expectError(t, s, "claim", claimRequest(b, "wider", "CAFÉ", "prefix"), "conflict")
	if out := call(t, s, "touch", touchRequest(b, "beside", "café/Srcs/a.go")); out["action"] != "allow" {
		t.Fatal("a sibling that shares only the leading characters was refused", out)
	}
}

func TestRepositoryWidePrefixStillCoversEverything(t *testing.T) {
	s, _, _ := testStore(t)
	a, _ := fixtureSession(t, s, 1)
	b, _ := fixtureSession(t, s, 2)
	r := claimRequest(a, "all", "", "prefix")
	r.RepoWide = true
	call(t, s, "claim", r)
	if out := call(t, s, "touch", touchRequest(b, "any", "ANY/Path.go")); out["action"] != "deny" {
		t.Fatal(out)
	}
}

func TestClaimsKeepThePathAsWritten(t *testing.T) {
	s, _, _ := testStore(t)
	a, out := fixtureSession(t, s, 1)
	call(t, s, "claim", claimRequest(a, "held", "SRC/Mixed.GO", "exact"))
	list := call(t, s, "claims", Request{RepositoryID: out["repository_id"].(string)})["claims"].([]map[string]any)
	if len(list) != 1 || list[0]["path"] != "SRC/Mixed.GO" {
		t.Fatal(list)
	}
	if _, found := list[0]["path_key"]; found {
		t.Fatal("the key is not for display", list[0])
	}
}

func TestOneExecutionKeepsOneClaimAcrossSpellings(t *testing.T) {
	s, _, _ := testStore(t)
	a, out := fixtureSession(t, s, 1)
	repo := Request{RepositoryID: out["repository_id"].(string)}
	call(t, s, "touch", touchRequest(a, "first", "src/a.go"))
	if out := call(t, s, "touch", touchRequest(a, "second", "SRC/A.GO", "src/a.go")); out["action"] != "allow" {
		t.Fatal(out)
	}
	call(t, s, "claim", claimRequest(a, "explicit", "Src/A.go", "exact"))
	list := call(t, s, "claims", repo)["claims"].([]map[string]any)
	if len(list) != 1 || list[0]["path"] != "src/a.go" {
		t.Fatal("spellings of one file must not multiply claims", list)
	}
}

func TestReplayedTouchChecksTheKey(t *testing.T) {
	s, _, _ := testStore(t)
	a, _ := fixtureSession(t, s, 1)
	first := touchRequest(a, "tool", "src/café.go")
	call(t, s, "touch", first)
	everything := a
	everything.All = true
	call(t, s, "release", everything)
	call(t, s, "claim", claimRequest(a, "again", "SRC/CAFÉ.GO", "exact"))
	if out := call(t, s, "touch", first); out["action"] != "allow" {
		t.Fatal("the same file under another spelling still owns the replay", out)
	}
	call(t, s, "release", everything)
	expectError(t, s, "touch", first, "lost")
}

func TestIgnoredPathsAreFoldedToo(t *testing.T) {
	s, _, _ := testStore(t)
	a, _ := fixtureSession(t, s, 1)
	out := call(t, s, "touch", touchRequest(a, "tool", "DIST/app.js"))
	if len(out["ignored"].([]string)) != 1 || len(out["granted"].([]string)) != 0 {
		t.Fatal(out)
	}
}

func TestOptingOutComparesExactlyAndRekeysLiveClaims(t *testing.T) {
	ctx := context.Background()
	s, _, _ := testStore(t)
	a, out := fixtureSession(t, s, 1)
	b, _ := fixtureSession(t, s, 2)
	repo := out["repository_id"].(string)
	call(t, s, "claim", claimRequest(a, "held", "src/a.go", "exact"))
	if err := s.SetRepositoryPolicy(ctx, repo, "sensitive", "deny"); err != nil {
		t.Fatal(err)
	}
	if out := call(t, s, "touch", touchRequest(b, "other", "SRC/A.GO")); out["action"] != "allow" {
		t.Fatal("an opted-out repository treated two spellings as one", out)
	}
	if out := call(t, s, "touch", touchRequest(b, "same", "src/a.go")); out["action"] != "deny" {
		t.Fatal(out)
	}
	// Going back folds the two live claims to one key. Both stay; only new claims conflict.
	if err := s.SetRepositoryPolicy(ctx, repo, "folded", "deny"); err != nil {
		t.Fatal(err)
	}
	list := call(t, s, "claims", Request{RepositoryID: repo})["claims"].([]map[string]any)
	if len(list) != 2 {
		t.Fatal(list)
	}
	c, _ := fixtureSession(t, s, 3)
	if out := call(t, s, "touch", touchRequest(c, "third", "sRc/a.Go")); out["action"] != "deny" {
		t.Fatal(out)
	}
}

func TestRecoveryCarriesTheKey(t *testing.T) {
	s, _, file := testStore(t)
	a, _ := fixtureSession(t, s, 1)
	call(t, s, "claim", claimRequest(a, "held", "SRC/A.GO", "exact"))
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := Open(file, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	r := worktreeRequest(1)
	r.Generation = a.Generation
	call(t, restarted, "sessions", r)
	var key string
	var rows int
	if err = restarted.db.QueryRow("SELECT COUNT(*),MAX(path_key) FROM Claim WHERE revoked_ms IS NULL AND boot_epoch=?", restarted.epoch).Scan(&rows, &key); err != nil || rows != 1 || key != pathKey("SRC/A.GO", "folded") {
		t.Fatal(rows, key, err)
	}
}

func TestNewRepositoriesFoldByDefault(t *testing.T) {
	s, _, _ := testStore(t)
	_, out := fixtureSession(t, s, 1)
	var policy string
	if err := s.db.QueryRow("SELECT case_policy FROM Repository WHERE id=?", out["repository_id"]).Scan(&policy); err != nil || policy != "folded" {
		t.Fatal(policy, err)
	}
}

// oldDatabase builds a file as version 5 left it: real rows, no path_key, sensitive repositories.
// The last claim is released, because the backfill covers history too.
func oldDatabase(t *testing.T) (file string, held []string) {
	t.Helper()
	s, _, file := testStore(t)
	a, _ := fixtureSession(t, s, 1)
	held = []string{"SRC/A.GO", "café/Dir", "src/free.go"}
	call(t, s, "claim", claimRequest(a, "exact", held[0], "exact"))
	call(t, s, "claim", claimRequest(a, "prefix", held[1], "prefix"))
	released := a
	released.Fence = number(call(t, s, "claim", claimRequest(a, "gone", held[2], "exact"))["fence"])
	call(t, s, "release", released)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", file)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	for _, q := range []string{"ALTER TABLE Claim DROP COLUMN path_key", "UPDATE Repository SET case_policy='sensitive'", "PRAGMA user_version=5"} {
		if _, err = raw.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	return file, held
}

func TestMigrationBackfillsEveryClaimAndFoldsEveryRepository(t *testing.T) {
	file, held := oldDatabase(t)
	s, err := Open(file, nil)
	if err != nil {
		t.Fatal(err)
	}
	var version int
	if err = s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 6 {
		t.Fatal(version, err)
	}
	for _, p := range held {
		var key string
		if err = s.db.QueryRow("SELECT path_key FROM Claim WHERE path=?", p).Scan(&key); err != nil || key != pathKey(p, "folded") {
			t.Fatal(p, key, err)
		}
	}
	var sensitive int
	if err = s.db.QueryRow("SELECT COUNT(*) FROM Repository WHERE case_policy<>'folded'").Scan(&sensitive); err != nil || sensitive != 0 {
		t.Fatal(sensitive, err)
	}
	s.Close()
	// A second open changes nothing, and a migrated claim refuses another spelling.
	again, err := Open(file, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	r := worktreeRequest(9)
	r.Host = "other"
	out := call(t, again, "sessions", r)
	b := Request{ExecutionID: out["execution_id"].(string), Generation: out["generation"].(int64)}
	if got := call(t, again, "touch", touchRequest(b, "tool", "src/a.go")); got["action"] != "deny" || !strings.Contains(got["message"].(string), "as SRC/A.GO") {
		t.Fatal(got)
	}
}

func TestPathKeyMigrationRollsBackAtEveryStep(t *testing.T) {
	file, held := oldDatabase(t)
	f := &storageFault{discover: true}
	name := registerFaultDriver(f)
	if s, err := openDatabase(file, nil, name); !errors.Is(err, errStorageFault) {
		if s != nil {
			s.Close()
		}
		t.Fatal("discovery", err)
	}
	trace := append([]string(nil), f.trace...)
	f.discover = false
	backfill := 0
	for _, label := range trace {
		if strings.Contains(label, "path_key") {
			backfill++
		}
	}
	if backfill < len(held) {
		t.Fatalf("the sweep did not reach the backfill: %v", trace)
	}
	for i, label := range trace {
		f.count = 0
		f.at = i + 1
		f.trace = nil
		s, err := openDatabase(file, nil, name)
		f.at = 0
		if !errors.Is(err, errStorageFault) {
			if s != nil {
				s.Close()
			}
			t.Fatalf("fault %d %s: %v", i+1, label, err)
		}
		raw, err := sql.Open("sqlite", file)
		if err != nil {
			t.Fatal(err)
		}
		var version, keyed, folded int
		if err = raw.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 5 {
			t.Fatalf("fault %d %s left version %d: %v", i+1, label, version, err)
		}
		if err = raw.QueryRow("SELECT COUNT(*) FROM pragma_table_info('Claim') WHERE name='path_key'").Scan(&keyed); err != nil || keyed != 0 {
			t.Fatalf("fault %d %s left the column: %v", i+1, label, err)
		}
		if err = raw.QueryRow("SELECT COUNT(*) FROM Repository WHERE case_policy='folded'").Scan(&folded); err != nil || folded != 0 {
			t.Fatalf("fault %d %s left a folded repository: %v", i+1, label, err)
		}
		raw.Close()
	}
	s, err := Open(file, nil)
	if err != nil {
		t.Fatal("the failed upgrade could not be retried", err)
	}
	defer s.Close()
	var key string
	if err = s.db.QueryRow("SELECT path_key FROM Claim WHERE path=?", held[0]).Scan(&key); err != nil || key != pathKey(held[0], "folded") {
		t.Fatal(key, err)
	}
}

// A hub rolled back to an older binary keeps schema 6 and writes claims without a key.
func TestClaimsFromAnOlderBinaryAreKeyedAtTheNextStart(t *testing.T) {
	s, _, file := testStore(t)
	a, _ := fixtureSession(t, s, 1)
	call(t, s, "claim", claimRequest(a, "dir", "Docs/Guide", "prefix"))
	wide := claimRequest(a, "wide", "", "prefix")
	wide.RepoWide = true
	call(t, s, "claim", wide)
	if _, err := s.db.Exec("UPDATE Claim SET path_key=''"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := Open(file, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	for path, want := range map[string]string{"Docs/Guide": "docs/guide", "": ""} {
		var key string
		if err = restarted.db.QueryRow("SELECT path_key FROM Claim WHERE path=?", path).Scan(&key); err != nil || key != want {
			t.Fatalf("%q: %q %v", path, key, err)
		}
	}
}
