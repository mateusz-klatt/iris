package wake

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/hook"
)

var errStateTest = errors.New("injected state operation failure")

func testStore(t *testing.T) *stateStore {
	t.Helper()
	return newStateStore(hook.NewSessions(filepath.Join(t.TempDir(), "token")), "session")
}

func testAttempt(n int, kind, answer string) attempt {
	a := attempt{ID: fmt.Sprintf("%032x", n), Kind: kind, Stream: "stream", Cursor: 7, StartedMS: 100, Answer: answer}
	if answer != "" {
		a.AnsweredMS = 101
	}
	if kind == "queue" {
		a.Token = "token-" + a.ID
		a.Message = "fixed prompt " + a.Token
	}
	return a
}

func testDocument(a ...attempt) document { return document{Version: hook.WakeVersion, Attempts: a} }

func mustSnapshot(t *testing.T, s *stateStore) stateSnapshot {
	t.Helper()
	snapshot, err := s.snapshot()
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func mustPublish(t *testing.T, s *stateStore, before stateSnapshot, next document) publication {
	t.Helper()
	result, err := s.publish(before, next)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func writeState(t *testing.T, s *stateStore, d document) {
	t.Helper()
	body, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(s.path()), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.path(), body, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestStateDocumentValidatesEveryReservationBeforeReuse(t *testing.T) {
	queue := testAttempt(1, "queue", "")
	binary := testAttempt(1, "binary_check", "failed")
	consumed := queue
	consumed.ConsumedMS = 102
	consumed.Token, consumed.Message = "", ""
	for _, d := range []document{testDocument(), testDocument(queue), testDocument(binary), testDocument(consumed)} {
		if !d.valid() {
			t.Fatalf("valid document refused: %+v", d)
		}
	}
	invalid := map[string]func(*document){
		"version":        func(d *document) { d.Version++ },
		"revision":       func(d *document) { d.Rev = -1 },
		"binary time":    func(d *document) { d.Binary.CheckedMS = -1 },
		"failure time":   func(d *document) { d.LastFailure = &failure{Detail: "fixed reason"} },
		"failure detail": func(d *document) { d.LastFailure = &failure{AtMS: 1} },
		"id hex":         func(d *document) { d.Attempts[0].ID = "broken" },
		"id size":        func(d *document) { d.Attempts[0].ID = "00" },
		"id case":        func(d *document) { d.Attempts[0].ID = strings.Repeat("AB", 16) },
		"duplicate id":   func(d *document) { d.Attempts = append(d.Attempts, binary) },
		"missing stream": func(d *document) { d.Attempts[0].Stream = "" },
		"cursor":         func(d *document) { d.Attempts[0].Cursor = 0 },
		"slot":           func(d *document) { d.Attempts[0].Slot = 2 },
		"started":        func(d *document) { d.Attempts[0].StartedMS = 0 },
		"answer time":    func(d *document) { d.Attempts[0].AnsweredMS = -1 },
		"consumed time":  func(d *document) { d.Attempts[0].ConsumedMS = -1 },
		"mixed stream": func(d *document) {
			a := testAttempt(2, "binary_check", "failed")
			a.Stream = "other"
			d.Attempts = append(d.Attempts, a)
		},
		"mixed cursor": func(d *document) {
			a := testAttempt(2, "binary_check", "failed")
			a.Cursor++
			d.Attempts = append(d.Attempts, a)
		},
		"binary prompt":     func(d *document) { d.Attempts[0].Message = "prompt" },
		"binary token":      func(d *document) { d.Attempts[0].Token = "token" },
		"binary consumed":   func(d *document) { d.Attempts[0].ConsumedMS = 102 },
		"binary outcome":    func(d *document) { d.Attempts[0].Answer = "submitted" },
		"binary unanswered": func(d *document) { d.Attempts[0].AnsweredMS = 0 },
		"unknown kind":      func(d *document) { d.Attempts[0].Kind = "other" },
		"too many tries": func(d *document) {
			d.Attempts = []attempt{testAttempt(1, "binary_check", "failed"), testAttempt(2, "binary_check", "failed"), testAttempt(3, "binary_check", "failed")}
		},
		"too many records": func(d *document) {
			for n := 2; n <= 6; n++ {
				d.Attempts = append(d.Attempts, testAttempt(n, "binary_check", "aborted"))
			}
		},
	}
	for name, change := range invalid {
		t.Run(name, func(t *testing.T) {
			d := testDocument(binary)
			change(&d)
			if d.valid() {
				t.Fatal("invalid state was granted allowance")
			}
		})
	}
	for name, change := range map[string]func(*attempt){
		"no prompt":            func(a *attempt) { a.Message = "" },
		"no token":             func(a *attempt) { a.Token = "" },
		"no correlation":       func(a *attempt) { a.Message, a.Token = "", "" },
		"outcome without time": func(a *attempt) { a.Answer = "new future answer" },
	} {
		t.Run(name, func(t *testing.T) {
			a := queue
			change(&a)
			if testDocument(a).valid() {
				t.Fatal("uncorrelated queue state accepted")
			}
		})
	}
	if testDocument(queue, testAttempt(2, "binary_check", "failed")).valid() {
		t.Fatal("non-newest outstanding accepted")
	}
	if testDocument(queue, testAttempt(2, "queue", "")).valid() {
		t.Fatal("multiple outstanding accepted")
	}
	unknown := testAttempt(1, "queue", "future-answer")
	if !testDocument(unknown).valid() || !unknown.outstanding() || testDocument(unknown).quota(0) != 2 {
		t.Fatal("unknown queue answer was not conservative")
	}
}

func TestStateQuotaConsumptionAndAbortCompaction(t *testing.T) {
	if testDocument().outstanding() != nil || testDocument().mark().Known || testDocument().quota(0) != 0 {
		t.Fatal("absent state spent allowance")
	}
	one := testAttempt(1, "binary_check", "failed")
	two := testAttempt(2, "binary_check", "failed")
	if testDocument(one).quota(0) != 1 || testDocument(one, two).quota(0) != 2 || testDocument(one, two, one).quota(0) != 2 {
		t.Fatal("pre-submission retry was not bounded")
	}
	for _, answer := range []string{"", "submitted", "stored", "unknown", "future"} {
		a := testAttempt(3, "queue", answer)
		d := testDocument(a)
		if d.quota(0) != 2 || d.outstanding() == nil || d.mark().Stream != "stream" || d.mark().Cursor != 7 {
			t.Fatalf("unsafe queue allowance for %q", answer)
		}
	}
	aborted := testAttempt(3, "queue", "aborted")
	if testDocument(aborted).quota(0) != 0 || testDocument(aborted).outstanding() != nil {
		t.Fatal("certain abort spent a try")
	}
	aborted.ConsumedMS = 103
	if testDocument(aborted).quota(0) != 2 {
		t.Fatal("late abort refunded a consumed wake")
	}
	d := testDocument(one, aborted, testAttempt(4, "binary_check", "aborted"))
	replaced := d.appendAttempt(testAttempt(5, "binary_check", "failed"))
	if len(replaced.Attempts) != 3 || replaced.Attempts[1].ConsumedMS != 103 || len(d.Attempts) != 3 || d.Attempts[2].ID == replaced.Attempts[2].ID {
		t.Fatal("compaction lost consumed quota or changed the unpublished mark")
	}
}

func TestStatePublicationAndAnswersKeepWholeSnapshots(t *testing.T) {
	s := testStore(t)
	before := mustSnapshot(t, s)
	if before.Present || before.Told.Present || before.Document.Version != hook.WakeVersion {
		t.Fatal("new state was not absent")
	}
	a := testAttempt(1, "queue", "")
	result := mustPublish(t, s, before, testDocument(a))
	if !result.Published || result.ID != a.ID {
		t.Fatal("reservation not identifiable")
	}
	got := mustSnapshot(t, s)
	if got.Document.Rev != 1 || got.Document.Attempts[0].Message != a.Message {
		t.Fatal("reservation changed prompt")
	}
	// W2 may consume and compact a prompt before its command returns.
	d := got.Document
	d.Attempts[0].ConsumedMS = 105
	d.Attempts[0].Token, d.Attempts[0].Message = "", ""
	mustPublish(t, s, got, d)
	if err := s.answer(a.ID, "submitted", 106, ""); err != nil {
		t.Fatal(err)
	}
	got = mustSnapshot(t, s)
	if got.Document.Attempts[0].ConsumedMS != 105 || got.Document.quota(0) != 2 {
		t.Fatal("late result undid consumption")
	}
	if err := s.answer(a.ID, "aborted", 107, ""); err != nil {
		t.Fatal(err)
	}
	if mustSnapshot(t, s).Document.quota(0) != 2 {
		t.Fatal("late abort refunded consumption")
	}
	// Answers merge by ID and retain any newer record and its diagnostic.
	d = mustSnapshot(t, s).Document
	b := testAttempt(2, "binary_check", "failed")
	b.Slot = 1
	d = d.appendAttempt(b)
	d.LastFailure = &failure{AtMS: 108, Detail: "newer fixed reason"}
	mustPublish(t, s, mustSnapshot(t, s), d)
	if err := s.answer(a.ID, "failed", 109, "older fixed reason"); err != nil {
		t.Fatal(err)
	}
	got = mustSnapshot(t, s)
	if got.Document.LastFailure.Detail != "newer fixed reason" || len(got.Document.Attempts) != 2 {
		t.Fatal("older result overwrote newer state")
	}
	if err := s.answer(b.ID, "failed", 110, "latest fixed reason"); err != nil {
		t.Fatal(err)
	}
	if mustSnapshot(t, s).Document.LastFailure.Detail != "latest fixed reason" {
		t.Fatal("latest failed result was not recorded")
	}
	if err := s.answer(b.ID, "aborted", 111, ""); err != nil {
		t.Fatal(err)
	}
	if !mustSnapshot(t, s).Document.valid() {
		t.Fatal("tokenless aborted binary state cannot reopen")
	}
	for _, args := range []struct {
		id, outcome string
		now         int64
	}{{"", "aborted", 1}, {a.ID, "bad", 1}, {a.ID, "aborted", 0}} {
		if err := s.answer(args.id, args.outcome, args.now, ""); !errors.Is(err, errStateUnusable) {
			t.Fatalf("invalid answer: %v", err)
		}
	}
	if err := s.answer(strings.Repeat("f", 32), "aborted", 112, ""); !errors.Is(err, errAttemptMissing) {
		t.Fatal(err)
	}
}

func toldPath(s *stateStore) string {
	return filepath.Join(s.sessions.Dir(), filepath.Base(s.path())+".attention")
}

func TestStateChangedToldBytesAndPresenceInvalidateAdmission(t *testing.T) {
	for _, mutation := range []string{"presence", "whole bytes", "same revision twice", "remove"} {
		t.Run(mutation, func(t *testing.T) {
			s := testStore(t)
			before := mustSnapshot(t, s)
			line := "1 0 1 0 73747265616d 7 100 0 9\n"
			if mutation != "presence" {
				if err := os.WriteFile(toldPath(s), []byte(line), 0600); err != nil {
					t.Fatal(err)
				}
				before = mustSnapshot(t, s)
			}
			if mutation == "remove" {
				if err := os.Remove(toldPath(s)); err != nil {
					t.Fatal(err)
				}
			} else {
				if mutation == "whole bytes" {
					line += "extra full-file bytes\n"
				}
				if mutation == "same revision twice" {
					line = strings.Replace(line, "100 0 9", "100 1 9", 1)
				}
				if err := os.WriteFile(toldPath(s), []byte(line), 0600); err != nil {
					t.Fatal(err)
				}
			}
			result, err := s.publish(before, testDocument(testAttempt(1, "queue", "")))
			if !errors.Is(err, errStateChanged) || result.Published {
				t.Fatalf("stale snapshot published: %+v %v", result, err)
			}
			// Once the writer settles a fresh snapshot makes progress without spending a retry.
			mustPublish(t, s, mustSnapshot(t, s), testDocument(testAttempt(2, "queue", "")))
		})
	}
}

func TestStateTwoWritersAdmitExactlyOneReservation(t *testing.T) {
	s := testStore(t)
	first := mustSnapshot(t, s)
	other := newStateStore(s.sessions, s.session)
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for n, writer := range []*stateStore{s, other} {
		wg.Add(1)
		go func(n int, writer *stateStore) {
			defer wg.Done()
			_, err := writer.publish(first, testDocument(testAttempt(n+1, "queue", "")))
			results <- err
		}(n, writer)
	}
	wg.Wait()
	close(results)
	won, lost := 0, 0
	for err := range results {
		if err == nil {
			won++
		} else if errors.Is(err, errStateChanged) {
			lost++
		} else {
			t.Fatal(err)
		}
	}
	if won != 1 || lost != 1 || len(mustSnapshot(t, s).Document.Attempts) != 1 {
		t.Fatalf("two writers admitted %d, refused %d", won, lost)
	}
}

func TestStateUnusableFilesAreNeverReset(t *testing.T) {
	for _, body := range []string{"broken", `{"version":9,"rev":0,"attempts":[]}`, `{"rev":0,"attempts":[]}`, `{"version":null,"rev":0,"attempts":[]}`, `{"version":1,"attempts":[]}`, `{"version":1,"rev":null,"attempts":[]}`, `{"version":1,"rev":0}`, `{"version":1,"rev":0,"attempts":null}`, `{"version":1,"rev":0,"attempts":[{"kind":"queue"}]}`} {
		t.Run(body, func(t *testing.T) {
			s := testStore(t)
			if err := os.MkdirAll(filepath.Dir(s.path()), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(s.path(), []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := s.snapshot(); !errors.Is(err, errStateUnusable) {
				t.Fatal(err)
			}
			if err := s.answer("id", "unknown", 1, ""); !errors.Is(err, errStateUnusable) {
				t.Fatal(err)
			}
			got, err := os.ReadFile(s.path())
			if err != nil || string(got) != body {
				t.Fatal("unusable guard was changed")
			}
		})
	}
	s := testStore(t)
	s.io.readFile = func(string) ([]byte, error) { return nil, errStateTest }
	if _, err := s.snapshot(); !errors.Is(err, errStateTest) {
		t.Fatal(err)
	}
	s = testStore(t)
	_ = mustSnapshot(t, s)
	if err := os.Mkdir(toldPath(s), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := s.snapshot(); err == nil {
		t.Fatal("unreadable told file treated as absent")
	}
	s = testStore(t)
	if err := os.WriteFile(s.sessions.Dir(), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.snapshot(); err == nil {
		t.Fatal("unusable sessions directory treated as fresh")
	}
	if _, _, err := s.prepare(testDocument(testAttempt(1, "queue", ""))); err == nil {
		t.Fatal("unusable directory prepared a reservation")
	}
	s = testStore(t)
	_ = mustSnapshot(t, s)
	if err := os.Remove(filepath.Join(s.sessions.Dir(), ".lock")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(s.sessions.Dir(), ".lock"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := s.snapshot(); err == nil {
		t.Fatal("unusable shared lock did not block snapshot")
	}
}

func TestStateSemanticDamageBlocksReplacementWithoutChangingBytes(t *testing.T) {
	for _, damage := range []string{"mixed identity", "duplicate id", "multiple outstanding", "non-newest outstanding", "prototype"} {
		t.Run(damage, func(t *testing.T) {
			s := testStore(t)
			before := mustSnapshot(t, s)
			a, b := testAttempt(1, "binary_check", "failed"), testAttempt(2, "binary_check", "failed")
			switch damage {
			case "mixed identity":
				b.Stream = "other"
			case "duplicate id":
				b.ID = a.ID
			case "multiple outstanding":
				a, b = testAttempt(1, "queue", ""), testAttempt(2, "queue", "")
			case "non-newest outstanding":
				a = testAttempt(1, "queue", "")
			case "prototype":
				a.ID = ""
				a.Kind = ""
			}
			writeState(t, s, testDocument(a, b))
			raw, err := os.ReadFile(s.path())
			if err != nil {
				t.Fatal(err)
			}
			result, err := s.publish(before, testDocument(testAttempt(3, "queue", "")))
			if !errors.Is(err, errStateUnusable) || result.Published {
				t.Fatalf("damaged state was replaced: %+v %v", result, err)
			}
			after, err := os.ReadFile(s.path())
			if err != nil || string(after) != string(raw) {
				t.Fatal("refusal changed guard")
			}
		})
	}
}

func TestStateFailedAbortLeavesUnansweredQueueOutstandingAcrossRestart(t *testing.T) {
	s := testStore(t)
	before := mustSnapshot(t, s)
	a := testAttempt(1, "queue", "")
	read := s.io.readFile
	calls := 0
	s.io.readFile = func(path string) ([]byte, error) {
		calls++
		if calls == 2 {
			return nil, errStateTest
		}
		return read(path)
	}
	result, err := s.publish(before, testDocument(a))
	if err == nil || !result.Published || result.ID != a.ID {
		t.Fatalf("uncertain reservation lost its ID: %+v %v", result, err)
	}
	s.io.readFile = read
	s.io.rename = func(string, string) error { return errStateTest }
	if err := s.answer(a.ID, "aborted", 105, ""); err == nil {
		t.Fatal("failed abort was hidden")
	}
	reopened := newStateStore(s.sessions, s.session)
	d := mustSnapshot(t, reopened).Document
	if d.outstanding() == nil || d.outstanding().ID != a.ID || d.quota(0) != 2 {
		t.Fatal("uncertain queue reservation was reset after failed abort")
	}
}

type faultyStateFile struct {
	stateFile
	writeErr, syncErr, closeErr error
	short                       bool
}

func (f faultyStateFile) Write(p []byte) (int, error) {
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	if f.short {
		return len(p) - 1, nil
	}
	return f.stateFile.Write(p)
}
func (f faultyStateFile) Sync() error {
	if f.syncErr != nil {
		return f.syncErr
	}
	return f.stateFile.Sync()
}
func (f faultyStateFile) Close() error {
	err := f.stateFile.Close()
	if f.closeErr != nil {
		return f.closeErr
	}
	return err
}

func TestStatePreparationFailureLeavesNoReservationOrTemporaryFile(t *testing.T) {
	for _, boundary := range []string{"sessions protection", "wake protection", "create", "protect file", "write", "short write", "sync", "close"} {
		t.Run(boundary, func(t *testing.T) {
			s := testStore(t)
			before := mustSnapshot(t, s)
			create := s.io.createTemp
			switch boundary {
			case "sessions protection", "wake protection":
				calls := 0
				real := s.io.protectDir
				s.io.protectDir = func(path string) error {
					calls++
					if (boundary == "sessions protection" && calls == 1) || (boundary == "wake protection" && calls == 2) {
						return errStateTest
					}
					return real(path)
				}
			case "create":
				s.io.createTemp = func(string, string) (stateFile, error) { return nil, errStateTest }
			case "protect file":
				s.io.protectFile = func(string) error { return errStateTest }
			default:
				s.io.createTemp = func(dir, pattern string) (stateFile, error) {
					f, err := create(dir, pattern)
					if err != nil {
						return nil, err
					}
					broken := faultyStateFile{stateFile: f}
					switch boundary {
					case "write":
						broken.writeErr = errStateTest
					case "short write":
						broken.short = true
					case "sync":
						broken.syncErr = errStateTest
					case "close":
						broken.closeErr = errStateTest
					}
					return broken, nil
				}
			}
			result, err := s.publish(before, testDocument(testAttempt(1, "queue", "")))
			if err == nil || result.Published {
				t.Fatalf("failed %s published %+v %v", boundary, result, err)
			}
			if boundary == "short write" && !errors.Is(err, io.ErrShortWrite) {
				t.Fatal(err)
			}
			if _, err := os.Stat(s.path()); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("failed preparation left guard")
			}
			entries, _ := os.ReadDir(filepath.Dir(s.path()))
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), ".tmp-") {
					t.Fatal("failed preparation leaked temporary")
				}
			}
		})
	}
	s := testStore(t)
	if _, _, err := s.prepare(document{Version: 99}); !errors.Is(err, errStateUnusable) {
		t.Fatal("invalid state prepared")
	}
	name, _, err := s.prepare(testDocument())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(name); err != nil {
		t.Fatal(err)
	}
	if err := s.io.protectFile(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Fatal("missing file protection succeeded")
	}
}

func TestStatePublicationFailureIdentifiesWhetherTheExactIDMayExist(t *testing.T) {
	for _, boundary := range []string{"lock", "snapshot", "changed wake", "rename", "readback error", "readback mismatch", "directory sync"} {
		t.Run(boundary, func(t *testing.T) {
			s := testStore(t)
			before := mustSnapshot(t, s)
			a := testAttempt(2, "queue", "")
			read := s.io.readFile
			switch boundary {
			case "lock":
				create := s.io.createTemp
				s.io.createTemp = func(dir, pattern string) (stateFile, error) {
					f, err := create(dir, pattern)
					if err == nil {
						lock := filepath.Join(s.sessions.Dir(), ".lock")
						if err = os.Remove(lock); err == nil {
							err = os.Mkdir(lock, 0700)
						}
					}
					return f, err
				}
			case "snapshot":
				s.io.readFile = func(string) ([]byte, error) { return nil, errStateTest }
			case "changed wake":
				writeState(t, s, testDocument(testAttempt(1, "binary_check", "aborted")))
			case "rename":
				s.io.rename = func(string, string) error { return errStateTest }
			case "readback error", "readback mismatch":
				calls := 0
				s.io.readFile = func(path string) ([]byte, error) {
					calls++
					if calls == 2 {
						if boundary == "readback error" {
							return nil, errStateTest
						}
						return []byte("unexpected read-back"), nil
					}
					return read(path)
				}
			case "directory sync":
				s.io.syncDir = func(string) error {
					unlock, err := s.sessions.Lock(20 * time.Millisecond)
					if err != nil {
						t.Fatal("directory sync held sessions lock")
					}
					unlock()
					return errStateTest
				}
			}
			result, err := s.publish(before, testDocument(a))
			published := strings.HasPrefix(boundary, "readback") || boundary == "directory sync"
			if err == nil || result.Published != published || result.ID != a.ID {
				t.Fatalf("wrong publication result: %+v %v", result, err)
			}
		})
	}
}

func TestStateAbortTargetsF2WithoutConfusingTokenlessF1(t *testing.T) {
	s := testStore(t)
	f1, f2 := testAttempt(1, "binary_check", "failed"), testAttempt(2, "binary_check", "failed")
	mustPublish(t, s, mustSnapshot(t, s), testDocument(f1))
	before := mustSnapshot(t, s)
	s.io.syncDir = func(string) error { return errStateTest }
	publication, err := s.publish(before, before.Document.appendAttempt(f2))
	if err == nil || !publication.Published || publication.ID != f2.ID {
		t.Fatalf("F2 boundary lost identity: %+v %v", publication, err)
	}
	s.io.syncDir = syncDirectory
	// Abort persistence itself can fail. Both known binary failures stay counted.
	s.io.rename = func(string, string) error { return errStateTest }
	if err := s.answer(f2.ID, "aborted", 110, ""); err == nil {
		t.Fatal("abort write failure hidden")
	}
	if got := mustSnapshot(t, s).Document; got.quota(0) != 2 || got.Attempts[0].Answer != "failed" || got.Attempts[1].Answer != "failed" {
		t.Fatal("abort failure refunded or modified F1")
	}
	s.io.rename = os.Rename
	if err := s.answer(f2.ID, "aborted", 111, ""); err != nil {
		t.Fatal(err)
	}
	reopened := newStateStore(s.sessions, s.session)
	got := mustSnapshot(t, reopened).Document
	if got.quota(0) != 1 || got.Attempts[0].Answer != "failed" || got.Attempts[1].Answer != "aborted" {
		t.Fatal("tokenless abort cannot reopen safely")
	}
	for n := 3; n < 40; n++ {
		before := mustSnapshot(t, reopened)
		a := testAttempt(n, "queue", "")
		mustPublish(t, reopened, before, before.Document.appendAttempt(a))
		if err := reopened.answer(a.ID, "aborted", int64(200+n), ""); err != nil {
			t.Fatal(err)
		}
		reopened = newStateStore(s.sessions, s.session)
		got = mustSnapshot(t, reopened).Document
		if len(got.Attempts) != 2 || got.quota(0) != 1 || got.mark().Cursor != 7 {
			t.Fatal("aborts grew state or reset spent quota/mark")
		}
	}
}

func TestStateAnswerRetriesMergeConsumptionAndIgnoreUnrelatedToldChanges(t *testing.T) {
	s := testStore(t)
	a := testAttempt(1, "queue", "")
	mustPublish(t, s, mustSnapshot(t, s), testDocument(a))
	realProtect := s.io.protectFile
	s.io.protectFile = func(path string) error {
		got := mustSnapshot(t, s).Document
		got.Attempts[0].ConsumedMS = 105
		writeState(t, s, got)
		return realProtect(path)
	}
	if err := s.answer(a.ID, "unknown", 106, ""); !errors.Is(err, errStateChanged) {
		t.Fatal("concurrent consumption was overwritten", err)
	}
	s.io.protectFile = func(path string) error {
		if err := os.WriteFile(toldPath(s), []byte("new told bytes"), 0600); err != nil {
			return err
		}
		return realProtect(path)
	}
	if err := s.answer(a.ID, "unknown", 107, ""); err != nil {
		t.Fatal(err)
	}
	if mustSnapshot(t, s).Document.Attempts[0].ConsumedMS != 105 {
		t.Fatal("answer lost consumed mark")
	}
}

func TestStateRetiredIdentityAnswerCannotRecreateItsLedger(t *testing.T) {
	s := testStore(t)
	a := testAttempt(1, "queue", "")
	mustPublish(t, s, mustSnapshot(t, s), testDocument(a))
	// Restoring the hub or changing the proposed identity does not clear a persisted guard.
	reopened := newStateStore(s.sessions, s.session)
	guard := mustSnapshot(t, reopened).Document.outstanding()
	if guard == nil || guard.ID != a.ID || guard.Stream != "stream" {
		t.Fatal("restart lost outstanding old-stream reservation")
	}
	d := mustSnapshot(t, reopened).Document
	d.Attempts[0].ConsumedMS = 105
	writeState(t, reopened, d) // the exact prompt hook consumed A
	b := testAttempt(2, "queue", "")
	b.Stream = "restored-stream"
	b.Cursor = 1
	mustPublish(t, reopened, mustSnapshot(t, reopened), testDocument(b))
	before := mustSnapshot(t, reopened)
	if err := reopened.answer(a.ID, "submitted", 106, ""); !errors.Is(err, errAttemptMissing) {
		t.Fatal("retired result did not remain benign", err)
	}
	after := mustSnapshot(t, reopened)
	if before.Raw != after.Raw || after.Document.outstanding().ID != b.ID || after.Document.mark().Stream != "restored-stream" {
		t.Fatal("late retired result changed the new ledger")
	}
}
