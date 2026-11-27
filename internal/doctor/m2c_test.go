package doctor

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/hook"
	"github.com/mateusz-klatt/iris/internal/integrate"
)

func testHashPrefix(dir, client, session string) string {
	sum := sha256.Sum256([]byte(client + "\n" + session))
	return filepath.Join(dir, "sessions", hex.EncodeToString(sum[:]))
}

func appendDoctorLogLines(t *testing.T, path string, entries ...hook.Failure) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	for _, e := range entries {
		raw, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write(append(raw, '\n')); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDoctorRow2SilentAndSessionEvidenceWriteFailedNonSilent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/health":
			_, _ = w.Write([]byte(`{"version":"v0.2.0","stream":"main","counts":{}}`))
		case "/v1/overview":
			_, _ = w.Write([]byte(`{"agents":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)

	t.Run("row 2 damaged exact record after confirmed end grades silent with recovery guidance", func(t *testing.T) {
		dir := t.TempDir()
		token := filepath.Join(dir, "token")
		if err := os.WriteFile(token, []byte("secret\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		execPath := testHashPrefix(dir, "claude", "s1") + ".execution"
		appendDoctorLogLines(t, filepath.Join(dir, "hook-observed.log"),
			hook.Failure{Time: now.Add(-time.Minute), Client: integrate.Claude, Event: "PreToolUse", Stage: "observed", Error: hook.DamagedGenerationProof + " at " + execPath},
		)
		appendDoctorLogLines(t, filepath.Join(dir, "hook-failures.log"),
			hook.Failure{Time: now.Add(-time.Minute), Client: integrate.Claude, Event: "PreToolUse", Stage: "session", Error: "damaged session proof after confirmed end"},
		)
		res := Result{tokenFile: token, Report: Report{Clients: []ClientReport{{Client: integrate.Claude, Status: StatusOK}}}}
		if err := applyFailures(&res, token, now); err != nil {
			t.Fatal(err)
		}
		row := res.Report.Clients[0]
		if row.Status != StatusSilent {
			t.Fatalf("status=%q want=%q (detail=%q)", row.Status, StatusSilent, row.Detail)
		}
		if !strings.Contains(row.Detail, "start a new client session to obtain fresh proof") {
			t.Fatalf("missing recovery guidance in detail: %q", row.Detail)
		}
		report := res.Format()
		if !strings.Contains(report, hook.DamagedGenerationProof+" at "+execPath) {
			t.Fatalf("missing .execution proof path in doctor output:\n%s", report)
		}
	})

	t.Run("session evidence write failed displays non-silent and counts in bundle", func(t *testing.T) {
		dir := t.TempDir()
		token := filepath.Join(dir, "token")
		if err := os.WriteFile(token, []byte("secret\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		execPath := testHashPrefix(dir, "codex", "s2") + ".execution"
		endedPath := testHashPrefix(dir, "codex", "s2") + ".ended"
		appendDoctorLogLines(t, filepath.Join(dir, "hook-observed.log"),
			hook.Failure{Time: now.Add(-time.Minute), Client: integrate.Codex, Event: "PreToolUse", Stage: "observed", Error: hook.SessionEvidenceWriteFailed + " at " + execPath},
			hook.Failure{Time: now.Add(-time.Minute), Client: integrate.Codex, Event: "SessionEnd", Stage: "observed", Error: hook.SessionEvidenceWriteFailed + " at " + endedPath},
		)
		res := Result{tokenFile: token, Report: Report{Clients: []ClientReport{{Client: integrate.Codex, Status: StatusOK}}}}
		if err := applyFailures(&res, token, now); err != nil {
			t.Fatal(err)
		}
		row := res.Report.Clients[0]
		if row.Status == StatusSilent {
			t.Fatalf("SessionEvidenceWriteFailed must not grade silent: %+v", row)
		}
		report := res.Format()
		if !strings.Contains(report, hook.SessionEvidenceWriteFailed+" at "+execPath) ||
			!strings.Contains(report, hook.SessionEvidenceWriteFailed+" at "+endedPath) {
			t.Fatalf("missing evidence write failed paths in report:\n%s", report)
		}
		bundle := NewBundle(res, "v0.2.0", now)
		if len(bundle.Observations) != 1 || bundle.Observations[0].Category != hook.SessionEvidenceWriteFailed || bundle.Observations[0].Count != 2 {
			t.Fatalf("unexpected bundle observations: %+v", bundle.Observations)
		}
	})
}
