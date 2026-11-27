package hook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/mateusz-klatt/iris/internal/config"
	"github.com/mateusz-klatt/iris/internal/hub"
	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/store"
)

func newFileMemory(t *testing.T) fileMemory {
	t.Helper()
	return fileMemory{dir: filepath.Join(t.TempDir(), "sessions"), now: time.Now, restrict: config.OwnerOnly}
}

// Recall inspects the on-disk generation without refreshing it.
func (m fileMemory) Recall(client, session string) int {
	generation, _ := readGeneration(m.name(client, session))
	return generation
}

func TestFileMemoryRemembersEachSessionApart(t *testing.T) {
	m := newFileMemory(t)
	if m.Recall("claude", "s") != 0 {
		t.Fatal("an unknown session has a generation")
	}
	m.Remember("claude", "s", 3)
	m.Remember("codex", "s", 7)
	if m.Recall("claude", "s") != 3 || m.Recall("codex", "s") != 7 {
		t.Fatal(m.Recall("claude", "s"), m.Recall("codex", "s"))
	}
	m.Remember("claude", "s", 4)
	if m.Recall("claude", "s") != 4 {
		t.Fatal("a later generation replaces the earlier one")
	}
	m.Forget("claude", "s", 4)
	if m.Recall("claude", "s") != 0 || m.Recall("codex", "s") != 7 {
		t.Fatal("forget removed the wrong session")
	}
	// Windows has no mode bits: the files take the ACL of the user's configuration directory.
	if runtime.GOOS == "windows" {
		return
	}
	for _, info := range mustList(t, m.dir) {
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("%s is %v", info.Name(), info.Mode())
		}
	}
	if dir, err := os.Stat(m.dir); err != nil || dir.Mode().Perm() != 0o700 {
		t.Fatal(dir, err)
	}
}

func TestFileMemoryChecksForNewerProofWhenTheWriteAcquiresItsLock(t *testing.T) {
	m := newFileMemory(t)
	m.Remember("claude", "s", 1)
	protecting, proceed, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	late := m
	late.restrict = func(path string) error {
		if path == m.dir {
			close(protecting)
			<-proceed
		}
		return config.OwnerOnly(path)
	}
	go func() {
		defer close(done)
		late.Remember("claude", "s", 2)
	}()
	<-protecting
	m.Remember("claude", "s", 3)
	close(proceed)
	<-done
	if got := m.Recall("claude", "s"); got != 3 {
		t.Fatalf("delayed G2 write replaced G3 with G%d", got)
	}
	m.Forget("claude", "s", 2)
	if got := m.Recall("claude", "s"); got != 3 {
		t.Fatalf("successful G2 end removed G3, now G%d", got)
	}
	unlock, err := m.lock(hookLockBudget)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.RecallGeneration("claude", "s"); !errors.Is(err, failSessionsLock) {
		t.Errorf("proof was renewed without the lock: %v", err)
	}
	m.Remember("claude", "s", 4)
	m.Forget("claude", "s", 3)
	unlock()
	if got := m.Recall("claude", "s"); got != 3 {
		t.Fatalf("an operation whose lock timed out changed G3 to G%d", got)
	}
}

func mustList(t *testing.T, dir string) []os.FileInfo {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []os.FileInfo
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, info)
	}
	return out
}

func TestDamagedProofCannotBeReadOrOverwritten(t *testing.T) {
	m := newFileMemory(t)
	m.Remember("claude", "s", 1)
	path := m.name("claude", "s")
	for _, body := range []string{"", "x", "0", "-2", "3\nx"} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if generation, err := m.RecallGeneration("claude", "s"); generation != 0 || !errors.Is(err, ErrInvalidGeneration) {
			t.Fatalf("read %q as generation %d with %v", body, generation, err)
		}
		m.Remember("claude", "s", 1)
		if got, err := os.ReadFile(path); string(got) != body || err != nil {
			t.Fatalf("Remember replaced %q with %q, %v", body, got, err)
		}
	}
}

func TestEndMarkerGuardsPrunedProofAndAgesOut(t *testing.T) {
	m := newFileMemory(t)
	path := m.name("claude", "s")
	prune := func(path string) {
		t.Helper()
		old := m.now().Add(-memoryAge - time.Hour)
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
		if err := m.prune(); err != nil {
			t.Fatal(err)
		}
	}

	m.Remember("claude", "s", 3)
	prune(path)
	m.Forget("claude", "s", 3) // The hub accepted the end after local proof was pruned.
	m.Forget("claude", "s", 2) // A delayed older end must not lower the marker.
	m.retainGeneration("claude", "s", 3)
	if gen, err := readGeneration(path); gen != 0 || err != nil {
		t.Fatalf("the refusal revived ended proof: G%d, %v", gen, err)
	}

	m.Remember("claude", "s", 4)
	prune(path)
	m.retainGeneration("claude", "s", 4)
	if gen, err := readGeneration(path); gen != 4 || err != nil {
		t.Fatalf("the refusal lost newer proof: G%d, %v", gen, err)
	}
	prune(path + ".ended")
	if gen, err := readGeneration(path + ".ended"); gen != 0 || err != nil {
		t.Fatalf("the old end marker survived pruning: G%d, %v", gen, err)
	}
	if gen, err := readGeneration(path); gen != 4 || err != nil {
		t.Fatalf("pruning the marker changed live proof: G%d, %v", gen, err)
	}
}

func TestEndMarkerVetoesDelayedRegistrationReplies(t *testing.T) {
	m := newFileMemory(t)
	m.Remember("claude", "s", 3)
	m.Forget("claude", "s", 3)
	for _, generation := range []int{2, 3} {
		m.Remember("claude", "s", generation)
		if got := m.Recall("claude", "s"); got != 0 {
			t.Fatalf("a late G%d reply brought back G%d proof", generation, got)
		}
	}
	m.Remember("claude", "s", 4)
	if got, err := m.RecallGeneration("claude", "s"); got != 4 || err != nil {
		t.Fatalf("newer hook registration proof = G%d, %v", got, err)
	}

	m.Remember("claude", "recall-ended", 2)
	m.Forget("claude", "recall-ended", 2)
	if err := os.WriteFile(m.name("claude", "recall-ended"), []byte("2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var endedErr endedProof
	if _, err := m.RecallGeneration("claude", "recall-ended"); !errors.As(err, &endedErr) {
		t.Fatalf("RecallGeneration on ended proof = %v, want endedProof", err)
	}
	marker := m.name("claude", "recall-ended") + ".ended"
	if err := os.WriteFile(marker, []byte("corrupt\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var damagedErr damagedEndMarker
	if _, err := m.RecallGeneration("claude", "recall-ended"); !errors.As(err, &damagedErr) || damagedErr.Error() != damagedGenerationObservation(marker) {
		t.Fatalf("RecallGeneration on damaged .ended = %v, want damagedEndMarker", err)
	}
}

func TestForgetKeepsProofWhenEndMarkerCannotBeRecorded(t *testing.T) {
	const (
		client  = "claude"
		session = "s"
	)
	for _, tc := range []struct {
		name    string
		prepare func(t *testing.T, m *fileMemory, marker string)
		verify  func(t *testing.T, marker string)
	}{
		{
			name: "malformed marker",
			prepare: func(t *testing.T, _ *fileMemory, marker string) {
				if err := os.WriteFile(marker, []byte("damaged\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			verify: func(t *testing.T, marker string) {
				if raw, err := os.ReadFile(marker); err != nil || string(raw) != "damaged\n" {
					t.Fatalf("marker was replaced with %q, %v", raw, err)
				}
			},
		},
		{
			name: "unreadable marker",
			prepare: func(t *testing.T, _ *fileMemory, marker string) {
				if err := os.MkdirAll(filepath.Join(marker, "keep"), 0o700); err != nil {
					t.Fatal(err)
				}
			},
			verify: func(t *testing.T, marker string) {
				if info, err := os.Stat(marker); err != nil || !info.IsDir() {
					t.Fatalf("marker directory was replaced: %v, %v", info, err)
				}
			},
		},
		{
			name: "marker write fails when absent",
			prepare: func(_ *testing.T, m *fileMemory, _ string) {
				m.restrict = func(string) error { return errors.New("acl") }
			},
			verify: func(t *testing.T, marker string) {
				if _, err := os.Lstat(marker); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("unexpected marker after failed write: %v", err)
				}
			},
		},
		{
			name: "marker write fails with prior marker",
			prepare: func(t *testing.T, m *fileMemory, marker string) {
				if err := os.WriteFile(marker, []byte("2\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				m.restrict = func(string) error { return errors.New("acl") }
			},
			verify: func(t *testing.T, marker string) {
				if raw, err := os.ReadFile(marker); err != nil || string(raw) != "2\n" {
					t.Fatalf("prior marker was replaced with %q, %v", raw, err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newFileMemory(t)
			m.Remember(client, session, 3)
			path := m.name(client, session)
			marker := path + ".ended"
			tc.prepare(t, &m, marker)

			if damaged := m.forget(client, session, 3); damaged != marker {
				t.Fatalf("forget reported damaged=%q, want %q", damaged, marker)
			}

			if gen, err := readGeneration(path); gen != 3 || err != nil {
				t.Fatalf("proof after failed Forget = G%d, %v; want G3", gen, err)
			}
			tc.verify(t, marker)
			for _, info := range mustList(t, m.dir) {
				if strings.HasPrefix(info.Name(), ".tmp-") {
					t.Fatalf("failed Forget left temporary file %s", info.Name())
				}
			}
		})
	}
}

func TestForgetNamesTheMarkerWhenTheSessionsLockIsMissed(t *testing.T) {
	m := newFileMemory(t)
	m.Remember("claude", "s", 3)
	path := m.name("claude", "s")
	marker := path + ".ended"
	unlock, err := m.lock(hookLockBudget)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(unlock)
	if got := m.forget("claude", "s", 3); got != marker {
		t.Fatalf("forget named %q, want the marker %q", got, marker)
	}
	if gen, err := readGeneration(path); gen != 3 || err != nil {
		t.Fatalf("a missed lock changed proof to G%d, %v", gen, err)
	}
	if _, err := os.Lstat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a missed lock created a marker: %v", err)
	}
	if got := m.forget("claude", "s", 0); got != "" {
		t.Fatalf("a generation of 0 named %q", got)
	}
}

func TestRetainGenerationRefusesAndReportsDamagedEndMarker(t *testing.T) {
	const (
		client  = "claude"
		session = "s"
	)
	for _, unreadable := range []bool{false, true} {
		name := "malformed marker"
		if unreadable {
			name = "directory marker"
		}
		t.Run(name, func(t *testing.T) {
			m := newFileMemory(t)
			if err := m.directory(); err != nil {
				t.Fatal(err)
			}
			path := m.name(client, session)
			marker := path + ".ended"
			if unreadable {
				if err := os.Mkdir(marker, 0o700); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(marker, []byte("damaged\n"), 0o600); err != nil {
				t.Fatal(err)
			}

			if marker := m.retainGeneration(client, session, 4); marker.Path != path+".ended" || !marker.Damaged {
				t.Fatalf("retainGeneration reported %+v, want the damaged marker", marker)
			}
			if gen, err := readGeneration(path); gen != 0 || err != nil {
				t.Fatalf("retainGeneration restored proof over damaged marker: G%d, %v", gen, err)
			}
		})
	}
}

func TestRetainGenerationNamesAMarkerThatContributed(t *testing.T) {
	const (
		client  = "claude"
		session = "s"
	)
	m := newFileMemory(t)
	if err := m.directory(); err != nil {
		t.Fatal(err)
	}
	path := m.name(client, session)
	marker := path + ".ended"
	absent := filepath.Join(m.dir, "not-there")
	if gen, err := readGeneration(absent); gen != 0 || err != nil {
		t.Fatalf("absent path = G%d, %v; want (0, nil)", gen, err)
	}
	if err := os.WriteFile(marker, []byte("5\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Proof is absent and the marker is at least this generation, so the end still rules it out.
	if got := m.retainGeneration(client, session, 4); got.Path != marker || got.Damaged {
		t.Fatalf("blocking marker = %+v", got)
	}
	if gen, err := readGeneration(path); gen != 0 || err != nil {
		t.Fatalf("retainGeneration restored ended proof: G%d, %v", gen, err)
	}
	// A newer generation is still restored, and the marker that allowed it is named.
	if got := m.retainGeneration(client, session, 6); got.Path != marker || got.Damaged {
		t.Fatalf("allowing marker = %+v", got)
	}
	if gen, err := readGeneration(path); gen != 6 || err != nil {
		t.Fatalf("retainGeneration lost newer proof: G%d, %v", gen, err)
	}
	// A later refusal finds the proof already present. The marker is still named, and the proof stays.
	if got := m.retainGeneration(client, session, 6); got.Path != marker || got.Damaged {
		t.Fatalf("repeated refusal marker = %+v", got)
	}
	if raw, err := os.ReadFile(path); err != nil || strings.TrimSpace(string(raw)) != "6" {
		t.Fatalf("repeated refusal rewrote proof to %q, %v", raw, err)
	}
	if raw, err := os.ReadFile(marker); err != nil || string(raw) != "5\n" {
		t.Fatalf("repeated refusal rewrote the marker to %q, %v", raw, err)
	}
}

func TestDanglingEndedMarkerIsDamageAndOnlyAnAbsentPathIsEmpty(t *testing.T) {
	m := newFileMemory(t)
	if err := m.directory(); err != nil {
		t.Fatal(err)
	}
	path := m.name("claude", "s")
	marker := path + ".ended"
	absent := filepath.Join(m.dir, "missing-ended")
	if gen, err := readGeneration(absent); gen != 0 || err != nil {
		t.Fatalf("absent path = G%d, %v; want (0, nil)", gen, err)
	}
	symlinkToMissing(t, marker)
	if gen, err := readGeneration(marker); gen != 0 || !errors.Is(err, ErrInvalidGeneration) {
		t.Fatalf("dangling marker = G%d, %v; want invalid generation", gen, err)
	}
	if got := m.retainGeneration("claude", "s", 4); got.Path != marker || !got.Damaged {
		t.Fatalf("retainGeneration reported %+v for a dangling marker", got)
	}
	if gen, err := readGeneration(path); gen != 0 || err != nil {
		t.Fatalf("retainGeneration restored proof over a dangling marker: G%d, %v", gen, err)
	}
	if info, err := os.Lstat(marker); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("retainGeneration replaced the dangling marker: %v, %v", info, err)
	}
	m.Remember("claude", "s", 3)
	if got := m.forget("claude", "s", 3); got != marker {
		t.Fatalf("forget named %q for a dangling marker, want %q", got, marker)
	}
	if gen, err := readGeneration(path); gen != 3 || err != nil {
		t.Fatalf("Forget removed proof despite a dangling marker: G%d, %v", gen, err)
	}
	if info, err := os.Lstat(marker); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("Forget replaced the dangling marker: %v, %v", info, err)
	}
}

func TestRepeatedRefusalObservationNamesTheEndMarker(t *testing.T) {
	root := gitRepo(t)
	getenv := failureEnv(t)
	m := sessionsFor(getenv)
	m.Remember("claude", "s", 4)
	marker := m.name("claude", "s") + ".ended"
	if err := os.WriteFile(marker, []byte("2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := &recoveryHub{generation: 1}
	out := Runner{
		ClientName: "claude", EventName: "PreToolUse", Stdin: strings.NewReader(writePayload(root, "s", "a.go")),
		Hub: h, Getenv: getenv, after: never, logAfter: never,
	}.Run()
	assertAllow(t, ClientClaude, out)
	if h.touched != 0 || len(h.sent) != 1 || h.sent[0] != 4 {
		t.Fatalf("refusal sent %+v and touched %d", h.sent, h.touched)
	}
	if got := m.Recall("claude", "s"); got != 4 {
		t.Fatalf("repeated refusal changed proof to G%d", got)
	}
	if raw, err := os.ReadFile(marker); err != nil || string(raw) != "2\n" {
		t.Fatalf("marker after refusal = %q, %v", raw, err)
	}
	notes, err := ReadRecentObservations(getenv("IRIS_TOKEN_FILE"), 10)
	if err != nil || len(notes) != 1 || notes[0].Error != RestoreFloorRefused+" at "+marker {
		t.Fatalf("observations %+v, %v", notes, err)
	}
}

func TestDamagedEndMarkerAllowsPreToolForEveryClient(t *testing.T) {
	for _, client := range []Client{ClientClaude, ClientCodex, ClientCopilot, ClientCursor, ClientAntigravity, ClientGrok, ClientKimi} {
		t.Run(string(client), func(t *testing.T) {
			root := gitRepo(t)
			getenv := failureEnv(t)
			m := sessionsFor(getenv)
			m.Remember(string(client), "s1", 4)
			proof := m.name(string(client), "s1")
			marker := proof + ".ended"
			if err := os.WriteFile(marker, []byte("also damaged\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			h := &recoveryHub{generation: 1}
			out := Runner{
				ClientName: string(client), EventName: "PreToolUse", Stdin: strings.NewReader(editPayload(client, root)),
				Hub: h, Memory: m, Getenv: getenv, after: never, logAfter: never,
			}.Run()
			assertAllow(t, client, out)
			if h.touched != 0 || len(h.sent) != 0 {
				t.Fatalf("damaged marker allowed registration or touch: requests %v, touches %d", h.sent, h.touched)
			}
			if raw, err := os.ReadFile(proof); err != nil || string(raw) != "4\n" {
				t.Fatalf("proof = %q, %v", raw, err)
			}
			if raw, err := os.ReadFile(marker); err != nil || string(raw) != "also damaged\n" {
				t.Fatalf("marker = %q, %v", raw, err)
			}
			notes, err := ReadRecentObservations(getenv("IRIS_TOKEN_FILE"), 10) // gitleaks:allow (a test observation token and path, not a credential)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, note := range notes {
				got = append(got, note.Error)
			}
			want := []string{"damaged generation proof at " + marker} // gitleaks:allow (a test observation token and path, not a credential)
			if !slices.Equal(got, want) {
				t.Fatalf("observations = %v, want %v", got, want)
			}
			failures, err := ReadRecentFailures(getenv("IRIS_TOKEN_FILE"), 10)
			if err != nil || len(failures) != 0 {
				t.Fatalf("damaged marker became a hook failure: %+v, %v", failures, err)
			}
		})
	}
}

func TestFileMemoryFailsQuietly(t *testing.T) {
	blocked := t.TempDir()
	file := filepath.Join(blocked, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	m := fileMemory{dir: filepath.Join(file, "sessions"), now: time.Now, restrict: config.OwnerOnly}
	if err := m.write(m.name("claude", "s"), "1"); err == nil {
		t.Fatal("a write reported success without a usable directory")
	}
	m.Remember("claude", "s", 1)
	m.Forget("claude", "s", 1)
	if err := m.prune(); err != nil {
		t.Fatalf("a directory that cannot be listed has nothing to prune: %v", err)
	}
	if m.Recall("claude", "s") != 0 {
		t.Fatal("nothing could have been stored")
	}

	// The directory can also vanish after it was prepared, before the temporary file opens.
	m = newFileMemory(t)
	m.restrict = os.Remove
	if err := m.write(m.name("claude", "s"), "1"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a write whose directory vanished returned %v", err)
	}
	if m.Recall("claude", "s") != 0 {
		t.Fatal("the vanished directory took a write")
	}

	// The shared file replacement rejects a directory target and cleans up its temporary file.
	m = newFileMemory(t)
	if err := os.MkdirAll(filepath.Join(m.name("claude", "s"), "keep"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := m.write(m.name("claude", "s"), "1"); err == nil {
		t.Fatal("replaced a directory with a generation file")
	}
	for _, info := range mustList(t, m.dir) {
		if strings.HasPrefix(info.Name(), ".tmp-") {
			t.Fatal("a failed write left", info.Name())
		}
	}

	// A mode of 0500 refuses a write only on POSIX, and never for root.
	if runtime.GOOS != "windows" && os.Getuid() != 0 {
		readOnly := newFileMemory(t)
		if err := os.MkdirAll(readOnly.dir, 0o500); err != nil {
			t.Fatal(err)
		}
		readOnly.Remember("claude", "s", 1)
		if readOnly.Recall("claude", "s") != 0 {
			t.Fatal("a read-only directory took a write")
		}
	}
}

func TestFileMemoryPrunesOnlyStaleEntries(t *testing.T) {
	now := time.Now()
	m := fileMemory{dir: filepath.Join(t.TempDir(), "sessions"), now: func() time.Time { return now }, restrict: config.OwnerOnly}
	m.Remember("claude", "old", 1)
	m.Remember("claude", "new", 2)
	old := now.Add(-memoryAge - time.Hour)
	if err := os.Chtimes(m.name("claude", "old"), old, old); err != nil {
		t.Fatal(err)
	}
	if err := m.prune(); err != nil {
		t.Fatal(err)
	}
	if m.Recall("claude", "old") != 0 || m.Recall("claude", "new") != 2 {
		t.Fatal("prune removed the wrong entry")
	}
}

func TestMemoryLivesBesideTheToken(t *testing.T) {
	token := filepath.Join(t.TempDir(), "hub.token")
	r := Runner{Getenv: func(k string) string {
		if k == "IRIS_TOKEN_FILE" {
			return token
		}
		return ""
	}}
	m, ok := r.memory().(fileMemory)
	if !ok || m.dir != filepath.Join(filepath.Dir(token), "sessions") || m.restrict == nil {
		t.Fatalf("%#v", r.memory())
	}
	if (Runner{}).memory() != nil {
		t.Fatal("a Runner without an environment has nowhere private to write")
	}
	given := &recoveryHub{}
	if got, ok := (Runner{Memory: fakeMemory{given}}).memory().(fakeMemory); !ok || got.h != given {
		t.Fatal("an injected Memory was ignored")
	}
	for _, key := range []string{"HOME", "XDG_CONFIG_HOME", "AppData"} {
		t.Setenv(key, "")
	}
	if (Runner{Getenv: func(string) string { return "" }}).memory() != nil {
		t.Fatal("no token path, so no memory")
	}
}

type fakeMemory struct{ h *recoveryHub }

func (fakeMemory) RecallGeneration(string, string) (int, error) { return 0, nil }
func (fakeMemory) Remember(string, string, int)                 {}
func (fakeMemory) Forget(string, string, int)                   {}

type recordedMemory struct {
	forgotten []int
}

func (m *recordedMemory) RecallGeneration(string, string) (int, error) { return 0, nil }
func (m *recordedMemory) Remember(string, string, int)                 {}
func (m *recordedMemory) Forget(_ string, _ string, generation int) {
	m.forgotten = append(m.forgotten, generation)
}

func TestSessionEndCallsInjectedMemoryForget(t *testing.T) {
	root := gitRepo(t)
	h := &recoveryHub{generation: 4}
	m := &recordedMemory{}
	out := runWith(h, m, "SessionEnd", `{"session_id":"s","cwd":`+quote(root)+`}`)
	assertAllow(t, ClientClaude, out)
	if h.ended != 1 || !slices.Equal(m.forgotten, []int{4}) {
		t.Fatalf("ended=%d forgotten=%v, want ended=1 forgotten=[4]", h.ended, m.forgotten)
	}
}

func lostWithHighest(highest int) *hubclient.Error {
	return &hubclient.Error{Code: "lost", Details: json.RawMessage(fmt.Sprintf(`{"highest_generation":%d}`, highest))}
}

// recoveryHub plays the hub's side of D9: an Execution in recovery refuses a request
// without its generation, and a stale generation is refused outright.
type recoveryHub struct {
	mu         sync.Mutex
	generation int
	recovering bool
	sent       []int
	retries    []int
	touched    int
	ended      int
}

func (h *recoveryHub) RegisterSession(_ context.Context, r hubclient.SessionRequest) (hubclient.SessionResponse, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sent = append(h.sent, r.Generation)
	h.retries = append(h.retries, r.RetryGeneration)
	switch {
	case r.Generation != 0 && r.Generation != h.generation:
		return hubclient.SessionResponse{}, lostWithHighest(h.generation)
	case h.recovering && r.Generation == 0:
		return hubclient.SessionResponse{}, lostWithHighest(h.generation)
	}
	h.recovering = false
	return hubclient.SessionResponse{ExecutionID: "ex", Generation: h.generation}, nil
}

func (h *recoveryHub) Touch(context.Context, hubclient.TouchRequest) (hubclient.TouchResponse, error) {
	h.mu.Lock()
	h.touched++
	h.mu.Unlock()
	return hubclient.TouchResponse{Action: "allow"}, nil
}

func (h *recoveryHub) EndExecution(context.Context, string, int) error {
	h.ended++
	return nil
}

func writePayload(root, session, file string) string {
	return `{"session_id":` + quote(session) + `,"cwd":` + quote(root) + `,"hook_event_name":"PreToolUse","tool_name":"Write","tool_use_id":"t-` + file + `","tool_input":{"file_path":` + quote(filepath.Join(root, file)) + `}}`
}

func TestRegistrationWithSessionsLockTimeoutFailsOpen(t *testing.T) {
	root := gitRepo(t)
	env := failureEnv(t)
	m := sessionsFor(env)
	m.Remember("claude", "s", 3)
	unlock, err := m.lock(hookLockBudget)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()

	var calls int
	out := Runner{
		ClientName: "claude", EventName: "PreToolUse",
		Stdin: strings.NewReader(writePayload(root, "s", "a.go")), Getenv: env,
		Hub: countingHub{fn: func() { calls++ }}, after: never, logAfter: never,
	}.Run()
	want := FallbackAllow(ClientClaude)
	if out.Exit != want.Exit || string(out.Stdout) != string(want.Stdout) || string(out.Stderr) != string(want.Stderr) {
		t.Errorf("hook output %+v, want fallback allow %+v", out, want)
	}
	if calls != 0 {
		t.Errorf("the hook made %d hub calls while it could not take the lock", calls)
	}
	token := env("IRIS_TOKEN_FILE")
	notes, err := ReadRecentObservations(token, 10)
	if err != nil || len(notes) != 1 || notes[0].Error != "sessions_lock_timeout" {
		t.Errorf("observations = %+v, %v, want only sessions_lock_timeout", notes, err)
	}
	if failure := waitForFailure(t, token); failure.Stage != "session" || failure.Error != failSessionsLock.Error() {
		t.Errorf("failure = %+v, want a session lock timeout at the session stage", failure)
	}
}

func TestRegistrationRetryWithSessionsLockTimeoutRestoresPrunedProof(t *testing.T) {
	const generation = 3
	r := editRunner(t, ClientClaude, "")
	m := sessionsFor(r.Getenv)
	m.Remember("claude", "s1", generation)

	synctest.Test(t, func(t *testing.T) {
		locked := make(chan func(), 1)
		var sent []int
		r.Hub = interceptedRegistration{Client: r.Hub.(*hubclient.Client), register: func(_ context.Context, req hubclient.SessionRequest) (hubclient.SessionResponse, error) {
			sent = append(sent, req.Generation)
			if len(sent) == 1 {
				unlock, err := m.lock(hookLockBudget)
				if err != nil {
					t.Errorf("take the sessions lock after the first registration: %v", err)
					return hubclient.SessionResponse{}, err
				}
				locked <- unlock
			}
			return hubclient.SessionResponse{}, lostWithHighest(generation)
		}}
		done := make(chan Output, 1)
		go func() { done <- r.Run() }()
		var unlock func()
		select {
		case unlock = <-locked:
		case out := <-done:
			t.Fatalf("hook returned before the hub took the lock: %+v", out)
		}

		// Once the retry has read proof and is waiting for the lock, simulate pruning
		// under that lock. Release it after the read's budget, during retention's budget.
		synctest.Wait()
		pruneErr := os.Remove(m.name("claude", "s1"))
		time.Sleep(hookLockBudget + lockRetry/2)
		unlock()
		if pruneErr != nil {
			t.Fatal(pruneErr)
		}

		assertAllow(t, ClientClaude, <-done)
		if !slices.Equal(sent, []int{generation}) {
			t.Errorf("sent generations %v, want only the first request with G%d", sent, generation)
		}
		token := r.Getenv("IRIS_TOKEN_FILE")
		if notes, err := ReadRecentObservations(token, 10); err != nil || len(notes) != 1 || notes[0].Error != "sessions_lock_timeout" {
			t.Errorf("observations = %+v, %v, want only sessions_lock_timeout", notes, err)
		}
		if failure := waitForFailure(t, token); failure.Stage != "session" || failure.Error != failSessionsLock.Error() {
			t.Errorf("failure = %+v, want a session lock timeout at the session stage", failure)
		}
		if got := m.Recall("claude", "s1"); got != generation {
			t.Errorf("retained proof after pruning = G%d, want G%d", got, generation)
		}
	})
}

func runWith(h Hub, m Memory, event, payload string) Output {
	return Runner{ClientName: "claude", EventName: event, Stdin: strings.NewReader(payload), Hub: h, Getenv: func(string) string { return "" }, Memory: m, after: never}.Run()
}

func TestARememberedGenerationReattachesARecoveringSession(t *testing.T) {
	root := gitRepo(t)
	m := newFileMemory(t)
	h := &recoveryHub{generation: 2}
	runWith(h, m, "SessionStart", `{"session_id":"s","cwd":`+quote(root)+`}`)
	if m.Recall("claude", "s") != 2 {
		t.Fatal("session start did not remember its generation")
	}
	h.recovering = true
	out := runWith(h, m, "PreToolUse", writePayload(root, "s", "a.go"))
	if strings.Contains(string(out.Stdout), "deny") || h.touched != 1 {
		t.Fatalf("a session with proof was refused: %s", out.Stdout)
	}
	if h.sent[len(h.sent)-1] != 2 {
		t.Fatal("the hook did not send its proof", h.sent)
	}
}

func TestAnEditWithoutProofIsRefusedNotAllowed(t *testing.T) {
	root := gitRepo(t)
	h := &recoveryHub{generation: 1, recovering: true}
	out := runWith(h, newFileMemory(t), "PreToolUse", writePayload(root, "s", "a.go"))
	text := string(out.Stdout)
	if !strings.Contains(text, `"permissionDecision":"deny"`) || !strings.Contains(text, "hub restarted") {
		t.Fatalf("a recovering session's edit must be refused: %s", text)
	}
	if h.touched != 0 {
		t.Fatal("the refused edit reached touch")
	}
}

func TestAStaleGenerationBecomesTheRetryFloor(t *testing.T) {
	root := gitRepo(t)
	m := newFileMemory(t)
	m.Remember("claude", "s", 1)
	h := &recoveryHub{generation: 5}
	out := runWith(h, m, "PreToolUse", writePayload(root, "s", "a.go"))
	if strings.Contains(string(out.Stdout), "deny") || h.touched != 1 {
		t.Fatalf("%s", out.Stdout)
	}
	if len(h.sent) != 2 || h.sent[0] != 1 || h.sent[1] != 0 {
		t.Fatal("the hook must retry once using the stale generation as a floor", h.sent)
	}
	if len(h.retries) != 2 || h.retries[0] != 0 || h.retries[1] != 1 {
		t.Fatalf("the retry did not preserve its floor: %v", h.retries)
	}
	if m.Recall("claude", "s") != 5 {
		t.Fatal("the current generation was not remembered")
	}
}

func TestAStaleGenerationDuringRecoveryStillRefuses(t *testing.T) {
	root := gitRepo(t)
	m := newFileMemory(t)
	m.Remember("claude", "s", 1)
	h := &recoveryHub{generation: 5, recovering: true}
	out := runWith(h, m, "PreToolUse", writePayload(root, "s", "a.go"))
	if !strings.Contains(string(out.Stdout), `"permissionDecision":"deny"`) || h.touched != 0 || m.Recall("claude", "s") != 1 {
		t.Fatalf("%s", out.Stdout)
	}
}

func TestMalformedOrMissingFloorDetailsAndLowerRetryRepliesFailOpen(t *testing.T) {
	root := gitRepo(t)
	for _, tc := range []struct {
		name, detail, note string
		retry              bool
	}{
		{"old hub", "", RestoreFloorHubTooOld, false},
		{"missing field", `,"details":{}`, RestoreFloorHubTooOld, false},
		{"malformed field", `,"details":{"highest_generation":"5"}`, RestoreFloorRefused, false},
		{"unexpected lower success", `,"details":{"highest_generation":5}`, RestoreFloorRefused, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var sent []floorRequest
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req hubclient.SessionRequest
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
				}
				mu.Lock()
				sent = append(sent, floorRequest{route: r.URL.Path, generation: req.Generation, floor: req.RetryGeneration, endedThrough: req.EndedThrough})
				second := len(sent) == 2
				mu.Unlock()
				if second && tc.retry {
					_, _ = w.Write([]byte(`{"execution_id":"restored","generation":2}`))
					return
				}
				w.WriteHeader(http.StatusConflict)
				_, _ = fmt.Fprint(w, `{"error":{"code":"lost","message":"refused"`+tc.detail+`}}`)
			}))
			defer server.Close()
			getenv := failureEnv(t)
			token := getenv("IRIS_TOKEN_FILE")
			m := sessionsFor(getenv)
			m.Remember("claude", "s", 3)
			stale := time.Now().Add(-memoryAge - time.Hour)
			if err := os.Chtimes(m.name("claude", "s"), stale, stale); err != nil {
				t.Fatal(err)
			}
			client := &hubclient.Client{Config: hubURL(server.URL)}
			assertAllow(t, ClientClaude, run(client, "PreToolUse", writePayload(root, "s", "unguarded.go"), getenv))
			want := []floorRequest{{route: "/v1/sessions", generation: 3}}
			if tc.retry {
				want = append(want, floorRequest{route: "/v1/sessions", floor: 3})
			}
			mu.Lock()
			got := slices.Clone(sent)
			mu.Unlock()
			if !slices.Equal(got, want) {
				t.Fatalf("sent %+v, want %+v", got, want)
			}
			if err := m.prune(); err != nil {
				t.Fatal(err)
			}
			if got := m.Recall("claude", "s"); got != 3 {
				t.Fatalf("refused proof pruned or changed to G%d", got)
			}
			notes, err := ReadRecentObservations(token, 1)
			if err != nil || len(notes) != 1 || notes[0].Error != tc.note {
				t.Fatalf("observations %+v, %v", notes, err)
			}
			if failure := waitForFailure(t, token); failure.Stage != "session" || failure.Error != RestoreFloorRefused {
				t.Fatalf("missing fail-open record: %+v", failure)
			}
		})
	}
}

func TestSessionStartAndEndStillFailOpenAndEndForgets(t *testing.T) {
	root := gitRepo(t)
	m := newFileMemory(t)
	h := &recoveryHub{generation: 1, recovering: true}
	start := `{"session_id":"s","cwd":` + quote(root) + `}`
	if out := runWith(h, m, "SessionStart", start); strings.Contains(string(out.Stdout), "deny") {
		t.Fatal("session start is not a tool and must not refuse")
	}
	h.recovering = false
	runWith(h, m, "SessionStart", start)
	if m.Recall("claude", "s") != 1 {
		t.Fatal(m.Recall("claude", "s"))
	}
	runWith(h, m, "SessionEnd", start)
	if h.ended != 1 || m.Recall("claude", "s") != 0 {
		t.Fatal("session end must forget the generation", h.ended)
	}
}

func TestAnotherHubFailureStillFailsOpen(t *testing.T) {
	root := gitRepo(t)
	out := runWith(scriptHub{regErr: errors.New("down")}, newFileMemory(t), "PreToolUse", writePayload(root, "s", "a.go"))
	if strings.Contains(string(out.Stdout), "deny") {
		t.Fatalf("only a recovering session refuses; an outage keeps D3: %s", out.Stdout)
	}
}

// hookClock lets the test move the hub past its recovery deadline.
type hookClock struct {
	mu   sync.Mutex
	wall time.Time
	tick int64
}

func (c *hookClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.wall
}
func (c *hookClock) Tick() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.tick
}
func (c *hookClock) advance(d time.Duration) {
	c.mu.Lock()
	c.wall = c.wall.Add(d)
	c.tick += d.Milliseconds()
	c.mu.Unlock()
}

// The window from D9, against the real store and hub: a restart, then edits from a session
// that kept its proof, one that lost it, and one that never had a claim.
func TestTheRecoveryWindowNeverAllowsAnUnclaimedEdit(t *testing.T) {
	root := gitRepo(t)
	file := filepath.Join(t.TempDir(), "hub.db")
	clock := &hookClock{wall: time.Unix(1800000000, 0)}
	token := tokenFile(t)
	var url string
	start := func() func() {
		database, err := store.Open(file, clock)
		if err != nil {
			t.Fatal(err)
		}
		server := httptest.NewUnstartedServer(nil)
		server.Config.Handler = hub.New(database, "secret", []string{server.Listener.Addr().String()})
		server.Start()
		url = server.URL
		return func() { server.Close(); _ = database.Close() }
	}
	env := func(k string) string {
		switch k {
		case "IRIS_TOKEN_FILE":
			return token
		case "IRIS_HUB_URL":
			return url
		}
		return ""
	}
	hook := func(event, payload string) string {
		client := &hubclient.Client{Config: hubclient.EnvConfig{Getenv: env}}
		out := Runner{ClientName: "claude", EventName: event, Stdin: strings.NewReader(payload), Hub: client, Getenv: env, after: never, logAfter: never}.Run()
		return string(out.Stdout)
	}
	allowed := func(out string) bool { return !strings.Contains(out, `"permissionDecision"`) }
	stop := start()
	for _, s := range []string{"kept", "forgot"} {
		hook("SessionStart", `{"session_id":"`+s+`","cwd":`+quote(root)+`}`)
		if out := hook("PreToolUse", writePayload(root, s, s+".go")); !allowed(out) {
			t.Fatal(out)
		}
	}
	stop()
	stop = start()
	defer func() { stop() }()
	if err := os.Remove(sessionsFor(env).name("claude", "forgot")); err != nil {
		t.Fatal(err)
	}

	if out := hook("PreToolUse", writePayload(root, "forgot", "new.go")); !strings.Contains(out, "hub restarted") {
		t.Fatalf("an edit without proof was not refused: %s", out)
	}
	if out := hook("PreToolUse", writePayload(root, "kept", "next.go")); !allowed(out) {
		t.Fatalf("a session with proof was refused: %s", out)
	}
	hook("SessionStart", `{"session_id":"fresh","cwd":`+quote(root)+`}`)
	if out := hook("PreToolUse", writePayload(root, "fresh", "forgot.go")); !strings.Contains(out, "is held by") {
		t.Fatalf("a recovering session's claim must still block others: %s", out)
	}
	clock.advance(121 * time.Second)
	if out := hook("PreToolUse", writePayload(root, "forgot", "new.go")); !allowed(out) {
		t.Fatalf("after the deadline the session starts afresh: %s", out)
	}
	if out := hook("PreToolUse", writePayload(root, "fresh", "new.go")); !strings.Contains(out, "is held by") {
		t.Fatalf("the fresh generation's edit was not claimed: %s", out)
	}
}

// A file that cannot be protected is not kept, and neither is one in an unprotected directory.
func TestFileMemoryKeepsNothingItCannotProtect(t *testing.T) {
	refused := errors.New("acl")
	for name, failOn := range map[string]func(string, string) bool{
		"directory": func(path, dir string) bool { return path == dir },
		"file":      func(path, dir string) bool { return path != dir },
	} {
		m := newFileMemory(t)
		m.restrict = func(path string) error {
			if failOn(path, m.dir) {
				return refused
			}
			return nil
		}
		m.Remember("claude", "s", 3)
		if m.Recall("claude", "s") != 0 {
			t.Fatalf("%s: an unprotected generation was kept", name)
		}
		if entries, err := os.ReadDir(m.dir); err == nil {
			for _, entry := range entries {
				if entry.Name() != sessionsLock {
					t.Fatalf("%s: left %s", name, entry.Name())
				}
			}
		}
	}
}
