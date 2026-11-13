package wake

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/binding"
	"github.com/mateusz-klatt/iris/internal/hook"
)

// While an outstanding Antigravity wake's transcript has more than one scan pass left to read, the
// file poll comes back after at most 50 ms instead of a whole file poll, so a long transcript is
// caught up quickly (#318). One call, with no runner loop and no sleeps: the coverage of this
// branch no longer depends on how a live helper's passes race the transcript.
func TestAnAntigravityFilePollComesBackSoonWhileTheScanHasMore(t *testing.T) {
	const convID = "e28a6164-6409-4810-b8ee-df8202a16a0a"
	now := time.UnixMilli(1800000000000)
	target := binding.WakeTarget{SessionID: convID, Client: "antigravity", Host: "host", Bound: binding.Bound{ExecutionID: "ex", Generation: 1, Rung: 2}}
	sessions := hook.NewSessions(filepath.Join(t.TempDir(), "token"))
	store := newClientStateStore(sessions, "antigravity", convID)

	// A transcript longer than one scan pass, without the wake's message.
	transcript := filepath.Join(t.TempDir(), "transcript.jsonl")
	line := `{"step_index":1,"source":"MODEL","type":"GENERIC","content":"` + strings.Repeat("P", 1<<20) + "\"}\n"
	if err := os.WriteFile(transcript, []byte(strings.Repeat(line, 5)), 0o600); err != nil {
		t.Fatal(err)
	}
	a := attempt{ID: "att-live", Kind: "queue", Stream: "stream", Cursor: 7, StartedMS: now.UnixMilli(), Token: "tok", Message: "the wake prompt", Answer: "submitted", AnsweredMS: now.UnixMilli(), Scan: &hook.AntigravityScanCursor{TranscriptPath: transcript}}
	writeState(t, store, testDocument(a))

	r := &Runner{resolver: &engineResolver{target: target, eligible: true}, sessions: sessions, now: func() time.Time { return now }, settle: 10 * time.Second, filePoll: 15 * time.Second}
	if got := r.pollTargetFile(context.Background(), target, store); !got.fileWait || got.due != now.Add(50*time.Millisecond) {
		t.Fatalf("with more to scan: %+v, want a file wait due in 50 ms", got)
	}
	// A file poll shorter than 50 ms is kept.
	r.filePoll = 20 * time.Millisecond
	if got := r.pollTargetFile(context.Background(), target, store); !got.fileWait || got.due != now.Add(20*time.Millisecond) {
		t.Fatalf("with a 20 ms file poll: %+v", got)
	}
	// Once the scan has read the whole transcript, the ordinary file poll decides again.
	r.filePoll = 15 * time.Second
	for range 4 {
		r.pollTargetFile(context.Background(), target, store)
	}
	if got := r.pollTargetFile(context.Background(), target, store); !got.fileWait || got.due != now.Add(15*time.Second) {
		t.Fatalf("with the transcript read: %+v, want the ordinary file poll", got)
	}
}
