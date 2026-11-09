package main

import (
	"bytes"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mateusz-klatt/iris/internal/config"
	"github.com/mateusz-klatt/iris/internal/hub"
	"github.com/mateusz-klatt/iris/internal/store"
)

type observedConsoleConfiguration struct {
	key      []byte
	activate func() error
	err      error
}

func (c *observedConsoleConfiguration) ConfigureConsole(key []byte, activate func() error) error {
	c.key, c.activate = append([]byte(nil), key...), activate
	return c.err
}

func consoleOwnerCredential(t *testing.T) (string, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hub.token")
	token, err := config.Token(path)
	if err != nil {
		t.Fatal(err)
	}
	return path, token
}

func TestConfigureConsoleRequiresExplicitActivationAndRestart(t *testing.T) {
	ownerFile, ownerToken := consoleOwnerCredential(t)
	keyFile := filepath.Join(t.TempDir(), "new-parent", "console.key")
	var logs bytes.Buffer
	first := &observedConsoleConfiguration{}
	configureConsole(first, keyFile, ownerFile, ownerToken, &logs)
	if first.key != nil || first.activate == nil || !strings.Contains(logs.String(), "Iris console closed") {
		t.Fatal("missing key did not configure closed-console owner recovery")
	}
	if _, err := os.Stat(filepath.Dir(keyFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("startup created the activation key or its parent", err)
	}
	if err := first.activate(); err != nil {
		t.Fatal("explicit activation failed", err)
	}
	want, err := config.LoadConsoleKey(keyFile)
	if err != nil || len(want) != 32 || first.key != nil {
		t.Fatal("activation hot-loaded a key or failed to persist it", err)
	}
	logs.Reset()
	restarted := &observedConsoleConfiguration{}
	configureConsole(restarted, keyFile, ownerFile, ownerToken, &logs)
	if !bytes.Equal(restarted.key, want) || logs.String() != "Iris console open\n" {
		t.Fatal("restart did not load the persisted key")
	}
	if err = restarted.activate(); err != nil {
		t.Fatal("explicit key preparation did not preserve an existing key", err)
	}
	again, err := config.LoadConsoleKey(keyFile)
	if err != nil || !bytes.Equal(again, want) {
		t.Fatal("preparation rotated a valid existing key", err)
	}
}

func TestConfigureConsoleKeepsFailuresSafeAndRecoveryAvailable(t *testing.T) {
	ownerFile, ownerToken := consoleOwnerCredential(t)
	keyFile := filepath.Join(t.TempDir(), "console.key")
	if err := os.WriteFile(keyFile, []byte("private malformed contents"), 0600); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	closed := &observedConsoleConfiguration{err: errors.New("private database diagnostic")}
	configureConsole(closed, keyFile, ownerFile, ownerToken, &logs)
	if closed.key != nil || closed.activate == nil || !strings.Contains(logs.String(), "account authentication unavailable") || strings.Contains(logs.String(), "private") || strings.Contains(logs.String(), keyFile) {
		t.Fatal("setup failure exposed diagnostics or removed owner recovery")
	}
	if err := closed.activate(); err == nil || strings.Contains(err.Error(), "private") || strings.Contains(err.Error(), keyFile) {
		t.Fatal("activation repaired a malformed key or exposed its contents", err)
	}
	unchanged, err := os.ReadFile(keyFile)
	if err != nil || string(unchanged) != "private malformed contents" {
		t.Fatal("refused activation changed the malformed file", err)
	}
}

func TestConsoleKeyForHubRefusesOwnerAliasesAndCopiedCredentials(t *testing.T) {
	for _, alias := range []string{"same-path", "symlink", "hardlink", "copy"} {
		t.Run(alias, func(t *testing.T) {
			ownerFile, ownerToken := consoleOwnerCredential(t)
			before, err := os.ReadFile(ownerFile)
			if err != nil {
				t.Fatal(err)
			}
			keyFile := filepath.Join(filepath.Dir(ownerFile), "console.key")
			switch alias {
			case "same-path":
				keyFile = ownerFile
			case "symlink":
				if err = os.Symlink(ownerFile, keyFile); err != nil {
					t.Skip("symlink unavailable", err)
				}
			case "hardlink":
				if err = os.Link(ownerFile, keyFile); err != nil {
					t.Skip("hardlink unavailable", err)
				}
			case "copy":
				if err = os.WriteFile(keyFile, before, 0600); err != nil {
					t.Fatal(err)
				}
			}
			for _, create := range []bool{false, true} {
				key, err := consoleKeyForHub(keyFile, ownerFile, ownerToken, create)
				if key != nil || err == nil || err.Error() != "console key must be independent of owner credential" {
					t.Fatal("owner credential became a console key", err)
				}
			}
			after, err := os.ReadFile(ownerFile)
			if err != nil || !bytes.Equal(after, before) {
				t.Fatal("console setup changed the machine credential", err)
			}
			var logs bytes.Buffer
			observed := &observedConsoleConfiguration{}
			configureConsole(observed, keyFile, ownerFile, ownerToken, &logs)
			if observed.key != nil || strings.Contains(logs.String(), ownerToken) || strings.Contains(logs.String(), ownerFile) {
				t.Fatal("reused credential opened the console or reached logs")
			}
		})
	}
}

func TestConsoleKeyForHubRefusesUnavailablePathsWithoutExposingThem(t *testing.T) {
	ownerFile, ownerToken := consoleOwnerCredential(t)
	keyFile := filepath.Join(t.TempDir(), "console.key")
	for _, create := range []bool{false, true} {
		key, err := consoleKeyForHub(keyFile, ownerFile+".absent", ownerToken, create)
		if key != nil || err == nil || err.Error() != "owner credential file unavailable" {
			t.Fatal("missing owner file allowed console key preparation", err)
		}
		key, err = consoleKeyForHub(keyFile+"\x00", ownerFile, ownerToken, create)
		if key != nil || err == nil || err.Error() != "console key file unavailable" {
			t.Fatal("invalid console path was accepted or exposed", err)
		}
		key, err = consoleKeyForHub(t.TempDir(), ownerFile, ownerToken, create)
		if key != nil || err == nil || err.Error() != "console key file unavailable" {
			t.Fatal("directory was accepted as a console key", err)
		}
	}
	if _, err := os.Stat(keyFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("failed console preparation created state", err)
	}
}

func TestClosedConsoleStartupPreservesAgentCoordination(t *testing.T) {
	for _, state := range []string{"missing", "malformed", "reused"} {
		t.Run(state, func(t *testing.T) {
			ownerFile, ownerToken := consoleOwnerCredential(t)
			keyFile := filepath.Join(filepath.Dir(ownerFile), "console.key")
			s, err := store.Open(filepath.Join(t.TempDir(), "hub.db"), nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close() })
			if state == "malformed" {
				if err = os.WriteFile(keyFile, []byte("private malformed key"), 0600); err != nil {
					t.Fatal(err)
				}
			} else if state == "reused" {
				keyFile = ownerFile
			}
			h := hub.New(s, ownerToken, []string{"127.0.0.1:7878"})
			configureConsole(h, keyFile, ownerFile, ownerToken, io.Discard)
			login := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7878/v1/ui/login", nil)
			login.RemoteAddr = "127.0.0.1:1234"
			response := httptest.NewRecorder()
			h.ServeHTTP(response, login)
			if response.Code != http.StatusUnauthorized || len(response.Result().Cookies()) != 0 {
				t.Fatal("failed startup left browser authentication open")
			}
			registration := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7878/v1/sessions", strings.NewReader(`{"client":"codex","host":"temporary","session_id":"temporary","cwd":"/temporary","worktree":"/temporary","source":"hook"}`))
			registration.Header.Set("Authorization", "Bearer "+ownerToken)
			response = httptest.NewRecorder()
			h.ServeHTTP(response, registration)
			if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "execution_id") {
				t.Fatal("console-key failure stopped agent coordination", response.Code)
			}
		})
	}
}

func TestServeConsoleKeyPathFollowsDatabaseOrExplicitFlag(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run(map[bool]string{false: "database-default", true: "explicit"}[explicit], func(t *testing.T) {
			dir := t.TempDir()
			dbFile := filepath.Join(dir, "custom.db")
			keyFile := dbFile + ".console.key"
			if explicit {
				keyFile = filepath.Join(dir, "different.key")
			}
			if _, err := config.ConsoleKey(keyFile); err != nil {
				t.Fatal(err)
			}
			taken, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = taken.Close() }()
			args := []string{"serve", "--listen", taken.Addr().String(), "--db", dbFile, "--token-file", filepath.Join(dir, "hub.token")}
			if explicit {
				args = append(args, "--console-key-file", keyFile)
			}
			var serveErr error
			_, logs := capture(t, func() { serveErr = run(args) })
			// The key exists but there is no operator. Reaching the account setup
			// refusal proves startup loaded the selected path without activating it.
			if serveErr == nil || !strings.Contains(logs, "account authentication unavailable") || strings.Contains(logs, "console key unavailable") || strings.Contains(logs, keyFile) {
				t.Fatal("serve ignored the selected key path or disclosed it", serveErr, logs)
			}
		})
	}
}
