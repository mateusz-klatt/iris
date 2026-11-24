//go:build windows

package hook

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/mateusz-klatt/iris/internal/proc"
)

func TestAntigravityWitnessSurvivesHeldHelperRecordReader(t *testing.T) {
	configDir, tokenPath, getenv := agyTestEnv(t)
	const pid, startMS = 8803, int64(88003)
	ancestor := proc.Ancestor{PID: pid, StartMS: startMS, Name: "agy"}
	sessions := NewSessions(tokenPath)
	if err := sessions.memory.rememberProcessSession("antigravity", "old-session", ancestor); err != nil {
		t.Fatal(err)
	}
	m := antigravityHelpersMemory(configDir)
	if err := m.directory(); err != nil {
		t.Fatal(err)
	}
	target := antigravityProcFile(m.dir, pid, startMS)
	if err := writeAntigravityJSON(m.dir, target, antigravityProcState{CurrentSessionID: "old-session"}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	proof := sessions.memory.name("antigravity", "new-session")
	if err := sessions.memory.write(proof, "1"); err != nil {
		t.Fatal(err)
	}
	if err := sessions.memory.write(proof+".execution", "1 ex_one"); err != nil {
		t.Fatal(err)
	}
	held, err := os.Open(target)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Close() }()

	out := (Runner{
		ClientName: "antigravity", EventName: "SessionStart",
		Stdin: agyJSON(t, map[string]any{"conversationId": "new-session"}),
		Hub:   scriptHub{}, Getenv: getenv, Host: func() string { return "testhost" },
		Ancestor: func() (proc.Ancestor, error) { return ancestor, nil },
		after:    never, logAfter: never,
	}).Run()
	if out.Exit != 0 || string(out.Stdout) != "{}\n" {
		t.Fatalf("held helper reader blocked SessionStart: %+v", out)
	}
	after, err := os.ReadFile(target)
	if err != nil || !bytes.Equal(after, before) {
		t.Fatalf("held helper record changed from %q to %q: %v", before, after, err)
	}
	if current, ok, err := sessions.CurrentSession("antigravity", ancestor); current != "new-session" || !ok || err != nil {
		t.Fatalf("separate witness was not published: current=%q present=%t err=%v", current, ok, err)
	}
	if generation, execution, err := sessions.PrepareSuccession("antigravity", "new-session", 1, "ex_one", ancestor); generation != 1 || execution != "ex_one" || err != nil {
		t.Fatalf("separate evidence did not authorize the new session: generation=%d execution=%q err=%v", generation, execution, err)
	}
	if _, _, err := sessions.PrepareSuccession("antigravity", "old-session", 1, "ex_one", ancestor); err == nil {
		t.Fatal("old helper record kept the old session eligible")
	}
	if entries, err := ReadRecentFailures(tokenPath, 10); err != nil || len(entries) == 0 {
		t.Fatalf("blocked helper replacement was not recorded: %+v %v", entries, err)
	}
	if _, err := os.Stat(filepath.Join(configDir, "sessions", "proc-8803-88003")); err != nil {
		t.Fatalf("separate process witness is absent: %v", err)
	}
}
