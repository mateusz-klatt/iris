//go:build windows

package hook

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mateusz-klatt/iris/internal/gitinfo"
	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/proc"
)

type sessionEndReplaceHub struct {
	registration    hubclient.SessionRequest
	endedID         string
	endedGeneration int
}

func (h *sessionEndReplaceHub) RegisterSession(_ context.Context, req hubclient.SessionRequest) (hubclient.SessionResponse, error) {
	h.registration = req
	return hubclient.SessionResponse{ExecutionID: "ex_replace", Generation: req.Generation}, nil
}

func (h *sessionEndReplaceHub) Touch(context.Context, hubclient.TouchRequest) (hubclient.TouchResponse, error) {
	return hubclient.TouchResponse{}, errors.New("unexpected touch during SessionEnd")
}

func (h *sessionEndReplaceHub) EndExecution(_ context.Context, id string, generation int) error {
	h.endedID, h.endedGeneration = id, generation
	return nil
}

func TestSessionEndReplacesMarkerWithReaderOpen(t *testing.T) {
	m := newFileMemory(t)
	m.Remember("claude", "s", 2)
	marker := m.name("claude", "s") + ".ended"
	if err := m.replace(marker, "1"); err != nil {
		t.Fatal(err)
	}
	held, err := openAntigravityStateFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Close() }()

	root := filepath.Dir(m.dir)
	token := filepath.Join(root, "hub.token")
	if err := os.WriteFile(token, []byte("test-only-credential\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	getenv := func(key string) string {
		if key == "IRIS_TOKEN_FILE" {
			return token
		}
		return ""
	}
	hub := &sessionEndReplaceHub{}
	payload := `{"session_id":"s","cwd":` + quote(root) + `}`
	out := (Runner{
		ClientName: "claude", EventName: "SessionEnd", Stdin: strings.NewReader(payload), Getenv: getenv,
		Find: func(string) (gitinfo.Repo, error) { return gitinfo.Repo{Worktree: root}, nil },
		Host: func() string { return "host" }, Ancestor: func() (proc.Ancestor, error) { return proc.Ancestor{}, nil },
		Hub: hub, Memory: m, after: never, logAfter: never,
	}).Run()
	assertAllow(t, ClientClaude, out)
	if hub.registration.Generation != 2 || hub.endedID != "ex_replace" || hub.endedGeneration != 2 {
		t.Fatalf("SessionEnd registration = %+v, ended = %q G%d", hub.registration, hub.endedID, hub.endedGeneration)
	}
	if generation, err := readGeneration(marker); generation != 2 || err != nil {
		t.Fatalf("replacement with reader open = G%d, %v; want G2", generation, err)
	}
	old, err := io.ReadAll(held)
	if err != nil || string(old) != "1\n" {
		t.Fatalf("open reader lost the old marker: %q, %v", old, err)
	}
	observed, err := os.ReadFile(filepath.Join(root, "hook-observed.log"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if strings.Contains(string(observed), DamagedGenerationProof) {
		t.Fatalf("SessionEnd recorded damaged proof after a successful replacement: %s", observed)
	}
}
