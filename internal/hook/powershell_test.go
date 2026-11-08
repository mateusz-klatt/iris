package hook

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/hub"
	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/proc"
	"github.com/mateusz-klatt/iris/internal/store"
)

// Claude Code on Windows writes files through its PowerShell tool as freely as through Bash. A
// real capture of Set-Content on a path another agent holds must take the shell path: recorded
// before, compared after, and reported, never passed as an unknown tool that touches nothing.
func TestCapturedPowerShellWriteIsReconciledLikeBash(t *testing.T) {
	root := t.TempDir()
	for _, args := range [][]string{{"init", "--initial-branch=main", root}, {"-C", root, "remote", "add", "origin", "https://example.com/team/ps.git"}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	database, err := store.Open(filepath.Join(t.TempDir(), "hub.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	server := httptest.NewUnstartedServer(nil)
	server.Config.Handler = hub.New(database, "secret", []string{server.Listener.Addr().String()})
	server.Start()
	defer server.Close()
	token := tokenFile(t)
	getenv := func(key string) string {
		switch key {
		case "IRIS_TOKEN_FILE":
			return token
		case "IRIS_HUB_URL":
			return server.URL
		}
		return ""
	}
	client := &hubclient.Client{Config: hubclient.EnvConfig{Getenv: getenv}}
	ctx := context.Background()
	// The holder works in another worktree of the same repository, as in TestShellStatusAgainstRealHub.
	holder, err := client.RegisterSession(ctx, hubclient.SessionRequest{Client: "codex", SessionID: "holder", Host: "testhost", CWD: "/other", Worktree: "/other", RemoteURL: "https://example.com/team/ps.git", Source: "hook"})
	if err != nil {
		t.Fatal(err)
	}
	if err = client.Do(ctx, "POST", "/v1/claims", map[string]any{"execution_id": holder.ExecutionID, "generation": holder.Generation, "key": "hold", "kind": "exact", "path": "note.txt"}, nil); err != nil {
		t.Fatal(err)
	}

	run := func(event, fixture string) Output {
		t.Helper()
		raw, err := os.ReadFile(filepath.Join("testdata", "claude", fixture))
		if err != nil {
			t.Fatal(err)
		}
		captured, err := Decode(ClientClaude, event, raw)
		if err != nil {
			t.Fatal(err)
		}
		if captured.ToolClass != ClassShell || !shellEvent(captured) {
			t.Fatalf("%s: class %q, shell %v", fixture, captured.ToolClass, shellEvent(captured))
		}
		var payload any
		if err = json.Unmarshal(raw, &payload); err != nil {
			t.Fatal(err)
		}
		raw, err = json.Marshal(rewriteCapturedPaths(payload, captured.CWD, filepath.ToSlash(root)))
		if err != nil {
			t.Fatal(err)
		}
		// This checks the captured tool's reconciliation, not the speed of git or the
		// test hub. after: never disables the outer timer, but not the shell's own
		// context deadline. Budget tests separately assert the production fail-open limit.
		out := Runner{ClientName: string(ClientClaude), EventName: event, Stdin: bytes.NewReader(raw), Getenv: getenv, Hub: client, Host: func() string { return "testhost" }, Ancestor: func() (proc.Ancestor, error) { return proc.Ancestor{}, nil }, ShellDeadline: time.Hour, after: never, logAfter: never}.Run()
		failures, err := ReadRecentFailures(token, 10)
		if err != nil || len(failures) != 0 {
			t.Fatalf("%s hook failed: failures=%+v log error=%v stdout=%s stderr=%s", event, failures, err, out.Stdout, out.Stderr)
		}
		if out.Exit != 0 {
			t.Fatalf("%s exited %d: stdout=%s stderr=%s", event, out.Exit, out.Stdout, out.Stderr)
		}
		return out
	}

	before := run("PreToolUse", "PreToolUse-powershell.json")
	if bytes.Contains(before.Stdout, []byte("Shell edit detected")) {
		t.Fatalf("a warning before anything changed: %s", before.Stdout)
	}
	// What the captured Set-Content -Path note.txt -Value hi did.
	if err = os.WriteFile(filepath.Join(root, "note.txt"), []byte("hi\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	after := run("PostToolUse", "PostToolUse-powershell.json")
	if !bytes.Contains(after.Stdout, []byte("Shell edit detected")) || !bytes.Contains(after.Stdout, []byte("note.txt")) {
		t.Fatalf("the PowerShell write was not reported: stdout=%s stderr=%s", after.Stdout, after.Stderr)
	}
}
