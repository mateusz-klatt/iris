package doctor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mateusz-klatt/iris/internal/gitinfo"
	"github.com/mateusz-klatt/iris/internal/hook"
	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/proc"
)

func TestReadEndMarkerGenerationIsBoundedAndMatchesAbsence(t *testing.T) {
	dir := t.TempDir()
	valid := filepath.Join(dir, "valid.ended")
	if err := os.WriteFile(valid, []byte("4\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if generation, err := readEndMarkerGeneration(valid); generation != 4 || err != nil {
		t.Fatalf("valid marker = G%d, %v", generation, err)
	}
	if generation, err := readEndMarkerGeneration(filepath.Join(dir, "absent.ended")); generation != 0 || err != nil {
		t.Fatalf("absent marker = G%d, %v", generation, err)
	}
	belowLimit := filepath.Join(dir, "below-limit.ended")
	body := strings.Repeat(" ", endMarkerReadLimit-3) + "4\n"
	if err := os.WriteFile(belowLimit, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if generation, err := readEndMarkerGeneration(belowLimit); generation != 4 || err != nil {
		t.Fatalf("below-limit marker = G%d, %v", generation, err)
	}
	atLimit := filepath.Join(dir, "at-limit.ended")
	if err := os.WriteFile(atLimit, []byte(" "+body), 0o600); err != nil {
		t.Fatal(err)
	}
	if generation, err := readEndMarkerGeneration(atLimit); generation != 0 || !errors.Is(err, hook.ErrInvalidGeneration) {
		t.Fatalf("full-bound marker = G%d, %v", generation, err)
	}
	malformed := filepath.Join(dir, "malformed.ended")
	if err := os.WriteFile(malformed, []byte("0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if generation, err := readEndMarkerGeneration(malformed); generation != 0 || !errors.Is(err, hook.ErrInvalidGeneration) {
		t.Fatalf("malformed marker = G%d, %v", generation, err)
	}

	t.Run("dangling marker is not absent", func(t *testing.T) {
		originalOpen, originalLstat := endMarkerOpenFile, endMarkerLstat
		t.Cleanup(func() { endMarkerOpenFile, endMarkerLstat = originalOpen, originalLstat })
		endMarkerOpenFile = func(string) (*os.File, error) { return nil, os.ErrNotExist }
		endMarkerLstat = func(string) (os.FileInfo, error) { return fakeMarkerInfo{}, nil }
		if generation, err := readEndMarkerGeneration("dangling.ended"); generation != 0 || !errors.Is(err, hook.ErrInvalidGeneration) {
			t.Fatalf("dangling marker = G%d, %v", generation, err)
		}
	})
	t.Run("open failure", func(t *testing.T) {
		original := endMarkerOpenFile
		t.Cleanup(func() { endMarkerOpenFile = original })
		endMarkerOpenFile = func(string) (*os.File, error) { return nil, errors.New("open failure") }
		if generation, err := readEndMarkerGeneration(valid); generation != 0 || !errors.Is(err, hook.ErrInvalidGeneration) {
			t.Fatalf("open failure = G%d, %v", generation, err)
		}
	})
	t.Run("read failure", func(t *testing.T) {
		original := endMarkerOpenFile
		t.Cleanup(func() { endMarkerOpenFile = original })
		endMarkerOpenFile = func(string) (*os.File, error) {
			f, err := os.Open(valid)
			if err != nil {
				return nil, err
			}
			return f, f.Close()
		}
		if generation, err := readEndMarkerGeneration(valid); generation != 0 || !errors.Is(err, hook.ErrInvalidGeneration) {
			t.Fatalf("read failure = G%d, %v", generation, err)
		}
	})
}

type markerReplacementHub struct {
	registration    hubclient.SessionRequest
	endedID         string
	endedGeneration int
}

func (h *markerReplacementHub) RegisterSession(_ context.Context, req hubclient.SessionRequest) (hubclient.SessionResponse, error) {
	h.registration = req
	return hubclient.SessionResponse{ExecutionID: "ex_marker", Generation: req.Generation}, nil
}

func (h *markerReplacementHub) Touch(context.Context, hubclient.TouchRequest) (hubclient.TouchResponse, error) {
	return hubclient.TouchResponse{}, errors.New("unexpected touch during SessionEnd")
}

func (h *markerReplacementHub) EndExecution(_ context.Context, id string, generation int) error {
	h.endedID, h.endedGeneration = id, generation
	return nil
}

func TestDoctorMarkerReadAllowsHookSessionEndReplacement(t *testing.T) {
	root := t.TempDir()
	token := filepath.Join(root, "hub.token")
	if err := os.WriteFile(token, []byte("test-only-credential\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sessions := hook.NewSessions(token)
	if err := sessions.Protect(); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("claude\x00session"))
	proof := filepath.Join(sessions.Dir(), hex.EncodeToString(sum[:16]))
	marker := proof + ".ended"
	if err := os.WriteFile(proof, []byte("2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, []byte("1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(map[string]string{"session_id": "session", "cwd": root})
	if err != nil {
		t.Fatal(err)
	}
	getenv := func(key string) string {
		if key == "IRIS_TOKEN_FILE" {
			return token
		}
		return ""
	}
	hub := &markerReplacementHub{}
	runner := hook.Runner{
		ClientName: "claude", EventName: "SessionEnd", Stdin: bytes.NewReader(payload), Getenv: getenv,
		Find: func(string) (gitinfo.Repo, error) { return gitinfo.Repo{Worktree: root}, nil },
		Host: func() string { return "host" }, Ancestor: func() (proc.Ancestor, error) { return proc.Ancestor{}, nil },
		Hub: hub,
	}

	originalOpen := endMarkerOpenFile
	t.Cleanup(func() { endMarkerOpenFile = originalOpen })
	var out hook.Output
	endMarkerOpenFile = func(path string) (*os.File, error) {
		f, err := originalOpen(path)
		if path == marker && err == nil {
			out = runner.Run()
		}
		return f, err
	}
	if _, err := readEndMarkerGeneration(marker); err != nil {
		t.Fatalf("doctor read after replacement: %v", err)
	}
	if out.Exit != 0 || !json.Valid(out.Stdout) || hub.registration.Generation != 2 || hub.endedID != "ex_marker" || hub.endedGeneration != 2 {
		t.Fatalf("SessionEnd output = %+v, registration = %+v, ended = %q G%d", out, hub.registration, hub.endedID, hub.endedGeneration)
	}
	if generation, err := hook.ReadGeneration(marker); generation != 2 || err != nil {
		t.Fatalf("replacement while doctor held the marker open = G%d, %v; want G2", generation, err)
	}
	if _, err := os.Stat(proof); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("SessionEnd left live proof behind: %v", err)
	}
	observed, err := os.ReadFile(filepath.Join(root, "hook-observed.log"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if strings.Contains(string(observed), hook.DamagedGenerationProof) {
		t.Fatalf("SessionEnd recorded damaged proof after replacement: %s", observed)
	}
}
