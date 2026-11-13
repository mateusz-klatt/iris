package wake

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/hook"
)

func inspectionBytes(t *testing.T, d document) []byte {
	t.Helper()
	if d.Attempts == nil {
		d.Attempts = []attempt{}
	}
	body, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestInspectMissingDirectoriesCreateNothing(t *testing.T) {
	token := filepath.Join(t.TempDir(), "token")
	if got, err := Inspect(token); err != nil || got != nil {
		t.Fatalf("missing guards: %+v %v", got, err)
	}
	if entries, err := os.ReadDir(filepath.Dir(token)); err != nil || len(entries) != 0 {
		t.Fatalf("inspection created files: %v %v", entries, err)
	}
	sessions := hook.NewSessions(token)
	if err := os.Mkdir(sessions.Dir(), 0700); err != nil {
		t.Fatal(err)
	}
	if got, err := Inspect(token); err != nil || got != nil {
		t.Fatalf("missing wake directory: %+v %v", got, err)
	}
	if entries, err := os.ReadDir(sessions.Dir()); err != nil || len(entries) != 0 {
		t.Fatalf("inspection created a lock without guards: %v %v", entries, err)
	}
	if _, err := Inspect(""); !errors.Is(err, ErrInspection) {
		t.Fatal("empty token path selected a relative sessions directory", err)
	}
}

func TestInspectLeavesGuardBytesAndDirectoryPermissionsUnchanged(t *testing.T) {
	token := filepath.Join(t.TempDir(), "token")
	sessions := hook.NewSessions(token)
	s := newStateStore(sessions, "session")
	a := testAttempt(1, "queue", "stored")
	a.Token, a.Message, a.Detail = "PRIVATE_TOKEN", "PRIVATE_PROMPT", "PRIVATE_DETAIL"
	d := testDocument(a)
	d.LastFailure = &failure{AtMS: 102, Detail: "PRIVATE_FAILURE"}
	d.Binary = binaryInfo{Path: "/expected/codex", Version: "0.167.0", ClientVersion: "0.167.0", CheckedMS: 99}
	mustPublish(t, s, mustSnapshot(t, s), d)
	before, err := os.ReadFile(s.path())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Dir(s.path()), 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Dir(s.path()), 0700) })
	beforeMode, err := os.Stat(filepath.Dir(s.path()))
	if err != nil {
		t.Fatal(err)
	}
	guards, err := Inspect(token)
	if err != nil || len(guards) != 1 {
		t.Fatalf("private guard unreadable: %+v %v", guards, err)
	}
	got := guards[0]
	if got.Path != s.path() || got.Status != "usable" || !got.Outstanding || got.Kind != "queue" || got.Outcome != "stored" || got.Attempts != 1 || got.Slot0 != 2 || got.FailureMS != 102 || got.BinaryPath != "/expected/codex" || got.CheckedMS != 99 {
		t.Fatalf("wrong metadata: %+v", got)
	}
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "PRIVATE_") {
		t.Fatal("inspection exported prompt, token or stored error detail")
	}
	if got.FailureReason != "Unrecognized wake failure" {
		t.Fatalf("unknown failure was not replaced with a fixed reason: %q", got.FailureReason)
	}
	after, err := os.ReadFile(s.path())
	if err != nil || string(after) != string(before) {
		t.Fatal("inspection changed guard bytes")
	}
	afterMode, err := os.Stat(filepath.Dir(s.path()))
	if err != nil || afterMode.Mode() != beforeMode.Mode() {
		t.Fatal("inspection changed directory protection")
	}
	for _, name := range []string{"Token", "Message", "Raw", "Detail", "Stream"} {
		if _, ok := reflect.TypeFor[Inspection]().FieldByName(name); ok {
			t.Fatalf("inspection exposes %s", name)
		}
	}
}

func TestInspectFailureReasonsRecognizeOnlyExactPreSubmissionDiagnostics(t *testing.T) {
	for _, reason := range []string{
		"Codex executable is not available on PATH",
		"Codex initialization did not name its version",
		"Codex version command did not complete successfully",
		"Codex version response exceeded its bound",
		"Codex version response named no version",
		"Codex executable and initialized client versions differ",
		"Codex status proxy could not start",
		"Codex status proxy did not establish its connection",
		"Codex thread status could not be verified",
		"Codex status proxy exceeded its bound or failed",
		"Codex queue command could not start",
		"Codex has no saved rollout for this thread",
		"Kimi executable version could not be determined",
		"Kimi executable and server versions differ",
		"Kimi session is open in another Kimi process",
	} {
		d := testDocument(testAttempt(1, "binary_check", "failed"))
		d.LastFailure = &failure{AtMS: 200, Detail: reason}
		got := inspectDocument("guard", inspectionBytes(t, d))
		if got.Status != "usable" || got.FailureReason != reason || got.FailureMS != 200 {
			t.Fatalf("known failure lost its reason/time: %+v", got)
		}
		for _, unknown := range []string{reason + " PRIVATE_SECRET", reason + "\nPRIVATE_SECRET", "PRIVATE_SECRET " + reason} {
			d.LastFailure.Detail = unknown
			got = inspectDocument("guard", inspectionBytes(t, d))
			if got.FailureReason != "Unrecognized wake failure" {
				t.Fatalf("inexact diagnostic leaked raw detail: %q", got.FailureReason)
			}
		}
	}
}

func TestInspectValidatesRetainedEnvelopeAndWhitelistsOutcomes(t *testing.T) {
	for _, body := range []string{"broken", `{"version":2,"rev":0,"attempts":[]}`, `{"version":1,"attempts":[]}`, `{"version":1,"rev":0}`, `{"version":1,"rev":0,"attempts":null}`} {
		if got := inspectDocument("guard", []byte(body)); got.Status != "unusable" || got.Outstanding {
			t.Fatalf("damaged state became fresh usable metadata: %+v", got)
		}
	}
	if got := inspectDocument("guard", inspectionBytes(t, testDocument())); got.Status != "usable" || got.Attempts != 0 || got.Kind != "" {
		t.Fatalf("empty valid ledger: %+v", got)
	}
	for _, outcome := range []string{"submitted", "stored", "failed", "aborted", "unknown", "UNTRUSTED_OUTCOME", ""} {
		a := testAttempt(1, "queue", outcome)
		got := inspectDocument("guard", inspectionBytes(t, testDocument(a)))
		want := outcome
		if outcome == "unknown" || outcome == "UNTRUSTED_OUTCOME" || outcome == "" {
			want = "unknown"
		}
		if got.Status != "usable" || got.Outcome != want || got.StartedMS != a.StartedMS || got.AnsweredMS != a.AnsweredMS || got.Outstanding != a.outstanding() {
			t.Fatalf("outcome was not conservative: %+v", got)
		}
	}
	a := testAttempt(1, "queue", "aborted")
	a.ConsumedMS = 200
	if got := inspectDocument("guard", inspectionBytes(t, testDocument(a))); got.ConsumedMS != 200 || got.Slot0 != 2 || got.Outstanding {
		t.Fatal("late abort refunded consumed diagnostic quota")
	}
	b := testAttempt(1, "binary_check", "failed")
	if got := inspectDocument("guard", inspectionBytes(t, testDocument(b))); got.Kind != "binary_check" || got.Slot0 != 1 || got.Outstanding {
		t.Fatal("known binary failure became an outstanding queue")
	}
}

func TestInspectFiltersOutsideLockAndReadsEachGuardInItsOwnHold(t *testing.T) {
	s := testStore(t)
	_ = mustSnapshot(t, s)
	dir := filepath.Dir(s.path())
	for _, name := range []string{strings.Repeat("a", 32), strings.Repeat("b", 32), ".tmp-ignored", strings.Repeat("C", 32), "short", "not-hex"} {
		if err := os.WriteFile(filepath.Join(dir, name), inspectionBytes(t, testDocument(testAttempt(1, "queue", ""))), 0600); err != nil {
			t.Fatal(err)
		}
	}
	var order []string
	held := false
	ops := inspectionIO{readDir: func(path string) ([]os.DirEntry, error) {
		if held {
			t.Fatal("listing held shared lock")
		}
		return os.ReadDir(path)
	}, lock: func(time.Duration) (func(), error) {
		if held {
			t.Fatal("previous guard retained shared lock")
		}
		held = true
		order = append(order, "lock")
		return func() { held = false; order = append(order, "unlock") }, nil
	}, readFile: func(path string) ([]byte, error) {
		if !held {
			t.Fatal("guard read lacked shared lock")
		}
		order = append(order, "read")
		return os.ReadFile(path)
	}}
	got, err := inspect(s.sessions, ops)
	if err != nil || len(got) != 2 || held {
		t.Fatalf("guard filtering/locking: %+v %v", got, err)
	}
	if !reflect.DeepEqual(order, []string{"lock", "read", "unlock", "lock", "read", "unlock"}) {
		t.Fatalf("inspection batched an unbounded lock hold: %v", order)
	}
}

func TestInspectClassifiesListLockReadAndConcurrentRemoval(t *testing.T) {
	s := testStore(t)
	_ = mustSnapshot(t, s)
	writeState(t, s, testDocument(testAttempt(1, "queue", "")))
	for _, where := range []string{"list", "lock", "lock budget", "read", "removed", "malformed"} {
		t.Run(where, func(t *testing.T) {
			unlocks := 0
			ops := inspectionIO{readDir: os.ReadDir, readFile: os.ReadFile, lock: func(time.Duration) (func(), error) { return func() { unlocks++ }, nil }}
			switch where {
			case "list":
				ops.readDir = func(string) ([]os.DirEntry, error) { return nil, errStateTest }
			case "lock":
				ops.lock = func(time.Duration) (func(), error) { return nil, errStateTest }
			case "lock budget":
				ops.lock = func(time.Duration) (func(), error) { return nil, hook.ErrSessionsLocked }
			case "read":
				ops.readFile = func(string) ([]byte, error) { return nil, errStateTest }
			case "removed":
				ops.readFile = func(string) ([]byte, error) { return nil, os.ErrNotExist }
			case "malformed":
				ops.readFile = func(string) ([]byte, error) { return []byte("malformed guard"), nil }
			}
			got, err := inspect(s.sessions, ops)
			switch where {
			case "list", "lock":
				if !errors.Is(err, ErrInspection) {
					t.Fatal(err)
				}
			case "lock budget":
				if !errors.Is(err, hook.ErrSessionsLocked) {
					t.Fatal(err)
				}
			default:
				if err != nil || unlocks != 1 {
					t.Fatalf("read failed to release lock: %v %d", err, unlocks)
				}
				if where == "removed" {
					if len(got) != 0 {
						t.Fatal("concurrent removal became a retained guard")
					}
				} else if len(got) != 1 || got[0].Status != map[string]string{"read": "unreadable", "malformed": "unusable"}[where] {
					t.Fatalf("guard failure was hidden: %+v", got)
				}
			}
		})
	}
}
